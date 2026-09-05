package promtool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"aiops/internal/tool"
)

// QueryInput prometheus.query 工具的输入。
type QueryInput struct {
	Template string            `json:"template"`           // 模板名（必填）
	Params   map[string]string `json:"params,omitempty"`   // 模板参数
	EndTime  string            `json:"end_time,omitempty"` // RFC3339；缺省 = now
}

// QueryTool 模板化 PromQL 查询工具。
type QueryTool struct {
	client *Client
	lib    *Lib
}

// NewQueryTool 构造查询工具。
func NewQueryTool(client *Client, lib *Lib) *QueryTool {
	return &QueryTool{client: client, lib: lib}
}

// Name 工具名。
func (q *QueryTool) Name() string { return "prometheus.query" }

// Desc 工具描述（LLM 可见）。
func (q *QueryTool) Desc() string {
	return "用预置查询模板查询 PreHire 集群 Prometheus 指标。输入模板名与参数，返回时间点序列数据。"
}

// Schema 工具 schema 与可靠性策略（超时10s/幂等重试2次/限流5qps/并发4/结果缓存30s）。
func (q *QueryTool) Schema() tool.Schema {
	return tool.Schema{
		Input: json.RawMessage(`{
  "type": "object",
  "required": ["template"],
  "properties": {
    "template": {"type": "string", "description": "查询模板名"},
    "params":   {"type": "object", "description": "模板参数，如 {\"window\":\"5m\"}"},
    "end_time": {"type": "string", "description": "查询时间点 RFC3339，缺省为当前时间"}
  }
}`),
		Idempotent:    true,
		MaxRetries:    2,
		Timeout:       10 * time.Second,
		RatePerSecond: 5,
		RateBurst:     10,
		Concurrency:   4,
		CacheTTL:      30 * time.Second,
	}
}

// Execute 执行查询。任何失败都返回 status=failed 的结果信封（降级不中断编排）。
func (q *QueryTool) Execute(ctx context.Context, input json.RawMessage) (*tool.Result, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}

	var in QueryInput
	if err := json.Unmarshal(input, &in); err != nil {
		return failed(fmt.Sprintf("输入解析失败: %v", err)), nil
	}

	tpl, ok := q.lib.Get(in.Template)
	if !ok {
		return failed(fmt.Sprintf("未知模板 %q，可用模板: %v", in.Template, names(q.lib.List()))), nil
	}

	promql, err := q.lib.Render(tpl, in.Params)
	if err != nil {
		return failed(fmt.Sprintf("模板渲染失败: %v", err)), nil
	}

	var end time.Time
	if in.EndTime != "" {
		end, err = time.Parse(time.RFC3339, in.EndTime)
		if err != nil {
			return failed(fmt.Sprintf("end_time 非法（需 RFC3339）: %v", err)), nil
		}
	}

	series, err := q.client.QueryInstant(ctx, promql, end)
	if err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("prometheus 查询不可用: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "prometheus", Query: promql},
		}, nil
	}

	return &tool.Result{
		Status: tool.StatusOK,
		Data:   series,
		Evidence: tool.EvidenceMeta{
			Source:  "prometheus",
			Query:   promql,
			Excerpt: excerpt(series),
		},
	}, nil
}

func failed(reason string) *tool.Result {
	return &tool.Result{
		Status:         tool.StatusFailed,
		DegradedReason: reason,
		Evidence:       tool.EvidenceMeta{Source: "prometheus"},
	}
}

// excerpt 取前 5 条序列作为证据摘录。
func excerpt(series json.RawMessage) json.RawMessage {
	var rows []map[string]any
	if err := json.Unmarshal(series, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	if len(rows) > 5 {
		rows = rows[:5]
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return nil
	}
	return out
}

func names(ts []Template) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
