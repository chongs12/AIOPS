package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeClock 可手动推进的时钟（秒，浮点）。
type fakeClock struct {
	mu  sync.Mutex
	now float64
}

func (f *fakeClock) Now() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now += d.Seconds()
}

// newTestLimiter 构造接 miniredis 的限流器与可控时钟。
func newTestLimiter(t *testing.T) (*Limiter, *miniredis.Miniredis, *fakeClock) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	clock := &fakeClock{}
	return New(Options{RDB: rdb, Timeout: 100 * time.Millisecond, NowSec: clock.Now}), mr, clock
}

func TestRedisBucketBurstAndRefill(t *testing.T) {
	l, _, clock := newTestLimiter(t)
	ctx := context.Background()

	// burst=5：前 5 次放行，第 6 次拒绝。
	for i := 0; i < 5; i++ {
		if d := l.Allow(ctx, "k", 1, 5); !d.Allowed || d.Degraded {
			t.Fatalf("第 %d 次: %+v", i+1, d)
		}
	}
	if d := l.Allow(ctx, "k", 1, 5); d.Allowed {
		t.Fatalf("超过 burst 应拒绝: %+v", d)
	} else if d.RetryAfter <= 0 {
		t.Fatalf("拒绝时应给 RetryAfter: %+v", d)
	}

	// 推进 2 秒 → 补充 2 个令牌。
	clock.Advance(2 * time.Second)
	for i := 0; i < 2; i++ {
		if d := l.Allow(ctx, "k", 1, 5); !d.Allowed {
			t.Fatalf("补充后第 %d 次应放行: %+v", i+1, d)
		}
	}
	if d := l.Allow(ctx, "k", 1, 5); d.Allowed {
		t.Fatal("2 个令牌耗尽后应拒绝")
	}
}

func TestRedisBucketConcurrentExactCount(t *testing.T) {
	l, _, _ := newTestLimiter(t)
	ctx := context.Background()

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow(ctx, "cc", 1, 10).Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 10 {
		t.Errorf("burst=10 并发下应恰好放行 10, got %d", got)
	}
}

func TestBucketKeyHasTTL(t *testing.T) {
	l, mr, _ := newTestLimiter(t)
	ctx := context.Background()
	_ = l.Allow(ctx, "ttlkey", 1, 5)
	if mr.TTL("ttlkey") <= 0 {
		t.Error("桶 key 应带 TTL 自动回收")
	}
}

func TestDegradedToLocalOnRedisFailure(t *testing.T) {
	l, mr, _ := newTestLimiter(t)
	ctx := context.Background()

	// 先正常放行一次（Redis 路径）。
	if d := l.Allow(ctx, "deg", 1, 3); !d.Allowed || d.Degraded {
		t.Fatalf("正常路径: %+v", d)
	}
	mr.Close() // 模拟 Redis 故障

	// 降级到本地桶：标记 Degraded，且本地桶自身仍执行限流（burst=3）。
	for i := 0; i < 3; i++ {
		if d := l.Allow(ctx, "deg", 1, 3); !d.Allowed || !d.Degraded {
			t.Fatalf("本地兜底第 %d 次: %+v", i+1, d)
		}
	}
	if d := l.Allow(ctx, "deg", 1, 3); d.Allowed || !d.Degraded {
		t.Fatalf("本地桶超 burst 应拒绝: %+v", d)
	}
}

func TestLocalOnlyMode(t *testing.T) {
	clock := &fakeClock{}
	l := New(Options{NowSec: clock.Now})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if d := l.Allow(ctx, "loc", 1, 3); !d.Allowed {
			t.Fatalf("本地桶第 %d 次: %+v", i+1, d)
		}
	}
	if d := l.Allow(ctx, "loc", 1, 3); d.Allowed {
		t.Fatal("本地桶超 burst 应拒绝")
	}
	clock.Advance(time.Second)
	if d := l.Allow(ctx, "loc", 1, 3); !d.Allowed {
		t.Fatalf("时钟推进后应放行: %+v", d)
	}
}

func TestInvalidRuleFailSafe(t *testing.T) {
	l, _, _ := newTestLimiter(t)
	ctx := context.Background()
	if d := l.Allow(ctx, "bad", 0, 0); !d.Allowed {
		t.Errorf("非法规则应 fail-safe 放行: %+v", d)
	}
	if d := l.Allow(ctx, "bad", -1, 5); !d.Allowed {
		t.Errorf("负 rate 应 fail-safe 放行: %+v", d)
	}
}

func TestSemaphore(t *testing.T) {
	s := NewSemaphore(2)
	ctx := context.Background()

	r1, err := s.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.TryAcquire(); ok {
		t.Fatal("满闸 TryAcquire 应失败")
	}

	// 等待超时。
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(tctx); err == nil {
		t.Fatal("占满时应超时")
	}

	r1() // 幂等释放
	r1()
	r3, ok := s.TryAcquire()
	if !ok {
		t.Fatal("释放后应可获取")
	}
	r3()
	r2()
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func TestSemaphoreUnlimited(t *testing.T) {
	s := NewSemaphore(0)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := s.Acquire(ctx); err != nil {
			t.Fatalf("不限并发不应报错: %v", err)
		}
	}
}
