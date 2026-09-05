package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"aiops/pkg/cache"
	"aiops/pkg/ratelimit"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Registry 工具注册表（重名注册报错）。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 构造注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册工具；重名报错。
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name()]; ok {
		return fmt.Errorf("tool: 工具 %q 重复注册", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回全部工具名（有序，供 LLM 工具清单与日志）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Executor 执行管道。
type Executor struct {
	reg     *Registry
	limiter *ratelimit.Limiter
	cache   *cache.Cache
	audit   Auditor

	sems   map[string]*ratelimit.Semaphore
	tracer trace.Tracer

	calls    metric.Int64Counter
	duration metric.Float64Histogram
}

// ExecutorDeps 执行器依赖。
type ExecutorDeps struct {
	Registry *Registry
	Limiter  *ratelimit.Limiter // 可为 nil（不限流）
	Cache    *cache.Cache       // 可为 nil（不缓存）
	Auditor  Auditor            // 可为 nil（noop）
}

// 默认值。
const (
	defaultTimeout = 10 * time.Second
	defaultBackoff = 500 * time.Millisecond
)

// NewExecutor 构造执行器，并按工具声明初始化并发闸与指标。
func NewExecutor(d ExecutorDeps) *Executor {
	e := &Executor{
		reg:     d.Registry,
		limiter: d.Limiter,
		cache:   d.Cache,
		audit:   d.Auditor,
		sems:    make(map[string]*ratelimit.Semaphore),
		tracer:  otel.Tracer("aiops/internal/tool"),
	}
	if d.Registry != nil {
		for _, name := range d.Registry.Names() {
			if t, ok := d.Registry.Get(name); ok {
				s := t.Schema()
				if s.Concurrency > 0 {
					e.sems[name] = ratelimit.NewSemaphore(s.Concurrency)
				}
			}
		}
	}
	m := otel.Meter("aiops/internal/tool")
	var err error
	if e.calls, err = m.Int64Counter("tool.calls.total",
		metric.WithDescription("工具调用次数")); err != nil {
		e.calls = nil
	}
	if e.duration, err = m.Float64Histogram("tool.call.duration",
		metric.WithDescription("工具调用耗时（秒）"), metric.WithUnit("s")); err != nil {
		e.duration = nil
	}
	return e
}

// Execute 走完整管道执行一次工具调用。
// 契约：除「工具不存在/输入非法」等调用方错误返回 error 外，
// 工具失败/超时/限流均返回 status=failed 的结果信封（不中断编排）。
func (e *Executor) Execute(ctx context.Context, ticketID, name string, input json.RawMessage) (*Result, error) {
	t, ok := e.reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool: 未注册的工具 %q", name)
	}
	schema := t.Schema()
	if err := validateInput(schema, input); err != nil {
		return nil, fmt.Errorf("tool %s 输入校验失败: %w", name, err)
	}

	sctx, span := e.tracer.Start(ctx, "tool.execute",
		trace.WithAttributes(
			attribute.String("tool.name", name),
			attribute.String("ticket.id", ticketID),
		))
	defer span.End()

	start := time.Now()

	// 缓存查询（命中便宜，放在限流之前，避免浪费配额）。
	if e.cache != nil && schema.CacheTTL > 0 {
		if res := e.getCached(sctx, name, input); res != nil {
			res.DurationMS = time.Since(start).Milliseconds()
			e.record(span, name, res, time.Since(start))
			return res, nil
		}
	}

	// 限流（继承 PreHire：判定 100ms 超时，Redis 故障走本地桶，绝不比业务慢）。
	if e.limiter != nil && schema.RatePerSecond > 0 {
		d := e.limiter.Allow(sctx, "tool:"+name, schema.RatePerSecond, schema.RateBurst)
		if !d.Allowed {
			res := &Result{
				Status:         StatusFailed,
				DegradedReason: fmt.Sprintf("rate_limited: 工具 %s 触发限流，%s 后重试", name, d.RetryAfter),
				Evidence:       EvidenceMeta{Source: sourceOf(name)},
			}
			e.record(span, name, res, time.Since(start))
			e.auditAsync(ticketID, name, input, res)
			return res, nil
		}
	}

	// 并发闸。
	res, err := e.execWithGate(sctx, t, schema, input)
	if res == nil && err != nil {
		// 硬错误兜底为 failed 信封。
		res = &Result{Status: StatusFailed, DegradedReason: err.Error(),
			Evidence: EvidenceMeta{Source: sourceOf(name)}}
		err = nil
	}
	res.DurationMS = time.Since(start).Milliseconds()

	// 写缓存（仅 ok/degraded）。
	if e.cache != nil && schema.CacheTTL > 0 && res.Status != StatusFailed {
		if b, jerr := json.Marshal(res); jerr == nil {
			e.cache.Set(sctx, cache.Key("tool", name, inputHash(input)), b, schema.CacheTTL)
		}
	}

	e.record(span, name, res, time.Since(start))
	e.auditAsync(ticketID, name, input, res)
	return res, nil
}

func (e *Executor) execWithGate(ctx context.Context, t Tool, s Schema, input json.RawMessage) (*Result, error) {
	sem, hasSem := e.sems[t.Name()]
	if hasSem {
		release, err := sem.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("并发闸获取失败: %w", err)
		}
		defer release()
	}
	return e.execWithRetry(ctx, t, s, input)
}

