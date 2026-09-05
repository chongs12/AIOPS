package k8stool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"aiops/internal/tool"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 工具名常量。
const (
	NameLogs   = "k8s.logs"
	NameEvents = "k8s.events"
	NamePods   = "k8s.get_pods"
	NameScale  = "k8s.scale"
	NameUndo   = "k8s.rollout_undo"
)

// NewReadTools 构造只读工具集（logs/events/get_pods）。
func NewReadTools(b Base) []tool.Tool {
	return []tool.Tool{
		&readTool{base: b, name: NameLogs, desc: "拉取指定服务 Pod 的最近容器日志（时间窗内，按 Pod 分节）",
			input: json.RawMessage(`{"type":"object","required":["pod_prefix"],"properties":{
				"pod_prefix":{"type":"string","description":"Pod 名前缀，如 makejob-question"},
				"container":{"type":"string"},"tail_lines":{"type":"number"},"since_minutes":{"type":"number"}}}`),
			run: func(ctx context.Context, b Base, in json.RawMessage) (any, error) {
				var p LogsInput
				if err := json.Unmarshal(in, &p); err != nil {
					return nil, err
				}
				return b.Logs(ctx, p)
			}},
		&readTool{base: b, name: NameEvents, desc: "拉取命名空间 K8s 事件（OOMKilled/BackOff/探针失败等根因关键信号）",
			input: json.RawMessage(`{"type":"object","properties":{
				"pod_prefix":{"type":"string"},"since_minutes":{"type":"number"}}}`),
			run: func(ctx context.Context, b Base, in json.RawMessage) (any, error) {
				var p EventsInput
				if err := json.Unmarshal(in, &p); err != nil {
					return nil, err
				}
				return b.Events(ctx, p)
			}},
		&readTool{base: b, name: NamePods, desc: "列出 Pod 状态（重启次数、上次退出原因如 OOMKilled、节点）",
			input: json.RawMessage(`{"type":"object","properties":{"pod_prefix":{"type":"string"}}}`),
			run: func(ctx context.Context, b Base, in json.RawMessage) (any, error) {
				var p PodsInput
				if err := json.Unmarshal(in, &p); err != nil {
					return nil, err
				}
				return b.Pods(ctx, p)
			}},
	}
}

// readTool 只读工具统一实现。
type readTool struct {
	base  Base
	name  string
	desc  string
	input json.RawMessage
	run   func(ctx context.Context, b Base, in json.RawMessage) (any, error)
}

func (t *readTool) Name() string { return t.name }
func (t *readTool) Desc() string { return t.desc }
func (t *readTool) Schema() tool.Schema {
	return tool.Schema{
		Input: t.input, Idempotent: true, MaxRetries: 1,
		Timeout: 8 * time.Second, Concurrency: 4,
	}
}

func (t *readTool) Execute(ctx context.Context, in json.RawMessage) (*tool.Result, error) {
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	data, err := t.run(ctx, t.base, in)
	if err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("k8s 查询不可用: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "kubernetes"},
		}, nil
	}
	return &tool.Result{
		Status: tool.StatusOK,
		Data:   mustJSON(data),
		Evidence: tool.EvidenceMeta{
			Source:  "kubernetes",
			Query:   t.name + " " + compact(in),
			Excerpt: excerptOf(data),
		},
	}, nil
}

// ScaleInput k8s.scale 输入。
type ScaleInput struct {
	Deployment string `json:"deployment"`
	Replicas   int32  `json:"replicas"`
	Confirmed  bool   `json:"confirmed"`
}

// UndoInput k8s.rollout_undo 输入。
type UndoInput struct {
	Deployment string `json:"deployment"`
	Confirmed  bool   `json:"confirmed"`
}

// NewMutatingTools 构造变更类工具（扩缩容/回滚）。
// M3 语义：confirmed=false 时返回 failed 信封要求人工确认，绝不执行。
func NewMutatingTools(b Base) []tool.Tool {
	return []tool.Tool{
		&mutatingTool{base: b, name: NameScale,
			desc: "扩缩容 Deployment 副本数（变更操作，confirmed=true 才执行）",
			input: json.RawMessage(`{"type":"object","required":["deployment","replicas"],"properties":{
				"deployment":{"type":"string"},"replicas":{"type":"number"},"confirmed":{"type":"boolean"}}}`),
			run: func(ctx context.Context, b Base, in json.RawMessage) (any, error) {
				var p ScaleInput
				if err := json.Unmarshal(in, &p); err != nil {
					return nil, err
				}
				if !p.Confirmed {
					return nil, errNeedsConfirm
				}
				s, err := b.Client.AppsV1().Deployments(b.Namespace).GetScale(ctx, p.Deployment, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				s.Spec.Replicas = p.Replicas
				_, err = b.Client.AppsV1().Deployments(b.Namespace).UpdateScale(ctx, p.Deployment, s, metav1.UpdateOptions{})
				return map[string]any{"deployment": p.Deployment, "replicas": p.Replicas, "note": "已执行扩缩容"}, err
			}},
		&mutatingTool{base: b, name: NameUndo,
			desc: "回滚 Deployment 到上一版本（变更操作，confirmed=true 才执行）",
			input: json.RawMessage(`{"type":"object","required":["deployment"],"properties":{
				"deployment":{"type":"string"},"confirmed":{"type":"boolean"}}}`),
			run: func(ctx context.Context, b Base, in json.RawMessage) (any, error) {
				var p UndoInput
				if err := json.Unmarshal(in, &p); err != nil {
					return nil, err
				}
				if !p.Confirmed {
					return nil, errNeedsConfirm
				}
				// 取上一 revision 的 RS 模板回写（kubectl rollout undo 语义）。
				if err := b.Rollback(ctx, k8sRollbackRequest(p.Deployment)); err != nil {
					return nil, err
				}
				return map[string]any{"deployment": p.Deployment, "note": "已回滚到上一 revision"}, nil
			}},
	}
}

var errNeedsConfirm = fmt.Errorf("变更类操作需要人工确认（confirmed=true）")

// mutatingTool 变更工具统一实现：不幂等、不自动重试。
type mutatingTool struct {
	base  Base
	name  string
	desc  string
	input json.RawMessage
	run   func(ctx context.Context, b Base, in json.RawMessage) (any, error)
}

func (t *mutatingTool) Name() string { return t.name }
func (t *mutatingTool) Desc() string { return t.desc }
func (t *mutatingTool) Schema() tool.Schema {
	return tool.Schema{
		Input: t.input, Idempotent: false, Mutating: true,
		Timeout: 20 * time.Second,
	}
}

func (t *mutatingTool) Execute(ctx context.Context, in json.RawMessage) (*tool.Result, error) {
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	data, err := t.run(ctx, t.base, in)
	if err == errNeedsConfirm {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: "needs_confirm: 变更类操作必须经人工二次确认（confirmed=true）后执行",
			Evidence:       tool.EvidenceMeta{Source: "kubernetes"},
		}, nil
	}
	if err != nil {
		return &tool.Result{
			Status:         tool.StatusFailed,
			DegradedReason: fmt.Sprintf("变更执行失败: %v", err),
			Evidence:       tool.EvidenceMeta{Source: "kubernetes"},
		}, nil
	}
	return &tool.Result{
		Status:   tool.StatusOK,
		Data:     mustJSON(data),
		Evidence: tool.EvidenceMeta{Source: "kubernetes", Query: t.name + " " + compact(in)},
	}, nil
}
