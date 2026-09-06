// 告警接入（P6）：webhook 收 Alertmanager/简化格式 → 去重 → 自动建单。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"aiops/internal/ticket"
	"aiops/pkg/cache"

	"github.com/gin-gonic/gin"
)

// alertInput 兼容两种载荷：
// 1) Alertmanager v2: {"alerts":[{"labels":{...},"annotations":{...},"fingerprint":"..."}]}
// 2) 简化: {"title","content","service","severity"}
type alertInput struct {
	Alerts []struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Service  string `json:"service"`
	Severity string `json:"severity"`
}

// severityPriority severity→priority 映射。
var severityPriority = map[string]string{
	"critical": "P0", "Critical": "P0", "CRITICAL": "P0",
	"warning": "P1", "Warning": "P1", "WARNING": "P1",
	"info": "P2", "page": "P0", "ticket": "P1",
}

// alertWebhook 告警入口：去重 → 建单 → 异步处理。
func (s *Server) alertWebhook(c *gin.Context) {
	var in alertInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "载荷非法: " + err.Error()})
		return
	}

	type result struct {
		TicketID string `json:"ticket_id"`
		Deduped  bool   `json:"deduped"`
	}
	results := []result{}

	handle := func(title, content, service, severity, fingerprint string) {
		if title == "" {
			return
		}
		if fingerprint == "" {
			sum := sha256.Sum256([]byte(title + "|" + service))
			fingerprint = hex.EncodeToString(sum[:8])
		}
		dedupKey := cache.Key("alert", "fp", fingerprint)
		if ticketID, ok := s.dedupGet(dedupKey); ok {
			results = append(results, result{TicketID: ticketID, Deduped: true})
			return
		}
		priority := severityPriority[severity]
		if priority == "" {
			priority = "P2"
		}
		tk := ticket.New(title, content, "alertmanager")
		tk.Priority = priority
		if service != "" {
			tk.Entities.Service = service
		}
		if err := s.store.CreateTicket(c.Request.Context(), tk); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		s.dedupSet(dedupKey, tk.ID)
		if s.queue != nil {
			payload, _ := json.Marshal(map[string]any{"ticket_id": tk.ID, "resume": false})
			_ = s.queue.Enqueue(context.WithoutCancel(c.Request.Context()), tk.ID,
				"process_ticket", "process:"+tk.ID, payload)
		} else {
			go s.orch.Process(traceContext(c.Request.Context()), tk.ID)
		}
		results = append(results, result{TicketID: tk.ID, Deduped: false})
	}

	if len(in.Alerts) > 0 {
		for _, a := range in.Alerts {
			title := a.Annotations["summary"]
			if title == "" {
				title = a.Labels["alertname"]
			}
			content := a.Annotations["description"]
			if content == "" {
				content = fmt.Sprintf("告警 %s（labels: %v）", a.Labels["alertname"], a.Labels)
			}
			handle(title, content, a.Labels["service"], a.Labels["severity"], a.Fingerprint)
		}
	} else {
		handle(in.Title, in.Content, in.Service, in.Severity, "")
	}

	c.JSON(http.StatusOK, gin.H{"items": results})
}

// dedupGet 幂等去重读取（复用缓存包；Redis/L1，故障降级为不去重——重复告警可容忍，不阻塞）。
func (s *Server) dedupGet(key string) (string, bool) {
	if s.cache == nil {
		return "", false
	}
	v, ok := s.cache.Get(context.Background(), key)
	if !ok {
		return "", false
	}
	var ticketID string
	_ = json.Unmarshal(v, &ticketID)
	return ticketID, ticketID != ""
}

// dedupSet 记录指纹→工单（24h）。
func (s *Server) dedupSet(key, ticketID string) {
	if s.cache == nil {
		return
	}
	b, _ := json.Marshal(ticketID)
	s.cache.Set(context.Background(), key, b, 24*time.Hour)
}
