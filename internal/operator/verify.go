package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"aiops/internal/tool/promtool"
)

// jsonMarshal 独立小工具，避免与 encoding/json 全名冲突时语义混乱。
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// Verifier 恢复回验查询接口（Prometheus 即时查询）。
type Verifier interface {
	// Query 按模板名渲染 PromQL 并执行即时查询，返回首个样本值。
	// 空结果视为 0（如 http_5xx_rate 无 5xx 序列即 0 速率——健康语义，
	// 与 internal/orchestrator/verify.go 的解析行为一致）。
	Query(ctx context.Context, template string, params map[string]string) (float64, error)
}

// promVerifier promtool 模板库 + HTTP API 的真实实现。
type promVerifier struct {
	client *promtool.Client
	lib    *promtool.Lib
}

// NewPromVerifier 构造真实回验层。
func NewPromVerifier(baseURL string, timeout time.Duration) Verifier {
	return &promVerifier{client: promtool.NewClient(baseURL, timeout), lib: promtool.BuiltinLib()}
}

// Query 执行一次回验查询。
func (v *promVerifier) Query(ctx context.Context, template string, params map[string]string) (float64, error) {
	tpl, ok := v.lib.Get(template)
	if !ok {
		return 0, fmt.Errorf("未知回验模板 %q", template)
	}
	expr, err := v.lib.Render(tpl, params)
	if err != nil {
		return 0, fmt.Errorf("渲染回验模板: %w", err)
	}
	raw, err := v.client.QueryInstant(ctx, expr, time.Time{})
	if err != nil {
		return 0, fmt.Errorf("prometheus 查询失败: %w", err)
	}
	var rows []struct {
		Value []any `json:"value"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, fmt.Errorf("prometheus 响应解析失败: %w", err)
	}
	if len(rows) == 0 || len(rows[0].Value) < 2 {
		return 0, nil
	}
	var val float64
	fmt.Sscan(fmt.Sprint(rows[0].Value[1]), &val)
	return val, nil
}
