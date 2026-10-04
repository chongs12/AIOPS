package operator

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// opMetrics operator 自定义指标（OTel meter 全局注册，随 :6060/:8060 导出）：
// remediation_transitions_total{action,phase} / remediation_reconcile_duration_seconds{action}。
type opMetrics struct {
	transitions metric.Int64Counter
	duration    metric.Float64Histogram
}

func newMetrics() *opMetrics {
	m := otel.Meter("aiops/internal/operator")
	var mm opMetrics
	var err error
	if mm.transitions, err = m.Int64Counter("remediation.transitions.total",
		metric.WithDescription("Remediation 状态迁移次数")); err != nil {
		slog.Warn("remediation.transitions.total 指标创建失败", "err", err)
	}
	if mm.duration, err = m.Float64Histogram("remediation.reconcile.duration",
		metric.WithDescription("单次 reconcile 耗时（秒）"),
		metric.WithUnit("s")); err != nil {
		slog.Warn("remediation.reconcile.duration 指标创建失败", "err", err)
	}
	return &mm
}

// transition 记录一次阶段迁移。
func (m *opMetrics) transition(action, phase string, d time.Duration) {
	if m == nil {
		return
	}
	if action == "" {
		action = "unknown"
	}
	if phase == "" {
		phase = "unknown"
	}
	if m.transitions != nil {
		m.transitions.Add(nil, 1, metric.WithAttributes(
			attribute.String("action", action),
			attribute.String("phase", phase)))
	}
	if m.duration != nil {
		m.duration.Record(nil, d.Seconds(), metric.WithAttributes(
			attribute.String("action", action)))
	}
}
