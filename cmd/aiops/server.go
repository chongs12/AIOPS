package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"aiops/internal/ticket"
	"aiops/pkg/telemetry"
)

// runServe 启动 HTTP 服务（M0：healthz + metrics；后续里程碑逐步挂载工单/聊天路由）。
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	slog.Info("配置加载完成", "config", *cfgPath)

	ctx, stop := newSignalContext()
	defer stop()

	tel := telemetry.Init(ctx, telemetry.Config{
		Enabled:      cfg.Telemetry.Enabled,
		ServiceName:  cfg.Telemetry.ServiceName,
		OTLPEndpoint: cfg.Telemetry.OTLPEndpoint,
		SampleRatio:  cfg.Telemetry.SampleRatio,
	})
	defer tel.Shutdown(context.Background())

	a, err := buildApp(ctx, cfg, tel)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", a.handler)

	apiSrv := &http.Server{Addr: cfg.Server.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	metrics := &http.Server{Addr: cfg.Server.MetricsAddr, Handler: tel.MetricsHandler()}

	errCh := make(chan error, 2)
	go func() { errCh <- metrics.ListenAndServe() }()
	go func() { errCh <- apiSrv.ListenAndServe() }()

	slog.Info("aiops-server 启动",
		"addr", cfg.Server.Addr, "metrics", cfg.Server.MetricsAddr,
		"telemetry", tel.Enabled(), "llm", a.llmEnabled, "store", storeKind(a.store))

	select {
	case <-ctx.Done():
		slog.Info("收到退出信号，正在优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var errs []error
		if err := apiSrv.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, err)
		}
		if err := metrics.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
	}
	return nil
}

// storeKind 存储实现名（启动日志用）。
func storeKind(s ticket.Store) string {
	switch s.(type) {
	case *ticket.PGStore:
		return "postgres"
	case *ticket.MemStore:
		return "memory(降级)"
	default:
		return "unknown"
	}
}
