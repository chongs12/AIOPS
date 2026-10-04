package k8stool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// k8sRollbackRequest 构造 DeploymentRollback 请求体（走 RS 历史回写 template，
// 与 kubectl rollout undo 语义一致）。
type rollbackSpec struct {
	deployment string
}

func k8sRollbackRequest(deployment string) rollbackSpec { return rollbackSpec{deployment: deployment} }

// Rollback 通过「取上一 revision 的 ReplicaSet 模板回写 Deployment」实现回滚。
// （client-go 新版本已移除 Rollback API。）
// 返回回滚到的目标 revision，供结果数据与审计/回验记录。
func (b *Base) Rollback(ctx context.Context, spec rollbackSpec) (int64, error) {
	dep, err := b.Client.AppsV1().Deployments(b.Namespace).Get(ctx, spec.deployment, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("获取 deployment: %w", err)
	}
	rss, err := b.Client.AppsV1().ReplicaSets(b.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("列出 replicasets: %w", err)
	}
	var owned []appsv1.ReplicaSet
	for _, rs := range rss.Items {
		for _, ref := range rs.OwnerReferences {
			if ref.UID == dep.UID {
				owned = append(owned, rs)
				break
			}
		}
	}
	if len(owned) < 2 {
		return 0, fmt.Errorf("无历史 revision 可回滚")
	}
	revOf := func(rs appsv1.ReplicaSet) int64 {
		n, _ := strconv.ParseInt(rs.Annotations["deployment.kubernetes.io/revision"], 10, 64)
		return n
	}
	sort.Slice(owned, func(i, j int) bool { return revOf(owned[i]) > revOf(owned[j]) })
	prev := owned[1] // [0] 为当前 revision
	patch := map[string]any{
		"spec": map[string]any{"template": prev.Spec.Template},
	}
	pb, err := json.Marshal(patch)
	if err != nil {
		return 0, err
	}
	if _, err := b.Client.AppsV1().Deployments(b.Namespace).Patch(ctx, spec.deployment,
		types.StrategicMergePatchType, pb, metav1.PatchOptions{}); err != nil {
		return 0, fmt.Errorf("回滚补丁失败: %w", err)
	}
	return revOf(prev), nil
}

// mustJSON 序列化辅助。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"marshal failed"}`)
	}
	return b
}

// compact 紧凑输入摘要（证据 query 用）。
func compact(in json.RawMessage) string {
	s := string(in)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// excerptOf 生成证据摘录（必须整体合法 JSON：大载荷用摘要替代字节截断）。
func excerptOf(data any) json.RawMessage {
	b, err := json.Marshal(data)
	if err != nil || len(b) == 0 {
		return nil
	}
	if len(b) <= 1024 {
		return b
	}
	summary, err := json.Marshal(map[string]any{
		"note":  "excerpt omitted: payload too large, 全量数据见 data 字段",
		"bytes": len(b),
	})
	if err != nil {
		return nil
	}
	return summary
}
