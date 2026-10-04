// Package scheme 提供全服务共享的 runtime.Scheme：
// client-go 内建类型 + Remediation CRD 类型。
// 服务端（创建/审批 CR）与 operator（watch/执行）必须用同一份 scheme。
package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	remediationv1 "aiops/internal/apis/remediation/v1"
)

// Scheme 共享 scheme 单例。
var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(remediationv1.AddToScheme(Scheme))
}
