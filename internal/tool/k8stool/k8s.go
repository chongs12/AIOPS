// Package k8stool 提供 K8s 工具：容器日志 / 事件 / Pod 状态（只读）与
// 扩缩容 / 回滚（变更类，需人工确认）。基于 client-go，复用 kubeconfig/in-cluster。
package k8stool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Base K8s 工具公共依赖。
type Base struct {
	Client    *kubernetes.Clientset
	Namespace string
	Timeout   time.Duration
}

// findPods 按名称前缀/全部列出匹配 pod（排序保证稳定）。
func (b *Base) findPods(ctx context.Context, prefix string) ([]v1.Pod, error) {
	list, err := b.Client.CoreV1().Pods(b.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("列出 pod 失败: %w", err)
	}
	var out []v1.Pod
	for _, p := range list.Items {
		if prefix == "" || strings.HasPrefix(p.Name, prefix) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LogsInput k8s.logs 输入。
type LogsInput struct {
	PodPrefix    string `json:"pod_prefix"` // pod 名前缀，如 makejob-question
	Container    string `json:"container,omitempty"`
	TailLines    int64  `json:"tail_lines,omitempty"`    // 默认 200
	SinceMinutes int64  `json:"since_minutes,omitempty"` // 默认 30
	MaxBytes     int64  `json:"-"`                       // 输出截断
}

// Logs 拉取匹配 pod 的最近容器日志（多 pod 合并，按 pod 分节）。
func (b *Base) Logs(ctx context.Context, in LogsInput) (json.RawMessage, error) {
	if in.PodPrefix == "" {
		return nil, fmt.Errorf("pod_prefix 必填")
	}
	if in.TailLines <= 0 {
		in.TailLines = 200
	}
	if in.SinceMinutes <= 0 {
		in.SinceMinutes = 30
	}
	if in.MaxBytes <= 0 {
		in.MaxBytes = 12 << 10
	}
	pods, err := b.findPods(ctx, in.PodPrefix)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return json.RawMessage(`{"pods":[],"note":"未找到匹配的 pod（可能已被删除或尚未创建）"}`), nil
	}
	since := time.Duration(in.SinceMinutes) * time.Minute
	type podLog struct {
		Pod   string `json:"pod"`
		Logs  string `json:"logs"`
		Bytes int64  `json:"bytes"`
	}
	var out []podLog
	for _, p := range pods {
		lctx, cancel := context.WithTimeout(ctx, b.Timeout)
		req := b.Client.CoreV1().Pods(b.Namespace).GetLogs(p.Name, &v1.PodLogOptions{
			Container:    in.Container,
			TailLines:    &in.TailLines,
			SinceSeconds: int64Ptr(int64(since.Seconds())),
			Timestamps:   true,
		})
		raw, err := req.DoRaw(lctx)
		cancel()
		if err != nil {
			out = append(out, podLog{Pod: p.Name, Logs: "(日志获取失败: " + trunc(err.Error(), 120) + ")"})
			continue
		}
		text := string(raw)
		out = append(out, podLog{Pod: p.Name, Logs: trunc(text, int(in.MaxBytes)), Bytes: int64(len(text))})
	}
	return json.Marshal(map[string]any{"namespace": b.Namespace, "pods": out})
}

// EventsInput k8s.events 输入。
type EventsInput struct {
	PodPrefix    string `json:"pod_prefix,omitempty"`
	SinceMinutes int64  `json:"since_minutes,omitempty"` // 默认 60
}

// Events 拉取命名空间事件（可按 pod 前缀过滤），根因关键信号源
// （OOMKilled / BackOff / FailedScheduling / 探针失败等）。
func (b *Base) Events(ctx context.Context, in EventsInput) (json.RawMessage, error) {
	if in.SinceMinutes <= 0 {
		in.SinceMinutes = 60
	}
	list, err := b.Client.CoreV1().Events(b.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("列出事件失败: %w", err)
	}
	cutoff := time.Now().Add(-time.Duration(in.SinceMinutes) * time.Minute)
	type ev struct {
		Time    string `json:"time"`
		Type    string `json:"type"`
		Reason  string `json:"reason"`
		Object  string `json:"object"`
		Count   int32  `json:"count"`
		Message string `json:"message"`
	}
	var out []ev
	for _, e := range list.Items {
		ts := e.LastTimestamp.Time
		if ts.IsZero() {
			ts = e.EventTime.Time
		}
		if !ts.IsZero() && ts.Before(cutoff) {
			continue
		}
		if in.PodPrefix != "" && !strings.HasPrefix(e.InvolvedObject.Name, in.PodPrefix) {
			continue
		}
		out = append(out, ev{
			Time: ts.Format(time.RFC3339), Type: e.Type, Reason: e.Reason,
			Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Count:  e.Count, Message: trunc(e.Message, 300),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return json.Marshal(map[string]any{"namespace": b.Namespace, "events": out, "count": len(out)})
}

// PodsInput k8s.get_pods 输入。
type PodsInput struct {
	PodPrefix string `json:"pod_prefix,omitempty"`
}

// podView Pod 状态视图：含重启数与上次退出原因（OOMKilled 等关键信号）。
type podView struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Restarts  int32  `json:"restarts"`
	LastState string `json:"last_state,omitempty"`
	Node      string `json:"node,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// Pods 列出 pod 状态（含 OOMKilled / CrashLoopBackOff 信号）。
func (b *Base) Pods(ctx context.Context, in PodsInput) (json.RawMessage, error) {
	pods, err := b.findPods(ctx, in.PodPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]podView, 0, len(pods))
	for _, p := range pods {
		v := podView{Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName}
		for _, cs := range p.Status.ContainerStatuses {
			v.Restarts += cs.RestartCount
			if ls := cs.LastTerminationState.Terminated; ls != nil {
				v.LastState = fmt.Sprintf("%s(exit=%d)", ls.Reason, ls.ExitCode)
			}
			if cs.State.Running != nil {
				v.StartedAt = cs.State.Running.StartedAt.Format(time.RFC3339)
			}
		}
		out = append(out, v)
	}
	return json.Marshal(map[string]any{"namespace": b.Namespace, "pods": out, "count": len(out)})
}

func int64Ptr(i int64) *int64 { return &i }

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
