// Package notify 通知渠道（P4）：webhook 通知，失败仅告警不阻塞主流程。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"aiops/pkg/httpx"
)

// Payload 通知载荷。
type Payload struct {
	Event      string  `json:"event"` // ticket.resolved | ticket.escalated
	TicketID   string  `json:"ticket_id"`
	Title      string  `json:"title"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence,omitempty"`
	TraceURL   string  `json:"trace_url,omitempty"`
}

// Notifier webhook 通知器。
type Notifier struct {
	url string
}

// NewNotifier 构造；url 为空时 Notify 直接跳过。
func NewNotifier(url string) *Notifier {
	return &Notifier{url: url}
}

// Enabled 报告通知是否配置。
func (n *Notifier) Enabled() bool { return n != nil && n.url != "" }

// Notify 推送 webhook（10s 超时；失败仅日志）。
func (n *Notifier) Notify(ctx context.Context, p Payload) {
	if !n.Enabled() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := json.Marshal(p)
	if err != nil {
		slog.Warn("通知序列化失败", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := httpx.NewClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("通知推送失败（不阻塞主流程）", "url", n.url, "err", err)
		return
	}
	defer resp.Body.Close()
	slog.Info("通知已推送", "event", p.Event, "ticket", p.TicketID, "status", resp.StatusCode)
}
