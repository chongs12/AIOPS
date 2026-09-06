// 恢复复查（P4）：处置动作执行成功后自动复查故障指标，恢复则自动关单。
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"aiops/internal/ticket"
)

// VerifyConfig 复查配置。
type VerifyConfig struct {
	WaitSeconds  int     `json:"wait_seconds"`  // 处置后等待观察时长，默认 60
	Threshold    float64 `json:"threshold"`     // 5xx 速率阈值（rps），默认 0.05
	Window       string  `json:"window"`        // 复查查询窗口，默认 2m
	RateTemplate string  `json:"rate_template"` // 复查用查询模板，默认 http_5xx_rate
}

// DefaultVerifyConfig 默认配置。
func DefaultVerifyConfig() VerifyConfig {
	return VerifyConfig{WaitSeconds: 60, Threshold: 0.05, Window: "2m", RateTemplate: "http_5xx_rate"}
}

// verifyRecovery 执行一次恢复复查：
// 返回 (recovered, value, err)。查询失败视为未恢复（保守），由任务重试机制兜底。
func (o *Orchestrator) verifyRecovery(ctx context.Context, cfg VerifyConfig) (bool, float64, error) {
	input, _ := json.Marshal(map[string]any{
		"template": cfg.RateTemplate,
		"params":   map[string]string{"window": cfg.Window},
	})
	res, err := o.tools.Execute(ctx, "console", "prometheus.query", input)
	if err != nil {
		return false, 0, err
	}
	if res.Status != "ok" {
		return false, 0, fmt.Errorf("复查查询失败: %s", res.DegradedReason)
	}
	var rows []struct {
		Value []any `json:"value"`
	}
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		return false, 0, err
	}
	var v float64
	if len(rows) > 0 && len(rows[0].Value) > 1 {
		fmt.Sscan(fmt.Sprint(rows[0].Value[1]), &v)
	}
	return v < cfg.Threshold, v, nil
}

// scheduleVerify 处置成功后入队复查任务（由 API 层调用；内部等待由任务逻辑承担）。
func (o *Orchestrator) ScheduleVerify(ctx context.Context, ticketID string) {
	if o.queue == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticketID,
		"cfg":       o.verifyCfg,
	})
	go func() {
		// 等待观察期（让指标反映处置效果），再入队正式复查任务。
		time.Sleep(time.Duration(o.verifyCfg.WaitSeconds) * time.Second)
		if err := o.queue.Enqueue(context.WithoutCancel(ctx), ticketID, "verify_recovery",
			"verify:"+ticketID+":"+fmt.Sprint(time.Now().Unix()/60), payload); err != nil {
			slog.Warn("复查任务入队失败", "ticket", ticketID, "err", err)
		}
	}()
}

// runVerify 任务执行体：复查通过 → resolved；未通过 → 通知并保持现状。
func (o *Orchestrator) RunVerifyTask(ctx context.Context, payload json.RawMessage) error {
	var p struct {
		TicketID string       `json:"ticket_id"`
		Cfg      VerifyConfig `json:"cfg"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	cfg := p.Cfg
	if cfg.Threshold == 0 {
		cfg = DefaultVerifyConfig()
	}
	tk, err := o.store.GetTicket(ctx, p.TicketID)
	if err != nil {
		return err
	}
	recovered, v, err := o.verifyRecovery(ctx, cfg)
	if err != nil {
		return err // 交给任务重试梯度
	}
	if recovered {
		tk.Status = ticket.StatusResolved
		if err := o.store.UpdateTicket(ctx, tk); err != nil {
			return err
		}
		o.appendMsg(ctx, tk.ID, "system", "",
			fmt.Sprintf("✅ 复查通过：5xx 速率已回落至 %.4f rps（阈值 %.2f），工单自动关闭。", v, cfg.Threshold))
		return nil
	}
	o.appendMsg(ctx, tk.ID, "system", "",
		fmt.Sprintf("⚠ 复查未通过：5xx 速率 %.4f rps 仍高于阈值 %.2f，请人工继续跟进。", v, cfg.Threshold))
	return nil
}

// 确保 queue 字段存在（与主文件共享）。
var _ = time.Now
