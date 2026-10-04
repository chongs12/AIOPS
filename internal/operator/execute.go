// Package operator 实现 Remediation CRD 的控制器：
// 处置闭环（审批门→执行→恢复回验）从应用内函数升级为 K8s 控制循环。
// 设计要点见 docs/adr/0003-remediation-operator.md。
package operator

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Actions 处置动作对集群的最小操作面。
// 收敛成窄接口：真实实现走 client-go，单测注入内存 fake，
// 避免 controller-runtime fake client 对 StrategicMergePatch 的支持差异。
type Actions interface {
	GetScale(ctx context.Context, name string, opts metav1.GetOptions) (*autoscalingv1.Scale, error)
	UpdateScale(ctx context.Context, name string, scale *autoscalingv1.Scale, opts metav1.UpdateOptions) (*autoscalingv1.Scale, error)
	GetDeployment(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error)
	ListReplicaSets(ctx context.Context, opts metav1.ListOptions) (*appsv1.ReplicaSetList, error)
	PatchDeployment(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error)
}

// k8sActions 基于 client-go 的真实实现。
type k8sActions struct {
	client    kubernetes.Interface
	namespace string
}

// NewK8sActions 构造真实执行层。
func NewK8sActions(client kubernetes.Interface, namespace string) Actions {
	return &k8sActions{client: client, namespace: namespace}
}

func (a *k8sActions) GetScale(ctx context.Context, name string, opts metav1.GetOptions) (*autoscalingv1.Scale, error) {
	return a.client.AppsV1().Deployments(a.namespace).GetScale(ctx, name, opts)
}

func (a *k8sActions) UpdateScale(ctx context.Context, name string, scale *autoscalingv1.Scale, opts metav1.UpdateOptions) (*autoscalingv1.Scale, error) {
	return a.client.AppsV1().Deployments(a.namespace).UpdateScale(ctx, name, scale, opts)
}

func (a *k8sActions) GetDeployment(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error) {
	return a.client.AppsV1().Deployments(a.namespace).Get(ctx, name, opts)
}

func (a *k8sActions) ListReplicaSets(ctx context.Context, opts metav1.ListOptions) (*appsv1.ReplicaSetList, error) {
	return a.client.AppsV1().ReplicaSets(a.namespace).List(ctx, opts)
}

func (a *k8sActions) PatchDeployment(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error) {
	return a.client.AppsV1().Deployments(a.namespace).Patch(ctx, name, pt, data, opts)
}

// ScaleResult 扩缩容结果。
type ScaleResult struct {
	From int32
	To   int32
}

// Scale 收敛副本数到目标值。scale 是期望态收敛（幂等）：
// 中断后重跑收敛到同一状态，安全。
func Scale(ctx context.Context, a Actions, name string, replicas int32) (ScaleResult, error) {
	s, err := a.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ScaleResult{}, fmt.Errorf("读取副本数: %w", err)
	}
	from := s.Spec.Replicas
	s.Spec.Replicas = replicas
	if _, err := a.UpdateScale(ctx, name, s, metav1.UpdateOptions{}); err != nil {
		return ScaleResult{}, fmt.Errorf("更新副本数: %w", err)
	}
	return ScaleResult{From: from, To: replicas}, nil
}

// RollbackResult 回滚收敛结果（revision 前后值）。
type RollbackResult struct {
	RevisionFrom int64
	RevisionTo   int64
}

const revisionAnnotation = "deployment.kubernetes.io/revision"

// PrevRevision 计算「当前 revision 与其上一 revision」。
// 在认领阶段调用，把回滚目标 RevisionTo 固化进 status——
// 这是回滚幂等化的关键：之后无论重放多少次，都只收敛到同一个目标。
func PrevRevision(ctx context.Context, a Actions, name string) (from, to int64, err error) {
	dep, err := a.GetDeployment(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("获取 deployment: %w", err)
	}
	rss, err := a.ListReplicaSets(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("列出 replicasets: %w", err)
	}
	cur, prev, err := prevReplicaSet(dep, rss)
	if err != nil {
		return 0, 0, err
	}
	return rsRevision(cur), rsRevision(prev), nil
}

// RollbackTo 把 Deployment 的 pod template 收敛到指定 revision 的 RS 模板
// （kubectl rollout undo 语义的目标态化改写）。
// 幂等：目标由 spec 化的 revisionTo 决定而非「相对上一版」，
// 中断重放收敛到同一状态，不存在二次回滚。
func RollbackTo(ctx context.Context, a Actions, name string, revisionTo int64) (RollbackResult, error) {
	dep, err := a.GetDeployment(ctx, name, metav1.GetOptions{})
	if err != nil {
		return RollbackResult{}, fmt.Errorf("获取 deployment: %w", err)
	}
	rss, err := a.ListReplicaSets(ctx, metav1.ListOptions{})
	if err != nil {
		return RollbackResult{}, fmt.Errorf("列出 replicasets: %w", err)
	}
	cur, _, err := prevReplicaSet(dep, rss) // cur 为 revision 最高的 RS
	if err != nil {
		return RollbackResult{}, err
	}
	var target *appsv1.ReplicaSet
	for i := range rss.Items {
		rs := &rss.Items[i]
		if !ownedBy(rs, dep) {
			continue
		}
		if rsRevision(rs) == revisionTo {
			target = rs
			break
		}
	}
	if target == nil {
		return RollbackResult{}, fmt.Errorf("未找到 revision=%d 的历史 ReplicaSet", revisionTo)
	}
	patch := map[string]any{
		"spec": map[string]any{"template": target.Spec.Template},
	}
	pb, err := jsonMarshal(patch)
	if err != nil {
		return RollbackResult{}, err
	}
	if _, err := a.PatchDeployment(ctx, name, types.StrategicMergePatchType, pb, metav1.PatchOptions{}); err != nil {
		return RollbackResult{}, fmt.Errorf("回滚补丁失败: %w", err)
	}
	return RollbackResult{RevisionFrom: rsRevision(cur), RevisionTo: revisionTo}, nil
}

// ownedBy 判断 RS 是否由该 Deployment 拥有。
func ownedBy(rs *appsv1.ReplicaSet, dep *appsv1.Deployment) bool {
	for _, ref := range rs.OwnerReferences {
		if ref.UID == dep.UID {
			return true
		}
	}
	return false
}

// rsRevision 读取 RS 的 revision 注解（缺失为 0）。
func rsRevision(rs *appsv1.ReplicaSet) int64 {
	n, _ := strconv.ParseInt(rs.Annotations[revisionAnnotation], 10, 64)
	return n
}

// prevReplicaSet 取该 Deployment 拥有的 RS 中 revision 最高的两个：
// [0]=当前，[1]=上一版本。不足两个视为无历史可回滚。
func prevReplicaSet(dep *appsv1.Deployment, rss *appsv1.ReplicaSetList) (cur, prev *appsv1.ReplicaSet, err error) {
	var owned []appsv1.ReplicaSet
	for i := range rss.Items {
		if ownedBy(&rss.Items[i], dep) {
			owned = append(owned, rss.Items[i])
		}
	}
	if len(owned) < 2 {
		return nil, nil, fmt.Errorf("无历史 revision 可回滚")
	}
	sort.Slice(owned, func(i, j int) bool { return rsRevision(&owned[i]) > rsRevision(&owned[j]) })
	return &owned[0], &owned[1], nil
}
