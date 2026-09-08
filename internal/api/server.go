// Package api 提供 HTTP 接口（gin）：工单提交/查询/流水，异步触发编排处理。
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"aiops/pkg/cache"

	"github.com/prometheus/client_golang/prometheus"

	"aiops/internal/orchestrator"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/web"

	"github.com/gin-gonic/gin"
)

// ToolExecutor 工具执行入口（处置动作用，与 agent.ToolExecutor 同形）。
type ToolExecutor interface {
	Execute(ctx context.Context, ticketID, toolName string, input json.RawMessage) (*tool.Result, error)
}

// AuditReader 审计查询（PG 实现提供；内存模式为 nil）。
type AuditReader interface {
	ListByTicket(ctx context.Context, ticketID string) ([]*tool.AuditEntry, error)
}

// Server HTTP 服务。
type Server struct {
	store      ticket.Store
	orch       *orchestrator.Orchestrator
	tools      ToolExecutor        // 处置动作执行
	obs        *ObsDeps            // 观测查询（可 nil）
	audit      AuditReader         // 审计查询（可 nil）
	prompts    PromptAdmin         // prompt 热更新（可 nil）
	caseSearch CaseSearch          // 案例检索（可 nil）
	caseDel    CaseDeleter         // 案例删除（可 nil）
	authSvc    AuthBackend         // 鉴权后端（可 nil = 关闭）
	queue      TaskSubmitter       // 异步任务队列（可 nil = goroutine 兜底）
	dead       DeadLister          // 死信查询（可 nil）
	verifier   Verifier            // 恢复复查调度（可 nil）
	cache      *cache.Cache        // 告警去重等（可 nil）
	metricsG   prometheus.Gatherer // 指标采集器（ai_usage 用，可 nil）
	providers  ProviderAdmin       // provider 链热插拔（可 nil）
	jbase      string              // Jaeger 查询页基地址（trace 链接）
}

// AuthBackend 鉴权能力（auth.Service 实现）。
type AuthBackend interface {
	Login(ctx context.Context, username, password string) (string, error)
	Middleware() gin.HandlerFunc
}

// TaskSubmitter 异步任务提交（async.Queue 实现；nil = goroutine 兜底）。
type TaskSubmitter interface {
	Enqueue(ctx context.Context, ticketID, kind, idempotencyKey string, payload json.RawMessage) error
}

// WithQueue 注入异步任务队列。
func (s *Server) WithQueue(q TaskSubmitter) *Server { s.queue = q; return s }

// Verifier 恢复复查调度（orchestrator 实现）。
type Verifier interface {
	ScheduleVerify(ctx context.Context, ticketID string)
}

// WithVerifier 注入复查调度。
func (s *Server) WithVerifier(v Verifier) *Server { s.verifier = v; return s }

// DeadLister 死信查询（tasks.Queue 实现；返回预序列化 JSON）。
type DeadLister interface {
	ListDead(ctx context.Context, limit int) (json.RawMessage, error)
}

// WithCache 注入缓存（告警去重）。
func (s *Server) WithCache(c *cache.Cache) *Server { s.cache = c; return s }

// WithMetricsGatherer 注入指标采集器（ai_usage 端点用）。
func (s *Server) WithMetricsGatherer(g prometheus.Gatherer) *Server { s.metricsG = g; return s }

// WithCaseDeleter 注入案例删除（PG/Milvus 双后端）。
func (s *Server) WithCaseDeleter(d CaseDeleter) *Server { s.caseDel = d; return s }

// WithDeadLister 注入死信查询（nil = 队列未装配）。
func (s *Server) WithDeadLister(d DeadLister) *Server { s.dead = d; return s }

// NewServer 构造。
func NewServer(store ticket.Store, orch *orchestrator.Orchestrator, jaegerBaseURL string) *Server {
	gin.SetMode(gin.ReleaseMode)
	return &Server{store: store, orch: orch, jbase: jaegerBaseURL}
}

