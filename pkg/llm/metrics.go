package llm

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// metrics LLM 调用指标，命名对齐 PreHire：
// ai_calls_total{provider,model,agent,status} / ai_tokens_total{provider,model,agent,type} /
// ai_call_duration_seconds{provider,model,agent}（histogram）。
// 维度含 model/agent（场景），支撑 token 消费按场景归因与消费看板。
type metrics struct {
	calls    metric.Int64Counter
	tokens   metric.Int64Counter
	duration metric.Float64Histogram
}

func newMetrics() *metrics {
	m := otel.Meter("aiops/pkg/llm")
	var mm metrics
	var err error
	if mm.calls, err = m.Int64Counter("ai.calls.total",
		metric.WithDescription("LLM 调用次数")); err != nil {
		slog.Warn("ai.calls.total 指标创建失败", "err", err)
	}
	if mm.tokens, err = m.Int64Counter("ai.tokens.total",
		metric.WithDescription("LLM token 用量")); err != nil {
		slog.Warn("ai.tokens.total 指标创建失败", "err", err)
	}
	if mm.duration, err = m.Float64Histogram("ai.call.duration",
		metric.WithDescription("LLM 调用耗时（秒）"),
		metric.WithUnit("s")); err != nil {
		slog.Warn("ai.call.duration 指标创建失败", "err", err)
	}
	return &mm
}

// record 记录一次 LLM 调用。model/agent 为归因维度（空值用 unknown 占位，保证序列标签完整）。
func (m *metrics) record(provider, model, agent, status string, d time.Duration, resp *ChatResponse) {
	if m == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	if agent == "" {
		agent = "unknown"
	}
	attrs := metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("model", model),
		attribute.String("agent", agent),
	)
	if m.calls != nil {
		m.calls.Add(nil, 1, metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
			attribute.String("agent", agent),
			attribute.String("status", status)))
	}
	if m.duration != nil {
		m.duration.Record(nil, d.Seconds(), metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
			attribute.String("agent", agent)))
	}
	if resp != nil && m.tokens != nil {
		if resp.PromptTokens > 0 {
			m.tokens.Add(nil, int64(resp.PromptTokens), metric.WithAttributes(
				attribute.String("provider", provider),
				attribute.String("model", model),
				attribute.String("agent", agent),
				attribute.String("type", "prompt")))
		}
		if resp.CompletionTokens > 0 {
			m.tokens.Add(nil, int64(resp.CompletionTokens), metric.WithAttributes(
				attribute.String("provider", provider),
				attribute.String("model", model),
				attribute.String("agent", agent),
				attribute.String("type", "completion")))
		}
	}
	_ = attrs // 保留：后续如需统一 attrs 可复用
}
