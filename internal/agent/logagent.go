package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aiops/internal/tool"
)

// NameLog 日志解析 Agent 名。
const NameLog = "log"

// LogAgent 日志解析 Agent：拉取故障时间窗内的容器日志、K8s 事件与 Pod 状态，
// 提取异常模式（ERROR/panic/OOMKilled/BackOff 等）写入观察集。
// M2 数据源为 client-go 直拉（docs/03 的方案②）；Loki 为后续可选增强。
type LogAgent struct{}

// NewLogAgent 构造。
func NewLogAgent() *LogAgent { return &LogAgent{} }

// Name Agent 名。
func (a *LogAgent) Name() string { return NameLog }

// Run 执行日志/事件/状态三类查询。
func (a *LogAgent) Run(ctx context.Context, rc *RunContext) (*Output, error) {
	prefix := podPrefixOf(rc.Ticket.Entities.Service)
	since := int64(maxInt(rc.Ticket.Entities.WindowMinutes, 30))
	res := &MetricsResult{}

	queries := []struct {
		tool   string
		input  map[string]any
		summar func(tr *tool.Result) string
	}{
		{k8sToolPods, map[string]any{"pod_prefix": prefix}, summarizePods},
		{k8sToolEvents, map[string]any{"pod_prefix": prefix, "since_minutes": since}, summarizeEvents},
		{k8sToolLogs, map[string]any{"pod_prefix": prefix, "since_minutes": since, "tail_lines": 200}, summarizeLogs},
	}

	for _, q := range queries {
		input, _ := json.Marshal(q.input)
		tr, err := rc.Tools.Execute(ctx, rc.Ticket.ID, q.tool, input)
		if err != nil {
			return nil, fmt.Errorf("log 工具调用失败: %w", err)
		}
		obs := ObservationFromToolResult(q.tool, tr)
		obs.Summary = q.summar(tr)
		id := rc.Board.AddObservation(obs)
		res.ObservationIDs = append(res.ObservationIDs, id)
		if obs.Status != tool.StatusOK {
			res.FailedCount++
		} else {
			res.Highlights = append(res.Highlights, obs.Summary)
		}
	}

	return &Output{
		Note:    fmt.Sprintf("日志检索：Pod 状态/事件/容器日志共 %d 项（失败 %d 项）", len(queries), res.FailedCount),
		Payload: res,
	}, nil
}

// podPrefixOf 服务名 → pod 名前缀（PreHire 命名约定 makejob-<svc>）。
func podPrefixOf(service string) string {
	if service == "" {
		return ""
	}
	return "makejob-" + service
}

const (
	k8sToolPods   = "k8s.get_pods"
	k8sToolEvents = "k8s.events"
	k8sToolLogs   = "k8s.logs"
)

// ---- 摘要器：把工具原始输出转成供 LLM 阅读的紧凑事实 ----

func summarizePods(tr *tool.Result) string {
	if tr.Status != tool.StatusOK {
		return "Pod 状态查询失败：" + tr.DegradedReason
	}
	var d struct {
		Pods []struct {
			Name      string `json:"name"`
			Phase     string `json:"phase"`
			Restarts  int32  `json:"restarts"`
			LastState string `json:"last_state"`
		} `json:"pods"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(tr.Data, &d); err != nil {
		return "Pod 状态查询完成，结果解析异常"
	}
	if d.Count == 0 {
		return "Pod 状态查询：当前无匹配 Pod（服务可能已缩容到 0 或已被删除）"
	}
	var parts []string
	for _, p := range d.Pods {
		part := fmt.Sprintf("%s phase=%s restarts=%d", p.Name, p.Phase, p.Restarts)
		if p.LastState != "" {
			part += " 上次退出=" + p.LastState
		}
		parts = append(parts, part)
	}
	return "Pod 状态查询（" + fmt.Sprint(d.Count) + " 个）：" + strings.Join(parts, "；")
}

func summarizeEvents(tr *tool.Result) string {
	if tr.Status != tool.StatusOK {
		return "事件查询失败：" + tr.DegradedReason
	}
	var d struct {
		Events []struct {
			Time    string `json:"time"`
			Type    string `json:"type"`
			Reason  string `json:"reason"`
			Object  string `json:"object"`
			Count   int32  `json:"count"`
			Message string `json:"message"`
		} `json:"events"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(tr.Data, &d); err != nil {
		return "事件查询完成，结果解析异常"
	}
	if d.Count == 0 {
		return "事件查询：时间窗内无相关事件"
	}
	var parts []string
	for i, e := range d.Events {
		if i >= 10 {
			parts = append(parts, fmt.Sprintf("（其余 %d 条略）", d.Count-10))
			break
		}
		parts = append(parts, fmt.Sprintf("[%s/%s] %s x%d: %s", e.Type, e.Reason, e.Object, e.Count, e.Message))
	}
	return fmt.Sprintf("事件查询（共 %d 条）：%s", d.Count, strings.Join(parts, "；"))
}

func summarizeLogs(tr *tool.Result) string {
	if tr.Status != tool.StatusOK {
		return "日志拉取失败：" + tr.DegradedReason
	}
	var d struct {
		Pods []struct {
			Pod  string `json:"pod"`
			Logs string `json:"logs"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(tr.Data, &d); err != nil {
		return "日志拉取完成，结果解析异常"
	}
	var out []string
	for _, p := range d.Pods {
		errLines, total := errorLines(p.Logs)
		out = append(out, fmt.Sprintf("%s: %d 行中 %d 行异常", p.Pod, total, errLines))
		for _, l := range sampleErrorLines(p.Logs, 5) {
			out = append(out, "  | "+l)
		}
	}
	return "容器日志：" + strings.Join(out, "；")
}

// errorLines 统计含错误关键词的行。
func errorLines(logs string) (errCount, total int) {
	for _, line := range strings.Split(logs, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		total++
		if containsAny(l, "error", "ERROR", "panic", "FATAL", "failed", "Failed", "timeout", "refused") {
			errCount++
		}
	}
	return
}

// sampleErrorLines 抽取最后 N 条异常行（截断）。
func sampleErrorLines(logs string, n int) []string {
	var samples []string
	for _, line := range strings.Split(logs, "\n") {
		l := strings.TrimSpace(line)
		if containsAny(l, "error", "ERROR", "panic", "FATAL", "failed", "Failed", "timeout", "refused") {
			samples = append(samples, truncLine(l, 160))
		}
	}
	if len(samples) > n {
		samples = samples[len(samples)-n:]
	}
	return samples
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func truncLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
