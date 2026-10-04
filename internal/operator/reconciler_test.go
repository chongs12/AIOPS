package operator

// Reconciler 单测：用 controller-runtime fake client + 内存 fake 执行层/回验层，
// 覆盖审批门、收敛式幂等执行、回验通过/重查/耗尽转人工。

import (
	"context"
	"strconv"
	"testing"
	"time"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/internal/apis/scheme"
	"aiops/pkg/config"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNS   = "default"
	testName = "rem-t1"
)

// fakeActions 内存执行层：记录调用次数，验证「不重复执行 / 收敛到同一目标」。
type fakeActions struct {
	scaleCalls int
	patchCalls int
	replicas   map[string]int32 // Deployment 当前副本数
	curRev     int64            // 当前 revision（RS 注解模拟）
	prevRev    int64            // 上一 revision
	scaleErr   error
	patchErr   error
}

func newFakeActions() *fakeActions {
	return &fakeActions{replicas: map[string]int32{}, curRev: 3, prevRev: 2}
}

func (f *fakeActions) GetScale(_ context.Context, name string, _ metav1.GetOptions) (*autoscalingv1.Scale, error) {
	if f.scaleErr != nil {
		return nil, f.scaleErr
	}
	return &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: autoscalingv1.ScaleSpec{Replicas: f.replicas[name]}}, nil
}

func (f *fakeActions) UpdateScale(_ context.Context, name string, s *autoscalingv1.Scale, _ metav1.UpdateOptions) (*autoscalingv1.Scale, error) {
	if f.scaleErr != nil {
		return nil, f.scaleErr
	}
	f.scaleCalls++
	f.replicas[name] = s.Spec.Replicas
	return s, nil
}

func (f *fakeActions) GetDeployment(_ context.Context, name string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "dep-uid"}}, nil
}

func (f *fakeActions) ListReplicaSets(_ context.Context, _ metav1.ListOptions) (*appsv1.ReplicaSetList, error) {
	rev := func(n string, rev int64, owner string) appsv1.ReplicaSet {
		return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name:            n,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": strconv.FormatInt(rev, 10)},
			OwnerReferences: []metav1.OwnerReference{{UID: types.UID(owner)}},
		}}
	}
	return &appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
		rev("rs-cur", f.curRev, "dep-uid"),
		rev("rs-prev", f.prevRev, "dep-uid"),
		rev("rs-other", f.curRev+5, "other-uid"), // 干扰项：验证 owner 过滤
	}}, nil
}

func (f *fakeActions) PatchDeployment(_ context.Context, name string, _ types.PatchType, _ []byte, _ metav1.PatchOptions) (*appsv1.Deployment, error) {
	if f.patchErr != nil {
		return nil, f.patchErr
	}
	f.patchCalls++
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "dep-uid"}}, nil
}

// fakeVerifier 可编程回验结果序列。
type fakeVerifier struct {
	values []float64 // 第 n 次查询返回 values[n-1]；越界取最后一个
	err    error
	calls  int
}

func (f *fakeVerifier) Query(_ context.Context, _ string, _ map[string]string) (float64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	if len(f.values) == 0 {
		return 0, nil
	}
	if f.calls-1 >= len(f.values) {
		return f.values[len(f.values)-1], nil
	}
	return f.values[f.calls-1], nil
}

// testClock 可控时钟。
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// ---- 测试夹具 ----

func scaleCR(approved bool) *remediationv1.Remediation {
	replicas := int32(5)
	rem := &remediationv1.Remediation{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
		Spec: remediationv1.RemediationSpec{
			TicketRef: "t-20260101-000000-ab", Deployment: "demo-app",
			Action: remediationv1.ActionScale, Replicas: &replicas, Reason: "5xx 突增",
		},
	}
	if approved {
		rem.Status.Approval = &remediationv1.ApprovalStatus{Approved: true, Approver: "alice"}
	}
	return rem
}

