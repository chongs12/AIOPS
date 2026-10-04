package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Resource 资源复数名（RBAC/CRD 引用）。
const Resource = "remediations"

// SchemeBuilder / AddToScheme 把本组类型注册进任意 runtime.Scheme。
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &Remediation{}, &RemediationList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
