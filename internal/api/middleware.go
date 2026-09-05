package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// traceContext 保留请求的 trace 上下文，脱离请求生命周期（后台继续处理不随请求中断）。
func traceContext(ctx context.Context) context.Context {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return context.WithoutCancel(ctx)
	}
	// 以请求 span 为父挂接，编排器在此上下文上接续同一 trace。
	bctx, span := otel.Tracer("aiops/internal/api").Start(context.WithoutCancel(ctx), "api.ticket.background")
	_ = span // 仅挂接父子关系；编排器链路自行结束
	return bctx
}

// requestLog 请求日志中间件。
func requestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}
