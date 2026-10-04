// Package v1 定义 Remediation CRD（aiops.io/v1）：
// 把 M3 处置闭环（扩缩容/回滚 + 审批门 + 执行 + 恢复回验）建模为 K8s API。
// 审批门、执行结果、回验结论全部落在 status，处置过程 kubectl describe 即全程可读。
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// 组/版本与资源常量。
const (
	GroupName = "aiops.io"
	Version   = "v1"

	// LabelTicketRef CR 上的工单标签键（服务端按工单反查处置记录用）。
	LabelTicketRef = "aiops.io/ticket"
)

// SchemeGroupVersion API 组版本。
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// 处置动作（与 internal/tool/k8stool 工具名一致）。
const (
	ActionScale = "k8s.scale"
	ActionUndo  = "k8s.rollout_undo"
)

// phase 状态机（level-triggered，每一步都可在崩溃后由 reconcile 续跑）。
const (
	PhasePendingApproval = "PendingApproval" // 待人工审批（终态之外的起点）
	PhaseExecuting       = "Executing"       // 已审批，动作执行中
	PhaseVerifying       = "Verifying"       // 动作完成，等待回验窗口/回验未达标重查
	PhaseSucceeded       = "Succeeded"       // 回验通过（终态）
	PhaseFailed          = "Failed"          // 动作执行失败（终态，不自动重试——变更语义）
	PhaseEscalated       = "Escalated"       // 回验多次未达标，转人工跟进（终态）
	PhaseRejected        = "Rejected"        // 审批拒绝（终态）
)

// Condition 类型与原因（kubectl describe 可读）。
const (
	ConditionApproved = "Approved"
	ConditionExecuted = "Executed"
	ConditionVerified = "Verified"

	ReasonWaitingApproval = "WaitingApproval"
	ReasonApproved        = "Approved"
	ReasonRejected        = "Rejected"
	ReasonExecuted        = "ActionExecuted"
	ReasonExecuteFailed   = "ActionExecuteFailed"
	ReasonVerifyPassed    = "RecoveryVerified"
	ReasonVerifyPending   = "RecoveryNotYetVerified"
	ReasonVerifyEscalated = "RecoveryNotConfirmed"
)

// VerifySpec 回验配置；nil/零值字段由 operator 用启动默认值填充。
// 模板来自 promtool 模板库（如 http_5xx_rate），threshold 与模板语义匹配。
type VerifySpec struct {
	Template    string   `json:"template,omitempty"`
	Threshold   *float64 `json:"threshold,omitempty"`
	WaitSeconds *int     `json:"waitSeconds,omitempty"`
	Attempts    *int     `json:"attempts,omitempty"`
}

// RemediationSpec 处置意图（期望状态）。
type RemediationSpec struct {
	// TicketRef 来源工单；console/脚本来源可空。
	TicketRef string `json:"ticketRef,omitempty"`
	// Deployment 目标（v1 仅支持同 namespace 的 Deployment）。
	Deployment string `json:"deployment"`
	// Action 处置动作：k8s.scale | k8s.rollout_undo。
	Action string `json:"action"`
	// Replicas scale 动作的目标副本数（undo 忽略）。
	Replicas *int32 `json:"replicas,omitempty"`
	// Reason 根因摘要（进入 Events/审计，供人审批时参考）。
	Reason string `json:"reason,omitempty"`
	// Verify 回验配置，零值字段用 operator 默认值。
	Verify VerifySpec `json:"verify,omitempty"`
}

// ApprovalStatus 审批记录：谁、何时、批注。
type ApprovalStatus struct {
	Approved bool         `json:"approved"`
	Approver string       `json:"approver,omitempty"`
	At       *metav1.Time `json:"at,omitempty"`
	Note     string       `json:"note,omitempty"`
}

// ExecutionStatus 动作执行结果。scale 记录前后副本数；undo 记录前后 revision。
// RevisionTo 在认领阶段即写入（目标态）：执行 = 把 Deployment 收敛到该
// revision 的模板，因此中断后重放收敛到同一结果（幂等），无需区分崩溃点。
type ExecutionStatus struct {
	StartedAt    *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt  *metav1.Time `json:"completedAt,omitempty"`
	FromReplicas *int32       `json:"fromReplicas,omitempty"`
	ToReplicas   *int32       `json:"toReplicas,omitempty"`
	RevisionFrom *int64       `json:"revisionFrom,omitempty"`
	RevisionTo   *int64       `json:"revisionTo,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// VerificationStatus 恢复回验结论。Result: passed | failed | error。
type VerificationStatus struct {
	Value       float64      `json:"value,omitempty"`
	Threshold   float64      `json:"threshold,omitempty"`
	Attempts    int          `json:"attempts,omitempty"`
	LastCheckAt *metav1.Time `json:"lastCheckAt,omitempty"`
	Result      string       `json:"result,omitempty"`
}

// RemediationStatus 处置实况。
type RemediationStatus struct {
	Phase string `json:"phase,omitempty"`
	// ObservedGeneration 已处理的 spec 代数——reconcile 幂等闸：
	// == metadata.generation 表示本代 spec 的动作已执行（绝不重复执行）。
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	Approval           *ApprovalStatus     `json:"approval,omitempty"`
	Execution          *ExecutionStatus    `json:"execution,omitempty"`
	Verification       *VerificationStatus `json:"verification,omitempty"`
	Message            string              `json:"message,omitempty"`
	Conditions         []metav1.Condition  `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen=package （提示性注释；本仓库手写 deepcopy，见 deepcopy.go）

// Remediation 一次故障处置的声明式表达。
// +k8s:deepcopy-gen=true
type Remediation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RemediationSpec   `json:"spec,omitempty"`
	Status RemediationStatus `json:"status,omitempty"`
}

// RemediationList 列表。
// +k8s:deepcopy-gen=true
type RemediationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Remediation `json:"items"`
}

// GetObjectKind 实现 runtime.Object。
func (r *Remediation) GetObjectKind() schema.ObjectKind { return &r.TypeMeta }

// GetObjectKind 实现 runtime.Object。
func (l *RemediationList) GetObjectKind() schema.ObjectKind { return &l.TypeMeta }

// DeepCopyObject 实现 runtime.Object（见 deepcopy.go）。
func (r *Remediation) DeepCopyObject() runtime.Object { return r.DeepCopy() }

// DeepCopyObject 实现 runtime.Object（见 deepcopy.go）。
func (l *RemediationList) DeepCopyObject() runtime.Object { return l.DeepCopy() }
