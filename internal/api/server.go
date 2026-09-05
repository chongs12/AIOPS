// Package api 提供 HTTP 接口（gin）：工单提交/查询/流水，异步触发编排处理。
package api

import (
	"net/http"
	"time"

	"aiops/internal/orchestrator"
	"aiops/internal/ticket"

	"github.com/gin-gonic/gin"
)

// Server HTTP 服务。
type Server struct {
	store ticket.Store
	orch  *orchestrator.Orchestrator
	jbase string // Jaeger 查询页基地址（trace 链接）
}

// NewServer 构造。
func NewServer(store ticket.Store, orch *orchestrator.Orchestrator, jaegerBaseURL string) *Server {
	gin.SetMode(gin.ReleaseMode)
	return &Server{store: store, orch: orch, jbase: jaegerBaseURL}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery(), requestLog())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	v1 := r.Group("/api/v1")
	{
		v1.POST("/tickets", s.createTicket)
		v1.GET("/tickets", s.listTickets)
		v1.GET("/tickets/:id", s.getTicket)
		v1.GET("/tickets/:id/messages", s.listMessages)
		v1.GET("/tickets/:id/observations", s.listObservations)
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