func undoCR(approved bool) *remediationv1.Remediation {
	rem := &remediationv1.Remediation{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
		Spec: remediationv1.RemediationSpec{
			TicketRef: "t-20260101-000000-ab", Deployment: "demo-app",
			Action: remediationv1.ActionUndo, Reason: "坏版本回滚",
		},
	}
	if approved {
		rem.Status.Approval = &remediationv1.ApprovalStatus{Approved: true, Approver: "bob"}
	}
	return rem
}

func newRig(t *testing.T, rem *remediationv1.Remediation) (*Reconciler, *fakeActions, *fakeVerifier, *testClock) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&remediationv1.Remediation{}).
		WithObjects(rem).
		Build()
	fa, fv, clk := newFakeActions(), &fakeVerifier{values: []float64{0.01}}, &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := NewReconciler(c, scheme.Scheme, record.NewFakeRecorder(64), fa, fv,
		config.OperatorVerify{Template: "http_5xx_rate", Threshold: 0.05, WaitSeconds: 60, Attempts: 3}, testNS)
	r.Now = clk.Now
	return r, fa, fv, clk
}

func do(t *testing.T, r *Reconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: testName}})
	if err != nil {
		t.Fatalf("reconcile 出错: %v", err)
	}
	return res
}

func getRem(t *testing.T, r *Reconciler) *remediationv1.Remediation {
	t.Helper()
	var rem remediationv1.Remediation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: testName}, &rem); err != nil {
		t.Fatalf("读取 CR: %v", err)
	}
	return &rem
}

// ---- 用例 ----

func TestPendingApproval_InitAndWaits(t *testing.T) {
	r, fa, _, _ := newRig(t, scaleCR(false))

	do(t, r)
	if got := getRem(t, r).Status.Phase; got != remediationv1.PhasePendingApproval {
		t.Fatalf("初始 reconcile 应进入 PendingApproval，got %s", got)
	}
	if fa.scaleCalls != 0 {
		t.Fatalf("未审批不应执行动作")
	}
	do(t, r) // 重复 reconcile 幂等
	if fa.scaleCalls != 0 {
		t.Fatalf("等待审批期间不应执行动作")
	}
}

func TestRejected_Terminal(t *testing.T) {
	rem := scaleCR(false)
	rem.Status.Phase = remediationv1.PhasePendingApproval
	rem.Status.Approval = &remediationv1.ApprovalStatus{Approved: false, Approver: "alice", Note: "副本数不合理"}
	r, fa, _, _ := newRig(t, rem)

	do(t, r)
	got := getRem(t, r)
	if got.Status.Phase != remediationv1.PhaseRejected {
		t.Fatalf("拒绝应进入 Rejected，got %s", got.Status.Phase)
	}
	if fa.scaleCalls != 0 {
		t.Fatalf("拒绝后不应执行动作")
	}
}

func TestApprovedScale_FullLifecycle(t *testing.T) {
	r, fa, fv, clk := newRig(t, scaleCR(true))

	// 1) 认领
	do(t, r)
	rem := getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseExecuting || rem.Status.ObservedGeneration != rem.Generation {
		t.Fatalf("审批后应认领（Executing + observedGeneration 闸），got phase=%s", rem.Status.Phase)
	}
	if fa.scaleCalls != 0 {
		t.Fatalf("认领轮不应执行动作")
	}

	// 2) 执行 → Verifying
	do(t, r)
	rem = getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseVerifying {
		t.Fatalf("执行后应进入 Verifying，got %s（msg=%s）", rem.Status.Phase, rem.Status.Message)
	}
	if fa.scaleCalls != 1 || rem.Status.Execution.ToReplicas == nil || *rem.Status.Execution.ToReplicas != 5 {
		t.Fatalf("scale 应执行一次并收敛到 5 副本: calls=%d status=%+v", fa.scaleCalls, rem.Status.Execution)
	}

	// 3) 观察窗内不重复执行
	res := do(t, r)
	if fa.scaleCalls != 1 {
		t.Fatalf("观察窗内不应重复执行")
	}
	if res.RequeueAfter != 60*time.Second {
		t.Fatalf("观察窗应 RequeueAfter=60s，got %v", res.RequeueAfter)
	}

	// 4) 回验通过 → Succeeded
	clk.Advance(61 * time.Second)
	do(t, r)
	rem = getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseSucceeded {
		t.Fatalf("回验通过应 Succeeded，got %s（msg=%s）", rem.Status.Phase, rem.Status.Message)
	}
	if fv.calls != 1 || rem.Status.Verification.Result != "passed" {
		t.Fatalf("回验应恰好查询一次且 passed: calls=%d vs=%+v", fv.calls, rem.Status.Verification)
	}
	if !hasCondition(rem, remediationv1.ConditionVerified, metav1.ConditionTrue) {
		t.Fatalf("应有 Verified=True condition: %+v", rem.Status.Conditions)
	}
}

