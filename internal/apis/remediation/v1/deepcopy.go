// 本文件手写 DeepCopy（类型少且稳定，省去 codegen 工具链）。
// 语义与 deepcopy-gen 生成代码一致：入参 nil 直接返回 nil。
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto 深拷贝。
func (in *VerifySpec) DeepCopyInto(out *VerifySpec) {
	*out = *in
	if in.Threshold != nil {
		v := *in.Threshold
		out.Threshold = &v
	}
	if in.WaitSeconds != nil {
		v := *in.WaitSeconds
		out.WaitSeconds = &v
	}
	if in.Attempts != nil {
		v := *in.Attempts
		out.Attempts = &v
	}
}

// DeepCopy 深拷贝。
func (in *VerifySpec) DeepCopy() *VerifySpec {
	if in == nil {
		return nil
	}
	out := new(VerifySpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 深拷贝。
func (in *RemediationSpec) DeepCopyInto(out *RemediationSpec) {
	*out = *in
	in.Verify.DeepCopyInto(&out.Verify)
	if in.Replicas != nil {
		v := *in.Replicas
		out.Replicas = &v
	}
}

// DeepCopyInto 深拷贝。
func (in *ApprovalStatus) DeepCopyInto(out *ApprovalStatus) {
	*out = *in
	if in.At != nil {
		v := *in.At
		out.At = &v
	}
}

// DeepCopyInto 深拷贝。
func (in *ExecutionStatus) DeepCopyInto(out *ExecutionStatus) {
	*out = *in
	if in.StartedAt != nil {
		v := *in.StartedAt
		out.StartedAt = &v
	}
	if in.CompletedAt != nil {
		v := *in.CompletedAt
		out.CompletedAt = &v
	}
	if in.FromReplicas != nil {
		v := *in.FromReplicas
		out.FromReplicas = &v
	}
	if in.ToReplicas != nil {
		v := *in.ToReplicas
		out.ToReplicas = &v
	}
	if in.RevisionFrom != nil {
		v := *in.RevisionFrom
		out.RevisionFrom = &v
	}
	if in.RevisionTo != nil {
		v := *in.RevisionTo
		out.RevisionTo = &v
	}
}

// DeepCopyInto 深拷贝。
func (in *VerificationStatus) DeepCopyInto(out *VerificationStatus) {
	*out = *in
	if in.LastCheckAt != nil {
		v := *in.LastCheckAt
		out.LastCheckAt = &v
	}
}

// DeepCopyInto 深拷贝。
func (in *RemediationStatus) DeepCopyInto(out *RemediationStatus) {
	*out = *in
	if in.Approval != nil {
		out.Approval = new(ApprovalStatus)
		in.Approval.DeepCopyInto(out.Approval)
	}
	if in.Execution != nil {
		out.Execution = new(ExecutionStatus)
		in.Execution.DeepCopyInto(out.Execution)
	}
	if in.Verification != nil {
		out.Verification = new(VerificationStatus)
		in.Verification.DeepCopyInto(out.Verification)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyInto 深拷贝 Remediation。
func (in *Remediation) DeepCopyInto(out *Remediation) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy 深拷贝。
func (in *Remediation) DeepCopy() *Remediation {
	if in == nil {
		return nil
	}
	out := new(Remediation)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto 深拷贝 RemediationList。
func (in *RemediationList) DeepCopyInto(out *RemediationList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Remediation, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy 深拷贝。
func (in *RemediationList) DeepCopy() *RemediationList {
	if in == nil {
		return nil
	}
	out := new(RemediationList)
	in.DeepCopyInto(out)
	return out
}

// _ 保持 runtime 导入（DeepCopyObject 声明在 types.go）。
var _ runtime.Object = (*Remediation)(nil)
