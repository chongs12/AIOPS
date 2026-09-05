package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestCache 构造带 miniredis L2 的缓存。
func newTestCache(t *testing.T) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(Options{
		L1Size:      16,
		L1TTL:       50 * time.Millisecond,
		L2:          rdb,
		L2Timeout:   100 * time.Millisecond,
		NegativeTTL: 40 * time.Millisecond,
		Jitter:      0.0, // 测试关闭抖动保证确定性
	}), mr
}

func TestKeyConstructor(t *testing.T) {
	if got := Key("prom", "query", "abc"); got != "aiops:prom:query:abc" {
		t.Errorf("Key = %q", got)
	}
}

func TestSetGetDelete(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	c.Set(ctx, "k1", []byte("v1"), time.Minute)
	if v, ok := c.Get(ctx, "k1"); !ok || string(v) != "v1" {
		t.Errorf("Get = (%q,%v)", v, ok)
	}

	c.Delete(ctx, "k1")
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Error("删除后不应命中")
	}
}

func TestGetOrLoadCachesResult(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("loaded"), nil
	}
	v, fromCache, err := c.GetOrLoad(ctx, "k", time.Minute, load)
	if err != nil || fromCache || string(v) != "loaded" {
		t.Fatalf("首次: v=%q fromCache=%v err=%v", v, fromCache, err)
	}
	v, fromCache, err = c.GetOrLoad(ctx, "k", time.Minute, load)
	if err != nil || !fromCache || string(v) != "loaded" {
		t.Fatalf("二次: v=%q fromCache=%v err=%v", v, fromCache, err)
	}
	if calls.Load() != 1 {
		t.Errorf("load 执行 %d 次, want 1", calls.Load())
	}
}

func TestGetOrLoadNegativeSentinel(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, nil // 确认不存在
	}
	v, fromCache, err := c.GetOrLoad(ctx, "neg", time.Minute, load)
	if err != nil || fromCache || v != nil {
		t.Fatalf("首次负缓存: v=%v fromCache=%v err=%v", v, fromCache, err)
	}
	// 哨兵 TTL 内二次调用不应再回源。
	_, _, _ = c.GetOrLoad(ctx, "neg", time.Minute, load)
	if calls.Load() != 1 {
		t.Errorf("空值哨兵未生效，load 执行 %d 次", calls.Load())
	}
	st := c.Stats()
	if st.NegativeHits != 1 {
		t.Errorf("NegativeHits = %d, want 1", st.NegativeHits)
	}
}

func TestGetOrLoadSingleflight(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) ([]byte, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond) // 拉大回源窗口
		return []byte("merged"), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, err := c.GetOrLoad(ctx, "sf", time.Minute, load)
			if err != nil || string(v) != "merged" {
				t.Errorf("并发结果错误: v=%q err=%v", v, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("singleflight 未合并回源，load 执行 %d 次", calls.Load())
	}
}

func TestGetOrLoadErrorNotCached(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	loadErr := func(context.Context) ([]byte, error) { return nil, errBoom }
	if _, _, err := c.GetOrLoad(ctx, "e", time.Minute, loadErr); err == nil {
		t.Fatal("应透传错误")
	}
	// 错误结果不缓存：修正后可再次加载。
	if _, _, err := c.GetOrLoad(ctx, "e", time.Minute, func(context.Context) ([]byte, error) {
		return []byte("ok"), nil
	}); err != nil {
		t.Fatalf("错误不应被缓存: %v", err)
	}
}

var errBoom = boom{}

type boom struct{}

func (boom) Error() string { return "boom" }

func TestL2FailureDegradesToMiss(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()

	c.Set(ctx, "k", []byte("v"), time.Minute)
	mr.Close() // 模拟 L2 故障

	// L1 仍命中。
	if v, ok := c.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Errorf("L1 应命中: (%q,%v)", v, ok)
	}
	// L1 淘汰后 L2 故障 → miss，不报错。
	c.l1.Remove("k")
	if _, ok := c.Get(ctx, "k"); ok {
		t.Error("L2 故障应按 miss 处理")
	}
	st := c.Stats()
	if st.L2Errors == 0 {
		t.Error("应统计 L2 错误")
	}
}

func TestL1OnlyMode(t *testing.T) {
	c := New(Options{L1Size: 8, L1TTL: 30 * time.Millisecond})
	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), time.Minute)
	if v, ok := c.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Errorf("仅 L1 模式应命中: (%q,%v)", v, ok)
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Error("L1 TTL 过期后不应命中")
	}
}

func TestJitterTTLBounds(t *testing.T) {
	c := New(Options{Jitter: 0.10})
	base := 10 * time.Second
	lo, hi := 9*time.Second, 11*time.Second
	for i := 0; i < 200; i++ {
		got := c.jitterTTL(base)
		if got < lo || got > hi {
			t.Fatalf("抖动越界: %v", got)
		}
	}
}

func TestVersioner(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	v := NewVersioner(c.l2)

	if got := v.Version(ctx, "dom"); got != 0 {
		t.Errorf("未 bump 前版本应为 0, got %d", got)
	}
	if _, err := v.BumpVersion(ctx, "dom"); err != nil {
		t.Fatalf("BumpVersion: %v", err)
	}
	k1 := v.VersionedKey(ctx, "dom", "id1")
	if k1 != "aiops:dom:v1:id1" {
		t.Errorf("VersionedKey = %q", k1)
	}
	if _, err := v.BumpVersion(ctx, "dom"); err != nil {
		t.Fatalf("BumpVersion: %v", err)
	}
	k2 := v.VersionedKey(ctx, "dom", "id1")
	if k2 != "aiops:dom:v2:id1" {
		t.Errorf("bump 后 VersionedKey = %q", k2)
	}
}

func TestVersionerRedisDownFallsBackToLocal(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()
	v := NewVersioner(c.l2)

	if _, err := v.BumpVersion(ctx, "dom"); err != nil {
		t.Fatalf("BumpVersion: %v", err)
	}
	mr.Close() // 模拟 Redis 故障

	// 本地 5s 缓存内直接可用。
	if got := v.Version(ctx, "dom"); got != 1 {
		t.Errorf("本地缓存版本 = %d, want 1", got)
	}
	if k := v.VersionedKey(ctx, "dom", "id"); k != "aiops:dom:v1:id" {
		t.Errorf("故障时 VersionedKey = %q", k)
	}
}
