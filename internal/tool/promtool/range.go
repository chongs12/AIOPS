package promtool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"aiops/internal/tool"
)

// 工具名。
const (
	NameRange   = "prometheus.query_range"
	NameFree    = "prometheus.free_query"
	ConsoleScop = "console" // 控制台类查询的 ticketID 标记
)

// minStep / maxPoints range 查询护栏，防高基数滥用。
const (
	minStep   = 15 * time.Second
	maxPoints = 2000
)

// RangeInput prometheus.query_range 输入：模板 + 参数 + 时间区间。
type RangeInput struct {
	Template    string            `json:"template"`
	Params      map[string]string `json:"params,omitempty"`
	Start       string            `json:"start"` // RFC3339
	End         string            `json:"end"`   // RFC3339
	StepSeconds int               `json:"step_seconds"`
}

// RangeTool 模板化 range 查询（趋势曲线）。
type RangeTool struct {
	client *Client
	lib    *Lib
}

// NewRangeTool 构造。
func NewRangeTool(client *Client, lib *Lib) *RangeTool {
	return &RangeTool{client: client, lib: lib}
}

// Name 工具名。
func (q *RangeTool) Name() string { return NameRange }

// Desc 工具描述。
func (q *RangeTool) Desc() string {
	return "按模板查询一段时间区间的指标序列（趋势曲线），返回 matrix 数据。"
}

// Schema 工具 schema。
func (q *RangeTool) Schema() tool.Schema {
	return tool.Schema{
		Input: json.RawMessage(`{
  "type": "object",
  "required": ["template", "start", "end"],
  "properties": {
    "template": {"type": "string"},
    "params":   {"type": "object"},
    "start":    {"type": "string", "description": "RFC3339"},
    "end":      {"type": "string", "description": "RFC3339"},
    "step_seconds": {"type": "number", "description": "采样步长，最小 15"}
  }
}`),
		Idempotent:    true,
		MaxRetries:    1,
		Timeout:       15 * time.Second,
		RatePerSecond: 3,
		RateBurst:     6,
		Concurrency:   4,
		CacheTTL:      30 * time.Second,
	}
}

// Execute 执行 range 查询。
func (q *RangeTool) Execute(ctx context.Context, input json.RawMessage) (*tool.Result, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var in RangeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return failedResult(fmt.Sprintf("输入解析失败: %v", err)), nil
	}
	promql, start, end, step, err := q.prepare(in.Template, in.Params, in.Start, in.End, in.StepSeconds)
	if err != nil {
		return failedResult(err.Error()), nil
	}
	matrix, err := q.client.QueryRange(ctx, promql, start, end, step)
	if err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("prometheus range 查询不可用: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "prometheus", Query: promql},
		}, nil
	}
	return &tool.Result{
		Status: tool.StatusOK,
		Data:   matrix,
		Evidence: tool.EvidenceMeta{
			Source: "prometheus", Query: promql,
			Excerpt: firstSeriesKeys(matrix),
		},
	}, nil
}

func (q *RangeTool) prepare(tplName string, params map[string]string, startS, endS string, stepSec int) (string, time.Time, time.Time, time.Duration, error) {
	tpl, ok := q.lib.Get(tplName)
	if !ok {
		return "", time.Time{}, time.Time{}, 0, fmt.Errorf("未知模板 %q", tplName)
	}
	promql, err := q.lib.Render(tpl, params)
	if err != nil {
		return "", time.Time{}, time.Time{}, 0, fmt.Errorf("模板渲染失败: %w", err)
	}
	start, err := time.Parse(time.RFC3339, startS)
	if err != nil {
		return "", time.Time{}, time.Time{}, 0, fmt.Errorf("start 非法（需 RFC3339）: %w", err)
	}
	end, err := time.Parse(time.RFC3339, endS)
	if err != nil {
		return "", time.Time{}, time.Time{}, 0, fmt.Errorf("end 非法（需 RFC3339）: %w", err)
	}
	if !end.After(start) {
		return "", time.Time{}, time.Time{}, 0, fmt.Errorf("end 必须晚于 start")
	}
	if stepSec < int(minStep.Seconds()) {
		stepSec = int(minStep.Seconds())
	}
	step := time.Duration(stepSec) * time.Second
	if points := end.Sub(start) / step; points > maxPoints {
		return "", time.Time{}, time.Time{}, 0, fmt.Errorf("查询点数 %d 超过上限 %d（请增大 step）", points, maxPoints)
	}
	return promql, start, end, step, nil
}

// ---- 受限自由查询（D6 后期放开路径：白名单校验）----

// FreeInput prometheus.free_query 输入。
type FreeInput struct {
	Expr        string `json:"expr"`
	Start       string `json:"start"`
	End         string `json:"end"`
	StepSeconds int    `json:"step_seconds"`
}

// FreeTool 受限自由 PromQL 查询：metric 白名单 + 函数白名单 + 禁注释。
type FreeTool struct {
	client *Client
}

// NewFreeTool 构造。
func NewFreeTool(client *Client) *FreeTool { return &FreeTool{client: client} }

// Name 工具名。
func (q *FreeTool) Name() string { return NameFree }

// Desc 工具描述。
func (q *FreeTool) Desc() string {
	return "受限自由 PromQL 查询（指标名须在白名单内），用于高级排障场景。"
}

