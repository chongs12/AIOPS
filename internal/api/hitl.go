package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/pkg/telemetry"

	"github.com/gin-gonic/gin"
)

// replyTicket 处理用户补充回复：合并进工单 → investigating → 断点续跑。
func (s *Server) replyTicket(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content 必填"})
		return
	}
	ctx := c.Request.Context()
	tk, err := s.store.GetTicket(ctx, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if tk.Status != ticket.StatusAwaitingInfo {
		c.JSON(http.StatusConflict, gin.H{"error": "工单不在等待补充状态，当前: " + tk.Status})
		return
	}

	// 断点信息持久化：内容合并、状态回 investigating。tk.Content += "\n[补充回复] " + req.Content
	// pending 保留：续跑后由编排器决定二轮追问（round<2）或带默认值继续。
	tk.Status = ticket.StatusInvestigating
	if err := s.store.UpdateTicket(ctx, tk); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = s.store.AppendMessage(ctx, id, &ticket.Message{Role: "user", Content: req.Content, CreatedAt: time.Now()})

	// 异步续跑（保持 trace 上下文）。
	if s.queue != nil {
		payload, _ := json.Marshal(map[string]any{"ticket_id": id, "resume": true})
		_ = s.queue.Enqueue(context.WithoutCancel(c.Request.Context()), id, "resume_ticket",
			fmt.Sprintf("resume:%s:%d", id, time.Now().UnixMilli()), payload)
	} else {
		go s.orch.Resume(traceContext(c.Request.Context()), id)
	}
	c.JSON(http.StatusAccepted, gin.H{"id": id, "status": tk.Status, "message": "已收到补充，继续处理"})
}

// streamTicket SSE 推流：增量推送工单消息与状态变化（推理回放的实时视图）。
func (s *Server) streamTicket(c *gin.Context) {
	id := c.Param("id")
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	// 实时视图挂在后台 trace 上，便于观测长连接。
	_, span := telemetry.Tracer("aiops/internal/api").Start(traceContext(c.Request.Context()), "api.ticket.stream")
	defer span.End()

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}

	ctx := c.Request.Context()
	send := func(event, data string) {
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	seq, lastStatus := 0, ""
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msgs, err := s.store.ListMessages(ctx, id)
			if err != nil {
				send("error", `{"message":"工单不存在"}`)
				return
			}
			for ; seq < len(msgs); seq++ {
				b, _ := json.Marshal(msgs[seq])
				send("message", string(b))
			}
			tk, err := s.store.GetTicket(ctx, id)
			if err != nil {
				return
			}
			if tk.Status != lastStatus {
				lastStatus = tk.Status
				b, _ := json.Marshal(map[string]string{"status": tk.Status})
				send("status", string(b))
			}
			// 终态后再推一次状态即关闭。
			if terminalStatus(lastStatus) {
				// 给客户端一拍时间收到终态。
				idle++
				if idle >= 2 {
					return
				}
			}
		}
	}
}

func terminalStatus(s string) bool {
	return s == ticket.StatusResolved || s == ticket.StatusEscalated ||
		s == ticket.StatusFailed || s == ticket.StatusAwaitingInfo
}

// mutatingAllowlist 允许经 API 触发的处置工具白名单（M3：一键低危 + 高危确认）。
var mutatingAllowlist = map[string]bool{
	"k8s.scale":        true,
	"k8s.rollout_undo": true,
}

// actionTicket 执行处置动作：白名单校验 → 强制 confirmed → 走完整工具管道
// （审计/span/限流）→ 返回结果信封。错误率回落由编排器/用户复查。
func (s *Server) actionTicket(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法"})
		return
	}
	if !mutatingAllowlist[req.Tool] {
		c.JSON(http.StatusForbidden, gin.H{"error": "工具不在处置白名单: " + req.Tool})
		return
	}
	if s.tools == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "工具层未装配"})
		return
	}
	ctx := c.Request.Context()
	if _, err := s.store.GetTicket(ctx, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	// 强制 confirmed=true：HTTP 端点的调用本身即人工确认。
	var in map[string]any
	_ = json.Unmarshal(req.Input, &in)
	if in == nil {
		in = map[string]any{}
	}
	in["confirmed"] = true
	input, _ := json.Marshal(in)

	_ = s.store.AppendMessage(ctx, id, &ticket.Message{
		Role: "user", Agent: "action", Content: "人工确认执行处置: " + req.Tool + " " + string(req.Input), CreatedAt: time.Now(),
	})
	res, err := s.tools.Execute(ctx, id, req.Tool, input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = s.store.AppendMessage(ctx, id, &ticket.Message{
		Role: "agent", Agent: "action",
		Content: fmt.Sprintf("处置执行结果: %s（%s）", res.Status, res.DegradedReason), CreatedAt: time.Now(),
	})
	c.JSON(http.StatusOK, res)
}

// listAudit 工单审计记录（管理面/回放）。
func (s *Server) listAudit(c *gin.Context) {
	if s.audit == nil {
		c.JSON(http.StatusOK, gin.H{"items": []any{}})
		return
	}
	items, err := s.audit.ListByTicket(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if items == nil {
		items = []*tool.AuditEntry{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// rateTicket 满意度评价（仅 resolved 工单可评）。
func (s *Server) rateTicket(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Score   int    `json:"score"`
		Comment string `json:"comment"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法"})
		return
	}
	if req.Score < 1 || req.Score > 5 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "score 取值 1-5"})
		return
	}
	ctx := c.Request.Context()
	tk, err := s.store.GetTicket(ctx, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if tk.Status != ticket.StatusResolved {
		c.JSON(http.StatusConflict, gin.H{"error": "仅已解决工单可评价，当前: " + tk.Status})
		return
	}
	tk.Rating, tk.RatingNote = req.Score, req.Comment
	if err := s.store.UpdateTicket(ctx, tk); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "感谢反馈", "rating": tk.Rating})
}