func TestVerify_EscalatesAfterAttemptsExhausted(t *testing.T) {
	r, _, fv, clk := newRig(t, scaleCR(true))
	fv.values = []float64{0.50, 0.40, 0.30} // 始终未达标

	do(t, r) // 认领
	do(t, r) // 执行 → Verifying
	for i := 0; i < 3; i++ {
		clk.Advance(61 * time.Second)
		do(t, r)
	}
	rem := getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseEscalated {
		t.Fatalf("3 次回验未达标应 Escalated，got %s（msg=%s）", rem.Status.Phase, rem.Status.Message)
	}
	if fv.calls != 3 {
		t.Fatalf("应恰好查询 3 次，got %d", fv.calls)
	}
	if rem.Status.Verification.Attempts != 0 {
		t.Fatalf("attempts 应耗尽，got %d", rem.Status.Verification.Attempts)
	}
}

func TestScale_CrashReplayConverges(t *testing.T) {
	r, fa, _, _ := newRig(t, scaleCR(true))
	fa.replicas["demo-app"] = 1

	do(t, r) // 认领
	// 模拟「执行成功但结果未落库即崩溃」：把 CompletedAt 抹掉，重放 reconcile。
	// scale 是期望态收敛，重放后副本数仍收敛到 5（不叠加、不翻倍）。
	do(t, r) // 执行 → Verifying, replicas=5
	rem := getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseVerifying || fa.scaleCalls != 1 {
		t.Fatalf("前置状态异常: phase=%s calls=%d", rem.Status.Phase, fa.scaleCalls)
	}
	do(t, r) // 再重放一轮（模拟 crash 后重复 reconcile）
	if fa.scaleCalls != 1 {
		t.Fatalf("重放不应重复执行: calls=%d", fa.scaleCalls)
	}
	if fa.replicas["demo-app"] != 5 {
		t.Fatalf("副本数应收敛到 5，got %d", fa.replicas["demo-app"])
	}
}

