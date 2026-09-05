package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"aiops/pkg/ratelimit"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrNoProvider 未配置任何 provider。
var ErrNoProvider = errors.New("llm: 未配置任何 provider")

// Client LLM 调用入口：provider fallback 链（首个为主，失败依次切换），
// 内建并发闸、trace 与指标。全链路失败返回聚合错误。
type Client struct {
	providers []Provider
	sem       *ratelimit.Semaphore
	tracer    trace.Tracer
	metrics   *metrics
}

// ClientOptions 客户端参数。
type ClientOptions struct {
	// MaxConcurrency LLM 并发调用上限（昂贵资源保护），默认 4。
	MaxConcurrency int
}

// NewClient 构造客户端；providers 非空且首个为默认主 provider。
func NewClient(providers []Provider, opts ClientOptions) (*Client, error) {
	if len(providers) == 0 {
		return nil, ErrNoProvider
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 4
	}
	return &Client{
		providers: providers,
		sem:       ratelimit.NewSemaphore(opts.MaxConcurrency),
		tracer:    otel.Tracer("aiops/pkg/llm"),
		metrics:   newMetrics(),
	}, nil
}

// ProviderNames 返回 fallback 链顺序（用于启动日志/健康展示）。
func (c *Client) ProviderNames() []string {
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}
	return names
}

// Chat 走 fallback 链完成一次对话。
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	release, err := c.sem.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("llm 并发闸获取失败: %w", err)
	}
	defer release()

	var lastErr error
	for i, p := range c.providers {
		resp, err := c.chatOnce(ctx, p, i, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if i+1 < len(c.providers) {
			slog.Warn("llm provider 失败，切换 fallback", "provider", p.Name(), "next", c.providers[i+1].Name(), "err", err)
		}
	}
	return nil, fmt.Errorf("llm 全部 provider 失败: %w", lastErr)
}

// chatOnce 单 provider 调用，附 trace 与指标。
func (c *Client) chatOnce(ctx context.Context, p Provider, fallbackIndex int, req ChatRequest) (*ChatResponse, error) {
	sctx, span := c.tracer.Start(ctx, "llm.chat",
		trace.WithAttributes(
			attribute.String("llm.provider", p.Name()),
			attribute.Int("llm.fallback_index", fallbackIndex),
			attribute.Int("llm.input_messages", len(req.Messages)),
		))
	defer span.End()

	start := time.Now()
	resp, err := p.Chat(sctx, req)
	dur := time.Since(start)

	if err != nil {
		c.metrics.record(p.Name(), "error", dur, nil)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	c.metrics.record(p.Name(), "ok", dur, resp)
	span.SetAttributes(
		attribute.Int("llm.prompt_tokens", resp.PromptTokens),
		attribute.Int("llm.completion_tokens", resp.CompletionTokens),
	)
	return resp, nil
}
