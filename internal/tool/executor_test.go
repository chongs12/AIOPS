package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiops/pkg/cache"
	"aiops/pkg/ratelimit"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeTool 可编程假工具。
type fakeTool struct {
	name      string
	schema    Schema
	failFirst int           // 前 N 次返回错误
	sleep     time.Duration // 每次执行耗时
	calls     atomic.Int32
	output    string
}

func (f *fakeTool) Name() string   { return f.name }
func (f *fakeTool) Desc() string   { return "fake tool " + f.name }
func (f *fakeTool) Schema() Schema { return f.schema }
func (f *fakeTool) Execute(ctx context.Context, _ json.RawMessage) (*Result, error) {
	n := f.calls.Add(1)
	if f.sleep > 0 {
		select {
		case <-time.After(f.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if int(n) <= f.failFirst {
		return nil, errors.New("simulated failure")
	}
	return &Result{
		Status:   StatusOK,
		Data:     json.RawMessage(`{"out":"` + f.output + `"}`),
		Evidence: EvidenceMeta{Source: "fake", Query: "select 1"},
	}, nil
}

func newExecutor(t *testing.T, tools ...Tool) (*Executor, *MemAuditor) {
	t.Helper()
	reg := NewRegistry()
	for _, tl := range tools {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	ma := &MemAuditor{}
	return NewExecutor(ExecutorDeps{Registry: reg, Auditor: ma}), ma
}

// MemAuditor 内存审计实现。
type MemAuditor struct {
	mu      sync.Mutex
	entries []*AuditEntry
}

func (m *MemAuditor) Record(_ context.Context, e *AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *MemAuditor) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func TestExecutorHappyPath(t *testing.T) {
	ft := &fakeTool{name: "fake.query", output: "ok", schema: Schema{Input: json.RawMessage(`{"type":"object"}`)}}
	e, audit := newExecutor(t, ft)

	res, err := e.Execute(context.Background(), "t-1", "fake.query", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || res.FromCache || string(res.Data) != `{"out":"ok"}` {
		t.Errorf("res = %+v", res)
	}
	if res.DurationMS < 0 {
		t.Errorf("DurationMS 异常: %d", res.DurationMS)
	}
	if audit.count() != 1 {
		t.Errorf("审计应记录 1 条, got %d", audit.count())
	}
}

func TestExecutorToolNotFound(t *testing.T) {
	e, _ := newExecutor(t)
	if _, err := e.Execute(context.Background(), "t-1", "nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("未注册工具应报错")
	}
}

func TestExecutorValidation(t *testing.T) {
	ft := &fakeTool{name: "v.query", schema: Schema{
		Input: json.RawMessage(`{"type":"object","required":["template"],"properties":{"template":{"type":"string"},"n":{"type":"number"}}}`),
	}}
	e, _ := newExecutor(t, ft)

	if _, err := e.Execute(context.Background(), "t-1", "v.query", json.RawMessage(`{}`)); err == nil {
		t.Fatal("缺必填应报错")
	}
	if _, err := e.Execute(context.Background(), "t-1", "v.query", json.RawMessage(`{"template":1}`)); err == nil {
		t.Fatal("类型不匹配应报错")
	}
	if _, err := e.Execute(context.Background(), "t-1", "v.query", json.RawMessage(`{"template":"a","n":"x"}`)); err == nil {
		t.Fatal("number 字段传字符串应报错")
	}
	res, err := e.Execute(context.Background(), "t-1", "v.query", json.RawMessage(`{"template":"a","unknown":true}`))
	if err != nil || res.Status != StatusOK {
		t.Fatalf("未声明参数应放行: %v %+v", err, res)
	}
}

func TestExecutorRetryIdempotent(t *testing.T) {
	ft := &fakeTool{name: "r.query", failFirst: 2, output: "after-retry", schema: Schema{
		Idempotent: true, MaxRetries: 2,
	}}
	e, _ := newExecutor(t, ft)

	res, err := e.Execute(context.Background(), "t-1", "r.query", json.RawMessage(`{}`))
	if err != nil || res.Status != StatusOK {
		t.Fatalf("重试后应成功: %v %+v", err, res)
	}
	if got := ft.calls.Load(); got != 3 {
		t.Errorf("应调用 3 次, got %d", got)
	}
}

func TestExecutorNoRetryForMutating(t *testing.T) {
	ft := &fakeTool{name: "m.exec", failFirst: 1, output: "x", schema: Schema{
		Idempotent: false, MaxRetries: 3, // 非幂等：不允许重试
	}}
	e, _ := newExecutor(t, ft)

	res, err := e.Execute(context.Background(), "t-1", "m.exec", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("硬错误应转 failed 信封: %v", err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.DegradedReason, "simulated failure") {
		t.Errorf("res = %+v", res)
	}
	if got := ft.calls.Load(); got != 1 {
		t.Errorf("非幂等不得重试, calls=%d", got)
	}
}

func TestExecutorRetryExhaustedFails(t *testing.T) {
	ft := &fakeTool{name: "x.query", failFirst: 99, schema: Schema{Idempotent: true, MaxRetries: 1}}
	e, _ := newExecutor(t, ft)

	res, err := e.Execute(context.Background(), "t-1", "x.query", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("重试耗尽应转 failed 信封而非 error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Errorf("res = %+v", res)
	}
	if got := ft.calls.Load(); got != 2 {
		t.Errorf("1+1 次尝试, got %d", got)
	}
}

func TestExecutorTimeout(t *testing.T) {
	ft := &fakeTool{name: "slow.query", sleep: 300 * time.Millisecond, schema: Schema{
		Timeout: 50 * time.Millisecond,
	}}
	e, _ := newExecutor(t, ft)

	res, err := e.Execute(context.Background(), "t-1", "slow.query", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.DegradedReason, "context deadline exceeded") {
		t.Errorf("超时应降级为 failed: %+v", res)
	}
}

func TestExecutorRateLimited(t *testing.T) {
	ft := &fakeTool{name: "lim.query", schema: Schema{RatePerSecond: 1, RateBurst: 1}}
	reg := NewRegistry()
	if err := reg.Register(ft); err != nil {
		t.Fatal(err)
	}
	// 本地桶模式（无 Redis），burst=1：第二次调用立即被拒。
	e := NewExecutor(ExecutorDeps{Registry: reg, Limiter: ratelimit.New(ratelimit.Options{})})

	ctx := context.Background()
	if res, err := e.Execute(ctx, "t-1", "lim.query", json.RawMessage(`{}`)); err != nil || res.Status != StatusOK {
		t.Fatalf("首次应放行: %v %+v", err, res)
	}
	res, err := e.Execute(ctx, "t-1", "lim.query", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.DegradedReason, "rate_limited") {
		t.Errorf("二次应被限流: %+v", res)
	}
	if ft.calls.Load() != 1 {
		t.Errorf("被限流不应执行工具, calls=%d", ft.calls.Load())
	}
}

func TestExecutorCacheHit(t *testing.T) {
	ft := &fakeTool{name: "c.query", output: "cached-val", schema: Schema{CacheTTL: time.Minute}}
	reg := NewRegistry()
	_ = reg.Register(ft)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	e := NewExecutor(ExecutorDeps{
		Registry: reg,
		Cache:    cache.New(cache.Options{L2: rdb, Jitter: 0}),
	})

	in := json.RawMessage(`{"q":"same"}`)
	res1, _ := e.Execute(context.Background(), "t-1", "c.query", in)
	res2, _ := e.Execute(context.Background(), "t-1", "c.query", in)
	if res1.FromCache {
		t.Error("首次不应来自缓存")
	}
	if !res2.FromCache || string(res2.Data) != string(res1.Data) {
		t.Errorf("二次应命中缓存: %+v", res2)
	}
	if ft.calls.Load() != 1 {
		t.Errorf("缓存命中不应重复执行, calls=%d", ft.calls.Load())
	}
}

func TestExecutorFailedNotCached(t *testing.T) {
	ft := &fakeTool{name: "f.query", failFirst: 1, output: "v", schema: Schema{Idempotent: false, CacheTTL: time.Minute}}
	e, _ := newExecutor(t, ft)

	in := json.RawMessage(`{}`)
	res1, _ := e.Execute(context.Background(), "t-1", "f.query", in)
	if res1.Status != StatusFailed {
		t.Fatalf("首次应失败: %+v", res1)
	}
	res2, _ := e.Execute(context.Background(), "t-1", "f.query", in)
	if res2.Status != StatusOK || res2.FromCache {
		t.Errorf("失败结果不得缓存: %+v", res2)
	}
}

func TestRegistryDuplicate(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&fakeTool{name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&fakeTool{name: "dup"}); err == nil {
		t.Fatal("重复注册应报错")
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "dup" {
		t.Errorf("Names = %v", names)
	}
}

func TestSemaphoreCap(t *testing.T) {
	// 两个慢工具调用，并发上限 1 → 串行，总时长 >= 2*sleep。
	ft := &fakeTool{name: "s.query", sleep: 100 * time.Millisecond, schema: Schema{Concurrency: 1}}
	e, _ := newExecutor(t, ft)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Execute(context.Background(), "t-1", "s.query", json.RawMessage(`{}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("并发闸未生效，总耗时 %v", d)
	}
}