func TestUndo_ClaimFixesTarget_AndReplayStaysOnTarget(t *testing.T) {
	r, fa, _, clk := newRig(t, undoCR(true))

	// 1) 认领：固化回滚目标 revision（当前 3 → 上一 2）
	do(t, r)
	rem := getRem(t, r)
	if rem.Status.Execution == nil || rem.Status.Execution.RevisionTo == nil || *rem.Status.Execution.RevisionTo != 2 {
		t.Fatalf("认领应固化 RevisionTo=2，got %+v", rem.Status.Execution)
	}
	if fa.patchCalls != 0 {
		t.Fatalf("认领轮不应打补丁")
	}

	// 2) 执行 → 收敛到 revision 2
	do(t, r)
	rem = getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseVerifying || fa.patchCalls != 1 {
		t.Fatalf("undo 应执行一次收敛到 revision 2: phase=%s patchCalls=%d", rem.Status.Phase, fa.patchCalls)
	}
	if *rem.Status.Execution.RevisionFrom != 3 || *rem.Status.Execution.RevisionTo != 2 {
		t.Fatalf("revision 记录错误: %+v", rem.Status.Execution)
	}

	// 3) 崩溃重放：模拟「补丁已打上但结果未落库即崩溃」——phase 停留在
	//    Executing 且 CompletedAt=nil。重放后仍收敛到 revision 2，
	//    绝不会因为「重算上一版」而滚到 revision 1——这是收敛式回滚的核心保证。
	rem2 := getRem(t, r)
	rem2.Status.Phase = remediationv1.PhaseExecuting
	rem2.Status.Execution.CompletedAt = nil
	if err := r.Status().Update(context.Background(), rem2); err != nil {
		t.Fatalf("构造崩溃态: %v", err)
	}
	do(t, r)
	rem = getRem(t, r)
	if fa.patchCalls != 2 || rem.Status.Phase != remediationv1.PhaseVerifying || *rem.Status.Execution.RevisionTo != 2 {
		t.Fatalf("崩溃重放应再收敛一次且目标仍是 revision 2: calls=%d phase=%s to=%v",
			fa.patchCalls, rem.Status.Phase, rem.Status.Execution.RevisionTo)
	}
	clk.Advance(61 * time.Second)
	do(t, r)
	if got := getRem(t, r).Status.Phase; got != remediationv1.PhaseSucceeded {
		t.Fatalf("回滚后回验通过应 Succeeded，got %s", got)
	}
}

func TestScale_MissingReplicasFails(t *testing.T) {
	rem := scaleCR(true)
	rem.Spec.Replicas = nil
	r, fa, _, _ := newRig(t, rem)

	do(t, r) // 认领
	do(t, r) // 执行 → 参数缺失应 Failed
	got := getRem(t, r)
	if got.Status.Phase != remediationv1.PhaseFailed {
		t.Fatalf("缺 replicas 应 Failed，got %s", got.Status.Phase)
	}
	if fa.scaleCalls != 0 {
		t.Fatalf("参数缺失不应触达集群")
	}
}

func TestExecuteError_TerminalFailed_NoRetry(t *testing.T) {
	rem := scaleCR(true)
	r, fa, _, _ := newRig(t, rem)
	fa.scaleErr = context.DeadlineExceeded

	do(t, r) // 认领
	do(t, r) // 执行 → 集群错误
	got := getRem(t, r)
	if got.Status.Phase != remediationv1.PhaseFailed {
		t.Fatalf("执行失败应 Failed（变更语义不自动重试），got %s", got.Status.Phase)
	}
	if fa.scaleCalls != 0 { // UpdateScale 前的 GetScale 已失败
		t.Fatalf("失败路径不应有成功执行")
	}
	do(t, r) // 终态：再 reconcile 不再有任何动作
	if got = getRem(t, r); got.Status.Phase != remediationv1.PhaseFailed {
		t.Fatalf("终态应保持 Failed，got %s", got.Status.Phase)
	}
}

func TestVerifyError_BurnsAttemptsThenEscalates(t *testing.T) {
	r, _, fv, clk := newRig(t, scaleCR(true))
	fv.err = context.DeadlineExceeded // Prometheus 不可用

	do(t, r)
	do(t, r) // → Verifying
	for i := 0; i < 3; i++ {
		clk.Advance(61 * time.Second)
		do(t, r)
	}
	rem := getRem(t, r)
	if rem.Status.Phase != remediationv1.PhaseEscalated {
		t.Fatalf("回验连续失败应 Escalated（有界自愈），got %s", rem.Status.Phase)
	}
	if rem.Status.Verification.Result != "error" {
		t.Fatalf("回验结果应记录 error，got %s", rem.Status.Verification.Result)
	}
}

func hasCondition(rem *remediationv1.Remediation, condType string, status metav1.ConditionStatus) bool {
	for _, c := range rem.Status.Conditions {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}
