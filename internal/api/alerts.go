// 告警接入（P6）：webhook 收 Alertmanager/简化格式 → 去重 → 自动建单。
// 处置联动：告警携带 aiops-remediation-action 标签时，建单同时自动创建
// 待审批的 Remediation CR（审批门仍在人工，operator 执行绝不自动触发）。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"aiops/internal/remediation"
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
		TicketID    string `json:"ticket_id"`
		Deduped     bool   `json:"deduped"`
		Remediation string `json:"remediation,omitempty"` // 联动创建的处置 CR 名
	}
	results := []result{}

	handle := func(title, content, service, severity, fingerprint string, labels map[string]string) {
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
		results = append(results, result{TicketID: tk.ID, Remediation: s.tryCreateRemediation(c.Request.Context(), tk.ID, labels)})
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
			handle(title, content, a.Labels["service"], a.Labels["severity"], a.Fingerprint, a.Labels)
		}
	} else {
		handle(in.Title, in.Content, in.Service, in.Severity, "", nil)
	}

	c.JSON(http.StatusOK, gin.H{"items": results})
}

// tryCreateRemediation 告警→处置联动：告警标签声明处置意图时自动创建
// 待审批 Remediation CR，与工单双向关联（label aiops.io/ticket）。
// 失败仅告警不阻断建单主流程。标签约定：
//
//	aiops-remediation-action: k8s.scale | k8s.rollout_undo
//	aiops-deployment:         目标 Deployment 名
//	aiops-replicas:           scale 目标副本数（scale 必填）
//	aiops-reason:             根因摘要（可选）
func (s *Server) tryCreateRemediation(ctx context.Context, ticketID string, labels map[string]string) string {
	if s.remedy == nil || len(labels) == 0 {
		return ""
	}
	action := labels["aiops-remediation-action"]
	if action == "" {
		return ""
	}
	deployment := labels["aiops-deployment"]
	if deployment == "" {
		slog.Warn("告警要求自动处置但缺 aiops-deployment 标签，跳过 CR 创建", "ticket", ticketID)
		return ""
	}
	in := remediation.CreateInput{
		TicketRef: ticketID, Deployment: deployment, Action: action, Reason: labels["aiops-reason"],
	}
	if r := labels["aiops-replicas"]; r != "" {
		if n, err := strconv.ParseInt(r, 10, 32); err == nil {
			rep := int32(n)
			in.Replicas = &rep
		} else {
			slog.Warn("aiops-replicas 非法，按缺省处理", "value", r, "err", err)
		}
	}
	rem, err := s.remedy.Create(ctx, in)
	if err != nil {
		slog.Warn("告警联动创建处置 CR 失败（工单照常流转）", "ticket", ticketID, "err", err)
		return ""
	}
	slog.Info("告警联动已创建待审批处置 CR", "ticket", ticketID, "rem", rem.Name)
	return rem.Name
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