// Schema 工具 schema：限流更紧（1qps）、并发 2。
func (q *FreeTool) Schema() tool.Schema {
	return tool.Schema{
		Input: json.RawMessage(`{
  "type": "object",
  "required": ["expr", "start", "end"],
  "properties": {
    "expr": {"type": "string", "description": "PromQL 表达式"},
    "start": {"type": "string"}, "end": {"type": "string"},
    "step_seconds": {"type": "number"}
  }
}`),
		Idempotent:    true,
		MaxRetries:    1,
		Timeout:       15 * time.Second,
		RatePerSecond: 1,
		RateBurst:     3,
		Concurrency:   2,
		CacheTTL:      30 * time.Second,
	}
}

// Execute 执行自由查询。
func (q *FreeTool) Execute(ctx context.Context, input json.RawMessage) (*tool.Result, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var in FreeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return failedResult(fmt.Sprintf("输入解析失败: %v", err)), nil
	}
	if err := ValidateFreeQuery(in.Expr); err != nil {
		return failedResult(fmt.Sprintf("表达式被拒绝: %v", err)), nil
	}
	start, err := time.Parse(time.RFC3339, in.Start)
	if err != nil {
		return failedResult("start 非法（需 RFC3339）"), nil
	}
	end, err := time.Parse(time.RFC3339, in.End)
	if err != nil {
		return failedResult("end 非法（需 RFC3339）"), nil
	}
	if !end.After(start) {
		return failedResult("end 必须晚于 start"), nil
	}
	stepSec := in.StepSeconds
	if stepSec < int(minStep.Seconds()) {
		stepSec = int(minStep.Seconds())
	}
	step := time.Duration(stepSec) * time.Second
	if points := end.Sub(start) / step; points > maxPoints {
		return failedResult(fmt.Sprintf("查询点数 %d 超过上限 %d", points, maxPoints)), nil
	}
	matrix, err := q.client.QueryRange(ctx, in.Expr, start, end, step)
	if err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("prometheus 查询不可用: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "prometheus", Query: in.Expr},
		}, nil
	}
	return &tool.Result{
		Status: tool.StatusOK, Data: matrix,
		Evidence: tool.EvidenceMeta{Source: "prometheus", Query: in.Expr, Excerpt: firstSeriesKeys(matrix)},
	}, nil
}

func failedResult(reason string) *tool.Result {
	return &tool.Result{
		Status:         tool.StatusFailed,
		DegradedReason: reason,
		Evidence:       tool.EvidenceMeta{Source: "prometheus"},
	}
}

func firstSeriesKeys(matrix json.RawMessage) json.RawMessage {
	var rows []map[string]any
	if err := json.Unmarshal(matrix, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	if len(rows) > 3 {
		rows = rows[:3]
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return nil
	}
	return out
}

// ---- 自由查询白名单 ----

// metricWhitelist 允许查询的指标名前缀（真实集群存在的指标族）。
var metricWhitelist = []string{
	"http_requests_total", "http_request_duration_seconds", "up",
	"ai_", "go_", "process_", "container_", "active_websocket_connections", "kube_",
}

// promFuncs 允许的函数/关键字（非指标名）。
var promFuncs = map[string]bool{
	"rate": true, "irate": true, "increase": true, "delta": true, "idelta": true,
	"deriv": true, "predict_linear": true, "histogram_quantile": true,
	"sum": true, "avg": true, "min": true, "max": true, "count": true,
	"count_values": true, "group": true, "stddev": true, "stdvar": true,
	"quantile": true, "topk": true, "bottomk": true, "abs": true, "sign": true,
	"ceil": true, "floor": true, "round": true, "clamp": true, "clamp_min": true,
	"clamp_max": true, "sort": true, "sort_desc": true, "absent": true,
	"vector": true, "scalar": true, "label_replace": true, "label_join": true,
	"ln": true, "log2": true, "log10": true, "exp": true, "sqrt": true,
	"pi": true, "sin": true, "cos": true, "tan": true, "deg": true, "rad": true,
	"day_of_month": true, "day_of_week": true, "day_of_year": true,
	"days_in_month": true, "hour": true, "minute": true, "month": true, "year": true, "timestamp": true,
	"changes": true, "resets": true, "holt_winters": true, "sum_over_time": true,
	"avg_over_time": true, "min_over_time": true, "max_over_time": true,
	"stddev_over_time": true, "stdvar_over_time": true, "count_over_time": true,
	"quantile_over_time": true, "last_over_time": true, "present_over_time": true,
	// 关键字
	"by": true, "without": true, "on": true, "ignoring": true,
	"group_left": true, "group_right": true, "offset": true, "bool": true,
	"and": true, "or": true, "unless": true, "atan2": true, "start": true, "end": true,
}

var identRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

// durationRe / sizeRe PromQL 字面量（时间窗/字节后缀），先剥离再扫标识符。
var (
	durationRe = regexp.MustCompile(`\d+(ms|[smhdwy])`)
	sizeRe     = regexp.MustCompile(`\d+(Ki|Mi|Gi|Ti|KB|MB|GB|TB)`)
)

// ValidateFreeQuery 校验自由 PromQL：禁注释、标识符须为函数白名单或指标前缀白名单。
func ValidateFreeQuery(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return fmt.Errorf("表达式为空")
	}
	if strings.Contains(expr, "#") {
		return fmt.Errorf("不允许注释")
	}
	expr = durationRe.ReplaceAllString(expr, "1")
	expr = sizeRe.ReplaceAllString(expr, "1")
	for _, id := range identRe.FindAllString(expr, -1) {
		if promFuncs[id] {
			continue
		}
		if !metricAllowed(id) {
			return fmt.Errorf("指标 %q 不在白名单（http_requests_total/http_request_duration_seconds/up/ai_*/go_*/container_* 等）", id)
		}
	}
	return nil
}

func metricAllowed(name string) bool {
	for _, p := range metricWhitelist {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
