package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"aiops/internal/apis/scheme"
	"aiops/internal/operator"
	"aiops/pkg/telemetry"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// runOperator 启动 Remediation Operator（aiops operator 子命令）：
// watch Remediation CR，驱动 审批门→执行→恢复回验 状态机。
// 与 serve 同镜像不同进程（helm chart 中独立 Deployment，双副本 + leader election）。
func runOperator(args []string) error {
	fs := flag.NewFlagSet("operator", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	slog.Info("配置加载完成", "config", *cfgPath, "namespace", cfg.K8s.Namespace)

	// controller-runtime 日志桥接到 zap（不设置则所有 reconcile 错误被静默吞掉）
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx, stop := newSignalContext()
	defer stop()

	tel := telemetry.Init(ctx, telemetry.Config{
		Enabled:      cfg.Telemetry.Enabled,
		ServiceName:  cfg.Telemetry.ServiceName + "-operator",
		OTLPEndpoint: cfg.Telemetry.OTLPEndpoint,
		SampleRatio:  cfg.Telemetry.SampleRatio,
	})
	defer tel.Shutdown(context.Background())

	restCfg, err := k8sRestConfig(cfg.K8s.Kubeconfig)
	if err != nil {
		return fmt.Errorf("构建 K8s 访问配置: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("构建 clientset: %w", err)
	}

	mgr, err := operator.NewManager(restCfg, cfg.K8s.Namespace,
		cfg.Operator.MetricsAddr, cfg.Operator.LeaderElection, cfg.Operator.LeaderElectionID)
	if err != nil {
		return fmt.Errorf("构建 controller manager: %w", err)
	}

	actions := operator.NewK8sActions(clientset, cfg.K8s.Namespace)
	verifier := operator.NewPromVerifier(cfg.Prometheus.BaseURL,
		time.Duration(cfg.Prometheus.TimeoutSeconds)*time.Second)
	reconciler := operator.NewReconciler(mgr.GetClient(), scheme.Scheme,
		mgr.GetEventRecorderFor("aiops-operator"), actions, verifier,
		cfg.Operator.Verify, cfg.K8s.Namespace)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("注册控制器: %w", err)
	}

	slog.Info("aiops-operator 启动",
		"namespace", cfg.K8s.Namespace,
		"leader_election", cfg.Operator.LeaderElection,
		"metrics", cfg.Operator.MetricsAddr,
		"verify", cfg.Operator.Verify)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("controller manager 退出: %w", err)
	}
	return nil
}
