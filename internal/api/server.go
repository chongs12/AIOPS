// Package api 提供 HTTP 接口（gin）：工单提交/查询/流水，异步触发编排处理。
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

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
	tools      ToolExecutor // 处置动作执行
	obs        *ObsDeps     // 观测查询（可 nil）
	audit      AuditReader  // 审计查询（可 nil）
	prompts    PromptAdmin  // prompt 热更新（可 nil）
	caseSearch CaseSearch   // 案例检索（可 nil）
	jbase      string       // Jaeger 查询页基地址（trace 链接）
}

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
		v1.POST("/tickets/:id/reply", s.replyTicket)    // HITL：补充信息，断点续跑
		v1.GET("/tickets/:id/stream", s.streamTicket)   // SSE 推流
		v1.POST("/tickets/:id/actions", s.actionTicket) // 处置动作（白名单+人工确认）
		v1.GET("/tickets/:id/audit", s.listAudit)       // 审计记录
		admin := v1.Group("/admin")
		{
			admin.GET("/prompts", s.listPrompts)
			admin.PUT("/prompts/:scene", s.updatePrompt)
			admin.DELETE("/prompts/:scene", s.resetPrompt)
		}
		v1.GET("/cases/search", s.searchCases) // M5 相似案例检索
		k8sGrp := v1.Group("/k8s")
		{
			k8sGrp.GET("/deployments", s.listK8sDeployments)
			k8sGrp.GET("/events", s.listK8sEvents)
			k8sGrp.GET("/logs", s.queryK8sLogs)
			k8sGrp.POST("/actions", s.k8sAction)
		}
		v1.GET("/jaeger/services", s.jaegerServices)
		v1.GET("/jaeger/traces", s.jaegerTraces)
		obs := v1.Group("/observability")
		{
			obs.GET("/templates", s.listTemplates)
			obs.GET("/overview", s.overview)
			obs.POST("/query", s.runQuery)
		}
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

	// 异步处理：脱离请求生命周期，但保持 trace 上下文连续。
	go s.orch.Process(traceContext(c.Request.Context()), tk.ID)

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