// execWithRetry 独立超时 + 重试（仅幂等读工具）。
func (e *Executor) execWithRetry(ctx context.Context, t Tool, s Schema, input json.RawMessage) (*Result, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	attempts := 1
	if s.Idempotent && s.MaxRetries > 0 {
		attempts = s.MaxRetries + 1
	}

	var lastRes *Result
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(defaultBackoff * time.Duration(i)) // 线性退避
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		res, err := t.Execute(actx, input)
		ctxErr := actx.Err() // 必须在 cancel 前读取
		cancel()
		if err == nil && ctxErr != nil {
			// 工具未感知 ctx 而在超时后返回成功：仍按超时处理。
			err = fmt.Errorf("执行超时（%v）", timeout)
		}
		if err == nil {
			return res, nil
		}
		lastRes, lastErr = res, err
	}
	// 工具自身可能返回部分降级结果（如 nil error + degraded）；此处为硬错误路径。
	if lastRes != nil && lastRes.Status == StatusDegraded {
		return lastRes, nil
	}
	return nil, fmt.Errorf("tool %s 执行失败（尝试 %d 次）: %w", t.Name(), attempts, lastErr)
}

func (e *Executor) getCached(ctx context.Context, name string, input json.RawMessage) *Result {
	v, ok := e.cache.Get(ctx, cache.Key("tool", name, inputHash(input)))
	if !ok {
		return nil
	}
	var res Result
	if err := json.Unmarshal(v, &res); err != nil {
		return nil
	}
	res.FromCache = true
	return &res
}

func (e *Executor) record(span trace.Span, name string, res *Result, d time.Duration) {
	span.SetAttributes(
		attribute.String("tool.status", res.Status),
		attribute.Bool("tool.from_cache", res.FromCache),
		attribute.Bool("tool.degraded", res.Status == StatusDegraded || res.Status == StatusFailed),
	)
	if res.Status != StatusOK {
		span.SetStatus(codes.Error, res.DegradedReason)
	}
	if e.calls != nil {
		e.calls.Add(nil, 1,
			metric.WithAttributes(
				attribute.String("tool", name),
				attribute.String("status", res.Status),
			))
	}
	if e.duration != nil {
		e.duration.Record(nil, d.Seconds(),
			metric.WithAttributes(attribute.String("tool", name)))
	}
}

func (e *Executor) auditAsync(ticketID, name string, input json.RawMessage, res *Result) {
	if e.audit == nil {
		return
	}
	entry := &AuditEntry{
		TicketID: ticketID, Tool: name, Input: input,
		Status: res.Status, DurationMS: res.DurationMS,
		Error: res.DegradedReason, At: time.Now(),
	}
	if err := e.audit.Record(context.WithoutCancel(context.Background()), entry); err != nil {
		fmt.Printf("tool audit 记录失败: %v\n", err)
	}
}

// sourceOf 从工具名推断数据源（prometheus.query → prometheus）。
func sourceOf(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}
