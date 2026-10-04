package operator

import (
	"context"
	"fmt"
	"time"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/pkg/config"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Reconciler Remediation 控制循环。
//
// 认领协议（claim protocol）——变更动作「只执行一次」不是靠锁，而是靠
// 把执行进度落成 status 的三段式：认领（Executing+observedGeneration 闸）
// → 标记进行中（Attempted=true）→ 写结果（CompletedAt）。任意一步崩溃，
// 重启后的 reconcile 都能从 status 判断推进到哪一步：
//   - scale 是期望态收敛，重复执行安全，直接重跑；
//   - rollback 不幂等（重跑会继续往前回滚），结果未知时宁可置 Failed
//     转人工核对 revision，也不盲跑。
type Reconciler struct {
	client.Client

	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Actions   Actions
	Verifier  Verifier
	Defaults  config.OperatorVerify
	Namespace string
	// Now 测试时钟注入点；生产为 time.Now。
	Now func() time.Time

	metrics *opMetrics
}

// NewReconciler 构造控制器。
func NewReconciler(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder,
	actions Actions, verifier Verifier, defaults config.OperatorVerify, ns string) *Reconciler {
	return &Reconciler{
		Client: c, Scheme: scheme, Recorder: recorder,
		Actions: actions, Verifier: verifier,
		Defaults: defaults, Namespace: ns,
		Now:     time.Now,
		metrics: newMetrics(),
	}
}

// isTerminal 终态：不再参与 reconcile（watch 事件到达也直接返回）。
func isTerminal(phase string) bool {
	switch phase {
	case remediationv1.PhaseSucceeded, remediationv1.PhaseFailed,
		remediationv1.PhaseEscalated, remediationv1.PhaseRejected:
		return true
	}
	return false
}

// Reconcile 主状态机：按 status.phase 分派。
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	logger := log.FromContext(ctx).WithValues("remediation", req.NamespacedName)

	var rem remediationv1.Remediation
	if err := r.Get(ctx, req.NamespacedName, &rem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if isTerminal(rem.Status.Phase) {
		return ctrl.Result{}, nil
	}

	var res ctrl.Result
	var err error
	switch rem.Status.Phase {
	case remediationv1.PhaseExecuting:
		res, err = r.reconcileExecuting(ctx, &rem)
	case remediationv1.PhaseVerifying:
		res, err = r.reconcileVerifying(ctx, &rem)
	default: // ""（新建）与 PendingApproval
		res, err = r.reconcilePending(ctx, &rem)
	}
	r.metrics.transition(rem.Spec.Action, rem.Status.Phase, time.Since(start))
	if err != nil {
		logger.Error(err, "reconcile 失败，等待重试", "phase", rem.Status.Phase)
	}
	return res, err
}

// reconcilePending 初始化 / 等待审批 / 拒绝 / 已批准→认领执行权。
func (r *Reconciler) reconcilePending(ctx context.Context, rem *remediationv1.Remediation) (ctrl.Result, error) {
	ap := rem.Status.Approval
	if ap == nil {
		if rem.Status.Phase == remediationv1.PhasePendingApproval {
			return ctrl.Result{}, nil // 已初始化，等待审批（审批写入 status 会再触发 reconcile）
		}
		rem.Status.Phase = remediationv1.PhasePendingApproval
		setCondition(rem, remediationv1.ConditionApproved, metav1.ConditionFalse,
			remediationv1.ReasonWaitingApproval, "处置待人工审批；拒绝请置 approval.approved=false")
		if err := r.saveStatus(ctx, rem); err != nil {
			return ctrl.Result{}, err
		}
		r.event(rem, corev1.EventTypeNormal, "PendingApproval",
			fmt.Sprintf("处置待人工审批: %s %s（来自工单 %s）", rem.Spec.Action, rem.Spec.Deployment, rem.Spec.TicketRef))
		return ctrl.Result{}, nil
	}

	if !ap.Approved {
		// 拒绝是终态。
		rem.Status.Phase = remediationv1.PhaseRejected
		rem.Status.Message = "审批拒绝：" + ap.Note
		setCondition(rem, remediationv1.ConditionApproved, metav1.ConditionFalse,
			remediationv1.ReasonRejected, "由 "+approverOf(ap)+" 拒绝："+ap.Note)
		if err := r.saveStatus(ctx, rem); err != nil {
			return ctrl.Result{}, err
		}
		r.event(rem, corev1.EventTypeWarning, "Rejected", "处置被拒绝，工单侧转人工跟进")
		return ctrl.Result{}, nil
	}

	// 已批准：认领执行权——先落 status 再动手。
	// 回滚在此固化目标 revision（execution.RevisionTo），执行 = 收敛到目标态，
	// 之后任意次重放结果一致（幂等），这才是「变更不重复生效」的真正保证。
	rem.Status.Phase = remediationv1.PhaseExecuting
	rem.Status.ObservedGeneration = rem.Generation
	if rem.Status.Execution == nil {
		rem.Status.Execution = &remediationv1.ExecutionStatus{}
	}
	rem.Status.Execution.StartedAt = ptrTime(r.Now())
	if rem.Spec.Action == remediationv1.ActionUndo {
		from, to, err := PrevRevision(ctx, r.Actions, rem.Spec.Deployment)
		if err != nil {
			return r.executeFailed(ctx, rem, "无法确定回滚目标: "+err.Error())
		}
		rem.Status.Execution.RevisionFrom = ptrI64(from)
		rem.Status.Execution.RevisionTo = ptrI64(to)
	}
	setCondition(rem, remediationv1.ConditionApproved, metav1.ConditionTrue,
		remediationv1.ReasonApproved, "由 "+approverOf(ap)+" 审批通过")
	if err := r.saveStatus(ctx, rem); err != nil {
		return ctrl.Result{}, err
	}
	r.event(rem, corev1.EventTypeNormal, "Approved",
		fmt.Sprintf("审批通过（%s），认领执行权，动作: %s %s", approverOf(ap), rem.Spec.Action, rem.Spec.Deployment))
	return ctrl.Result{Requeue: true}, nil
}

// reconcileExecuting 执行动作（收敛式幂等）→ 进入回验。
func (r *Reconciler) reconcileExecuting(ctx context.Context, rem *remediationv1.Remediation) (ctrl.Result, error) {
	ex := rem.Status.Execution
	if ex == nil || rem.Status.ObservedGeneration != rem.Generation {
		// 异常修复：Executing 却缺认领记录（历史数据/手改）→ 重新认领。
		return r.claimAgain(ctx, rem)
	}
	if ex.CompletedAt != nil {
		// 动作结果已落库但 phase 未推进（崩溃窗口）→ 幂等推进。
		return r.enterVerifying(ctx, rem)
	}

	// 执行动作。两种动作都是收敛式：scale 收敛副本数；undo 收敛到
	// 认领时固化的 RevisionTo 模板。中断重放收敛到同一结果。
	var desc string
	switch rem.Spec.Action {
	case remediationv1.ActionScale:
		if rem.Spec.Replicas == nil {
			return r.executeFailed(ctx, rem, "k8s.scale 缺少 replicas 参数")
		}
		res, err := Scale(ctx, r.Actions, rem.Spec.Deployment, *rem.Spec.Replicas)
		if err != nil {
			return r.executeFailed(ctx, rem, err.Error())
		}
		ex.FromReplicas = ptrI32(res.From)
		ex.ToReplicas = ptrI32(res.To)
		desc = fmt.Sprintf("副本数 %d → %d", res.From, res.To)
	case remediationv1.ActionUndo:
		if ex.RevisionTo == nil {
			return r.executeFailed(ctx, rem, "回滚缺少目标 revision（认领记录缺失）")
		}
		res, err := RollbackTo(ctx, r.Actions, rem.Spec.Deployment, *ex.RevisionTo)
		if err != nil {
			return r.executeFailed(ctx, rem, err.Error())
		}
		ex.RevisionFrom = ptrI64(res.RevisionFrom)
		desc = fmt.Sprintf("revision %d → %d", res.RevisionFrom, res.RevisionTo)
	default:
		return r.executeFailed(ctx, rem, "未知处置动作: "+rem.Spec.Action)
	}
	ex.CompletedAt = ptrTime(r.Now())
	rem.Status.Message = "动作已执行: " + desc
	return r.enterVerifying(ctx, rem)
}

// enterVerifying 进入回验阶段（幂等：重复进入不重复初始化 attempts）。
func (r *Reconciler) enterVerifying(ctx context.Context, rem *remediationv1.Remediation) (ctrl.Result, error) {
	rem.Status.Phase = remediationv1.PhaseVerifying
	if rem.Status.Verification == nil {
		_, threshold, _, attempts := r.effectiveVerify(rem)
		rem.Status.Verification = &remediationv1.VerificationStatus{
			Threshold: threshold,
			Attempts:  attempts,
		}
	}
	setCondition(rem, remediationv1.ConditionExecuted, metav1.ConditionTrue,
		remediationv1.ReasonExecuted, rem.Status.Message)
	if err := r.saveStatus(ctx, rem); err != nil {
		return ctrl.Result{}, err
	}
	r.event(rem, corev1.EventTypeNormal, "ActionExecuted", rem.Status.Message)
	return ctrl.Result{}, nil // 下一轮 reconcile 按 wait 计算 RequeueAfter
}

// executeFailed 动作执行失败：变更语义不自动重试，终态 Failed。
func (r *Reconciler) executeFailed(ctx context.Context, rem *remediationv1.Remediation, reason string) (ctrl.Result, error) {
	rem.Status.Phase = remediationv1.PhaseFailed
	rem.Status.Message = "动作执行失败：" + reason
	if rem.Status.Execution != nil {
		rem.Status.Execution.CompletedAt = ptrTime(r.Now())
		rem.Status.Execution.Error = reason
	}
	setCondition(rem, remediationv1.ConditionExecuted, metav1.ConditionFalse,
		remediationv1.ReasonExecuteFailed, reason)
	if err := r.saveStatus(ctx, rem); err != nil {
		return ctrl.Result{}, err
	}
	r.event(rem, corev1.EventTypeWarning, "ActionExecuteFailed", rem.Status.Message)
	return ctrl.Result{}, nil
}

// claimAgain 认领记录缺失时的修复路径。
func (r *Reconciler) claimAgain(ctx context.Context, rem *remediationv1.Remediation) (ctrl.Result, error) {
	rem.Status.ObservedGeneration = rem.Generation
	if rem.Status.Execution == nil {
		rem.Status.Execution = &remediationv1.ExecutionStatus{}
	}
	if rem.Status.Execution.StartedAt == nil {
		rem.Status.Execution.StartedAt = ptrTime(r.Now())
	}
	if err := r.saveStatus(ctx, rem); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileVerifying 等待观察窗 → 回验查询 → 达标 Succeeded / 未达标重查 / 耗尽 Escalated。
func (r *Reconciler) reconcileVerifying(ctx context.Context, rem *remediationv1.Remediation) (ctrl.Result, error) {
	ex := rem.Status.Execution
	if ex == nil || ex.CompletedAt == nil {
		// 状态不一致（不应发生）：以当前时间锚定，保守继续回验。
		ex = &remediationv1.ExecutionStatus{CompletedAt: ptrTime(r.Now())}
		rem.Status.Execution = ex
	}
	template, threshold, wait, _ := r.effectiveVerify(rem)
	if rem.Status.Verification == nil {
		rem.Status.Verification = &remediationv1.VerificationStatus{Threshold: threshold}
	}
	vs := rem.Status.Verification

	// 观察窗：首次以 CompletedAt 为锚，其后以上次检查时间为锚。
	anchor := ex.CompletedAt.Time
	if vs.LastCheckAt != nil {
		anchor = vs.LastCheckAt.Time
	}
	nextCheck := anchor.Add(wait)
	now := r.Now()
	if now.Before(nextCheck) {
		// 等待窗内：把剩余等待写入 verification 供 kubectl get 可见（不落库，纯返回值）。
		return ctrl.Result{RequeueAfter: nextCheck.Sub(now)}, nil
	}

	// 消耗一次检查机会（查询失败同样计入——有界自愈，避免 Prometheus 长期不可用时无限循环）。
	vs.Attempts--
	vs.LastCheckAt = ptrTime(now)
	value, err := r.Verifier.Query(ctx, template, map[string]string{"window": "2m"})
	if err != nil {
		vs.Result = "error"
		if vs.Attempts <= 0 {
			return r.escalate(ctx, rem, fmt.Sprintf("回验查询连续失败: %v", err))
		}
		rem.Status.Message = fmt.Sprintf("回验查询失败（剩余 %d 次机会）: %v", vs.Attempts, err)
		if err := r.saveStatus(ctx, rem); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	vs.Value = value
	if value <= threshold {
		vs.Result = "passed"
		rem.Status.Phase = remediationv1.PhaseSucceeded
		rem.Status.Message = fmt.Sprintf("回验通过: %s=%.4f ≤ 阈值 %.4f，恢复确认", template, value, threshold)
		setCondition(rem, remediationv1.ConditionVerified, metav1.ConditionTrue,
			remediationv1.ReasonVerifyPassed, rem.Status.Message)
		if err := r.saveStatus(ctx, rem); err != nil {
			return ctrl.Result{}, err
		}
		r.event(rem, corev1.EventTypeNormal, "Verified", rem.Status.Message)
		return ctrl.Result{}, nil
	}

	vs.Result = "failed"
	if vs.Attempts <= 0 {
		return r.escalate(ctx, rem, fmt.Sprintf("回验未通过: %s=%.4f > 阈值 %.4f（已复查 %d 次）", template, value, threshold, r.effectiveAttempts(rem)))
	}
	rem.Status.Message = fmt.Sprintf("回验未达标: %s=%.4f > %.4f，剩余 %d 次复查", template, value, threshold, vs.Attempts)
	if err := r.saveStatus(ctx, rem); err != nil {
		return ctrl.Result{}, err
	}
	r.event(rem, corev1.EventTypeNormal, "VerifyPending", rem.Status.Message)
	return ctrl.Result{RequeueAfter: wait}, nil
}

// escalate 回验耗尽：终态 Escalated，转人工（不自动追加动作）。
func (r *Reconciler) escalate(ctx context.Context, rem *remediationv1.Remediation, msg string) (ctrl.Result, error) {
	rem.Status.Phase = remediationv1.PhaseEscalated
	rem.Status.Message = msg
	setCondition(rem, remediationv1.ConditionVerified, metav1.ConditionFalse,
		remediationv1.ReasonVerifyEscalated, msg)
	if err := r.saveStatus(ctx, rem); err != nil {
		return ctrl.Result{}, err
	}
	r.event(rem, corev1.EventTypeWarning, "Escalated", msg+"，转人工跟进")
	return ctrl.Result{}, nil
}

// effectiveVerify 解析 CR spec.verify 与启动默认值的合并结果（零值回落默认）。
func (r *Reconciler) effectiveVerify(rem *remediationv1.Remediation) (string, float64, time.Duration, int) {
	vs := rem.Spec.Verify
	template := vs.Template
	if template == "" {
		template = r.Defaults.Template
	}
	threshold := r.Defaults.Threshold
	if vs.Threshold != nil {
		threshold = *vs.Threshold
	}
	waitSeconds := r.Defaults.WaitSeconds
	if vs.WaitSeconds != nil {
		waitSeconds = *vs.WaitSeconds
	}
	attempts := r.effectiveAttempts(rem)
	if waitSeconds < 5 {
		waitSeconds = 5 // 防误配成 0 导致忙轮询
	}
	return template, threshold, time.Duration(waitSeconds) * time.Second, attempts
}

func (r *Reconciler) effectiveAttempts(rem *remediationv1.Remediation) int {
	if rem.Spec.Verify.Attempts != nil {
		if n := *rem.Spec.Verify.Attempts; n > 0 {
			return n
		}
	}
	if r.Defaults.Attempts > 0 {
		return r.Defaults.Attempts
	}
	return 3
}

// saveStatus 提交 status 子资源；冲突返回错误交给 controller-runtime 退避重试。
func (r *Reconciler) saveStatus(ctx context.Context, rem *remediationv1.Remediation) error {
	err := r.Status().Update(ctx, rem)
	if apierrors.IsConflict(err) {
		return err
	}
	return err
}

func (r *Reconciler) event(rem *remediationv1.Remediation, typ, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(rem, typ, reason, msg)
}

func setCondition(rem *remediationv1.Remediation, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&rem.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: rem.Generation,
		Reason:             reason,
		Message:            msg,
	})
}

func approverOf(ap *remediationv1.ApprovalStatus) string {
	if ap == nil || ap.Approver == "" {
		return "operator"
	}
	return ap.Approver
}

func ptrTime(t time.Time) *metav1.Time { v := metav1.NewTime(t); return &v }
func ptrI32(v int32) *int32            { return &v }
func ptrI64(v int64) *int64            { return &v }
