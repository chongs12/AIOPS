package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"aiops/internal/evidence"
	"aiops/internal/tool"
)

// 按工单类型选取的查询模板集合（M1 覆盖 5xx 与通用体检）。
var templatesByType = map[string][]string{
	ticketType5xx: {
		"http_5xx_rate", "http_total_rate", "http_5xx_by_path", "http_latency_p99", "up_status",
	},
	ticketTypeOOM: {
		"container_memory_working_set", "up_status",
	},
	ticketTypeGeneric: {
		"up_status", "http_5xx_ratio", "http_latency_p99",
	},
}

const (
	ticketType5xx     = "5xx_surge"
	ticketTypeOOM     = "pod_oom"
	ticketTypeGeneric = "__generic"
)

// MetricsAgent 指标检索 Agent：按工单类型执行模板化 Prometheus 查询，
// 把结果转成观察写入黑板。M1 中数值解读交给根因 Agent（确定性与省 token）。
type MetricsAgent struct{}

// NewMetricsAgent 构造。
func NewMetricsAgent() *MetricsAgent { return &MetricsAgent{} }

// Name Agent 名。
func (a *MetricsAgent) Name() string { return NameMetrics }

// Run 执行模板集查询并生成观察。
func (a *MetricsAgent) Run(ctx context.Context, rc *RunContext) (*Output, error) {
	names, ok := templatesByType[rc.Ticket.Type]
	if !ok {
		names = templatesByType[ticketTypeGeneric]
	}
	window := fmt.Sprintf("%dm", maxInt(rc.Ticket.Entities.WindowMinutes, 1))
	end := rc.Ticket.Entities.WindowEndOrNow()

	res := &MetricsResult{}
	for _, tpl := range names {
		input, _ := json.Marshal(map[string]any{
			"template": tpl,
			"params":   map[string]string{"window": window},
			"end_time": end.UTC().Format(timeRFC3339),
		})
		tr, err := rc.Tools.Execute(ctx, rc.Ticket.ID, "prometheus.query", input)
		if err != nil {
			// 工具层契约：一般失败已是 failed 信封；此处为调用方错误。
			return nil, fmt.Errorf("metrics 工具调用失败: %w", err)
		}
		obs := ObservationFromToolResult(tpl, tr)
		id := rc.Board.AddObservation(obs)
		res.ObservationIDs = append(res.ObservationIDs, id)
		if obs.Status == tool.StatusFailed {
			res.FailedCount++
		} else {
			res.Highlights = append(res.Highlights, obs.Summary)
		}
	}

	note := fmt.Sprintf("指标检索：完成 %d 项查询（失败 %d 项）", len(names), res.FailedCount)
	return &Output{Note: note, Payload: res}, nil
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

// observationFromToolResult 工具结果 → 观察（含机械摘要，供 LLM 阅读）。
func ObservationFromToolResult(template string, tr *tool.Result) evidence.Observation {
	obs := evidence.Observation{
		Source:  "prometheus.query",
		Query:   tr.Evidence.Query,
		Status:  tr.Status,
		Summary: summarize(template, tr),
	}
	if len(tr.Data) > 0 {
		obs.Data = tr.Data
	}
	return obs
}

// summarize 生成观察摘要：ok → 序列数与峰值；failed → 降级原因。
func summarize(template string, tr *tool.Result) string {
	if tr.Status != tool.StatusOK {
		return fmt.Sprintf("查询失败（%s）：%s", template, tr.DegradedReason)
	}
	var rows []struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(tr.Data, &rows); err != nil {
		return fmt.Sprintf("查询完成（%s），结果解析异常", template)
	}
	if len(rows) == 0 {
		return fmt.Sprintf("查询完成（%s）：无匹配序列（该指标当前无数据）", template)
	}
	labels := make([]string, 0, len(rows))
	best := 0.0
	bestLabel := ""
	for _, r := range rows {
		lb := formatMetricLabels(r.Metric)
		labels = append(labels, lb)
		if v := firstNumber(r.Value); v > best {
			best, bestLabel = v, lb
		}
	}
	sort.Strings(labels)
	return fmt.Sprintf("查询完成（%s）：共 %d 条序列，最大值 %.4g（%s）。序列: %s",
		template, len(rows), best, bestLabel, strings.Join(labels, "; "))
}

func formatMetricLabels(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(k + "=" + m[k])
	}
	sb.WriteString("}")
	return sb.String()
}

func firstNumber(value []any) float64 {
	if len(value) < 2 {
		return 0
	}
	s, ok := value[1].(string)
	if !ok {
		return 0
	}
	var f float64
	_, _ = fmt.Sscan(s, &f)
	return f
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
