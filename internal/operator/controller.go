package operator

import (
	"k8s.io/client-go/rest"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/internal/apis/scheme"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// NewManager 构造限定单 namespace 的 manager：
// 缓存/watch 都收敛在业务 namespace，namespaced Role 的最小权限即可运行
// （若做集群级 watch，则必须升级 ClusterRole——本项目范围不做）。
// 健康探针独立监听 :8081（/healthz、/readyz），供 Deployment 存活/就绪探针。
func NewManager(restCfg *rest.Config, ns, metricsAddr string, leaderElection bool, leaderElectionID string) (ctrl.Manager, error) {
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme.Scheme,
		Cache:                   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		LeaderElection:          leaderElection,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: ns,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  ":8081",
	})
	if err != nil {
		return nil, err
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return nil, err
	}
	return mgr, nil
}

// SetupWithManager 注册控制器：只 watch Remediation 自身
// （审批写入 status / 状态迁移都会产生 watch 事件，驱动状态机推进）。
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&remediationv1.Remediation{}).
		Complete(r)
}