// WithTools 注入工具执行器与审计查询（M3 处置闭环）。
func (s *Server) WithTools(t ToolExecutor, a AuditReader) *Server {
	s.tools = t
	s.audit = a
	return s
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery(), requestLog())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// 前端 SPA（Vite 构建产物内嵌）：/ 走 index.html，静态资源直出，其余路径 SPA 回退。
	distFS, err := web.Dist()
	if err == nil {
		indexHTML, _ := fs.ReadFile(distFS, "index.html")
		fileFS := http.FS(distFS)
		r.NoRoute(func(c *gin.Context) {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/api/") {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			if path == "/" || !strings.Contains(path[strings.LastIndexByte(path, '/')+1:], ".") {
				c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
				return
			}
			if f, err := fileFS.Open(path); err == nil {
				_ = f.Close()
				c.FileFromFS(path, fileFS)
				return
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
		})
	}

	v1 := r.Group("/api/v1")
	{
		v1.POST("/tickets", s.createTicket)
		v1.GET("/tickets", s.listTickets)
		v1.GET("/tickets/:id", s.getTicket)
		v1.GET("/tickets/:id/messages", s.listMessages)
		v1.GET("/tickets/:id/observations", s.listObservations)
		v1.DELETE("/tickets/:id", s.deleteTicket) // 删除工单（保护组：鉴权开启时需 token）
		v1.GET("/tickets/:id/observations/:oid", s.getObservation)
		v1.POST("/tickets/:id/reply", s.replyTicket)  // HITL：补充信息，断点续跑
		v1.PATCH("/tickets/:id/rating", s.rateTicket) // 满意度评价
		v1.GET("/tickets/:id/stream", s.streamTicket) // SSE 推流
		v1.GET("/tickets/:id/audit", s.listAudit)     // 审计记录
		admin := v1.Group("/admin")
		{
			admin.GET("/prompts", s.listPrompts)
		}
		v1.GET("/cases/search", s.searchCases)        // M5 相似案例检索
		v1.GET("/cases", s.listCases)                 // P5 案例列表（知识库页）
		v1.GET("/cases/:id", s.caseDetail)            // P5 案例详情（关联工单报告）
		v1.GET("/tasks/dead", s.listDeadTasks)        // C5 死信任务（管理面排查）
		v1.GET("/tickets/:id/replay", s.ticketReplay) // P3 推理回放事件流
		v1.GET("/tickets/stats", s.stats)             // P5 工单统计
		v1.POST("/alerts/webhook", s.alertWebhook)    // P6 告警接入

		k8sGrp := v1.Group("/k8s")
		{
			k8sGrp.GET("/deployments", s.listK8sDeployments)
			k8sGrp.GET("/events", s.listK8sEvents)
			k8sGrp.GET("/logs", s.queryK8sLogs)
		}
		v1.GET("/jaeger/services", s.jaegerServices)
		v1.GET("/jaeger/traces", s.jaegerTraces)
		obs := v1.Group("/observability")
		{
			obs.GET("/templates", s.listTemplates)
			obs.GET("/overview", s.overview)
			obs.POST("/query", s.runQuery)
		}
		obs.GET("/ai_usage", s.aiUsage) // P4 token 消费聚合
	}

	// 受保护组：auth.enabled=true 时要求 Bearer；关闭时中间件为空（行为不变）。
	protected := v1.Group("", s.protectedMiddleware()...)
	{
		protected.POST("/tickets/:id/actions", s.actionTicket)
		protected.POST("/k8s/actions", s.k8sAction)
		protected.DELETE("/cases/:ticket_id", s.deleteCase) // 案例删除（知识库）
		protected.PUT("/admin/prompts/:scene", s.updatePrompt)
		protected.DELETE("/admin/prompts/:scene", s.resetPrompt)
		protected.GET("/admin/providers", s.getProviders)
		protected.PUT("/admin/providers", s.setProviders)
	}
	if s.authSvc != nil {
		v1.POST("/auth/login", s.Login)
		protected.GET("/auth/me", s.Me)
	}
	return r
}

// createTicket godoc
// POST /api/v1/tickets {"title","content","reporter"} → 202 受理（异步处理）。
func (s *Server) createTicket(c *gin.Context) {
	var req struct {
		Title    string `json:"title"`
		Content  string `json:"content"`
		Reporter string `json:"reporter"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	if req.Title == "" || req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "title 与 content 必填"})
		return
	}

	tk := ticket.New(req.Title, req.Content, req.Reporter)
	if err := s.store.CreateTicket(c.Request.Context(), tk); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 异步处理：优先经任务队列（状态机/幂等/重试），无队列时 goroutine 兜底。
	if s.queue != nil {
		payload, _ := json.Marshal(map[string]any{"ticket_id": tk.ID, "resume": false})
		_ = s.queue.Enqueue(context.WithoutCancel(c.Request.Context()), tk.ID, "process_ticket",
			"process:"+tk.ID, payload)
	} else {
		go s.orch.Process(traceContext(c.Request.Context()), tk.ID)
	}

	c.JSON(http.StatusAccepted, gin.H{
		"id":      tk.ID,
		"status":  tk.Status,
		"message": "工单已受理，正在异步处理",
	})
}

func (s *Server) listTickets(c *gin.Context) {
	limit := atoiDefault(c.Query("limit"), 50)
	list, err := s.store.ListTickets(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*ticket.Ticket{}
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

// ticketDetail 详情响应（附 trace 链接）。
type ticketDetail struct {
	*ticket.Ticket
	TraceURL string            `json:"trace_url,omitempty"`
	Messages []*ticket.Message `json:"messages,omitempty"`
}

func (s *Server) getTicket(c *gin.Context) {
	tk, err := s.store.GetTicket(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	msgs, _ := s.store.ListMessages(c.Request.Context(), tk.ID)
	if msgs == nil {
		msgs = []*ticket.Message{}
	}
	detail := &ticketDetail{Ticket: tk, TraceURL: s.traceURL(tk.TraceID), Messages: msgs}
	c.JSON(http.StatusOK, detail)
}

func (s *Server) listMessages(c *gin.Context) {
	msgs, err := s.store.ListMessages(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if msgs == nil {
		msgs = []*ticket.Message{}
	}
	c.JSON(http.StatusOK, gin.H{"items": msgs})
}

func (s *Server) listObservations(c *gin.Context) {
	obs, err := s.store.ListObservations(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	items := make([]observationView, 0, len(obs))
	for _, o := range obs {
		items = append(items, observationView{
			ID: o.ID, Source: o.Source, Query: o.Query,
			Summary: o.Summary, Status: o.Status, CreatedAt: o.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// observationView 观察的对外展示（裁剪原始数据）。
type observationView struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Query     string    `json:"query"`
	Summary   string    `json:"summary"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) traceURL(traceID string) string {
	if traceID == "" || s.jbase == "" {
		return ""
	}
	return s.jbase + "/trace/" + traceID
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
