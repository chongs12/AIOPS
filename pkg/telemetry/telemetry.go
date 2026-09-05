// Package telemetry 初始化 OTel：trace 经 OTLP gRPC 上报（PreHire Jaeger），
// metrics 经 prometheus exporter 暴露在 :6060/metrics。
// 故障语义：遥测初始化失败一律降级为 noop 并告警，绝不阻塞主路径。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config 遥测配置（由 pkg/config.Telemetry 映射而来）。
type Config struct {
	Enabled      bool
	ServiceName  string
	OTLPEndpoint string
	SampleRatio  float64
}

// Telemetry 持有 provider 生命周期；Shutdown 负责 flush。
type Telemetry struct {
	shutdownTracer func(context.Context) error
	shutdownMeter  func(context.Context) error
	enabled        bool
	registry       *prometheus.Registry
}

// Init 初始化全局 TracerProvider / MeterProvider。
// 任何一步失败都降级为 noop：返回的 Telemetry 的 Shutdown 是安全的空操作。
func Init(ctx context.Context, cfg Config) *Telemetry {
	if !cfg.Enabled {
		slog.Warn("telemetry 未启用，运行于无遥测模式")
		return noop()
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		slog.Warn("telemetry resource 构建失败，降级为 noop", "err", err)
		return noop()
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		slog.Warn("OTLP trace exporter 初始化失败，trace 降级为 noop", "endpoint", cfg.OTLPEndpoint, "err", err)
		exp = nil
	}

	var shutdownTracer func(context.Context) error
	if exp != nil {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
			// 根 span 按比例采样，子 span 跟随父决策（已有 trace 头则透传）。
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		)
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
		shutdownTracer = tp.Shutdown
	}

	// metrics：OTel 仪表 → 专用 prometheus registry（含 Go runtime 指标）。
	reg := prometheus.NewRegistry()
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		slog.Warn("prometheus exporter 初始化失败，metrics 降级为 noop", "err", err)
		return &Telemetry{enabled: false, registry: reg}
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExp),
	)
	otel.SetMeterProvider(mp)
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	return &Telemetry{
		shutdownTracer: shutdownTracer,
		shutdownMeter:  mp.Shutdown,
		enabled:        true,
		registry:       reg,
	}
}

// noop 返回无遥测实例（全局 provider 保持默认 noop 实现）。
func noop() *Telemetry {
	return &Telemetry{enabled: false}
}

// Shutdown flush 并释放；带 5s 超时，失败仅告警。
func (t *Telemetry) Shutdown(ctx context.Context) {
	if t == nil || !t.enabled {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var errs []error
	if t.shutdownTracer != nil {
		if err := t.shutdownTracer(cctx); err != nil {
			errs = append(errs, fmt.Errorf("trace flush: %w", err))
		}
	}
	if t.shutdownMeter != nil {
		if err := t.shutdownMeter(cctx); err != nil {
			errs = append(errs, fmt.Errorf("meter flush: %w", err))
		}
	}
	if len(errs) > 0 {
		slog.Warn("telemetry shutdown 部分失败", "err", errors.Join(errs...))
	}
}

// MetricsHandler 返回 /metrics 的 http.Handler（未启用时返回空 registry 的 handler）。
func (t *Telemetry) MetricsHandler() http.Handler {
	reg := t.registry
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// Tracer 返回命名 tracer（包外使用统一入口）。
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// Enabled 报告遥测是否实际启用。
func (t *Telemetry) Enabled() bool { return t != nil && t.enabled }
