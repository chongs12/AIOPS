// Package cache 实现两级缓存（L1 进程内 expirable LRU + L2 Redis）与读穿透 Loader。
// 三防：singleflight 合并同 key 并发回源（防击穿）、空值哨兵短 TTL（防穿透）、
// TTL 随机抖动（防雪崩）；另有版本号失效（Versioner）免 SCAN 前缀删除。
// 故障语义（核心）：缓存层任何故障都降级为 miss/直查数据源，绝不阻塞主路径，
// L2 读写设独立超时。
package cache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// KeyPrefix 所有缓存 key 的统一前缀。
const KeyPrefix = "aiops"

// negativeSentinel 空值哨兵：以 NUL 开头，正常 JSON/文本载荷不会撞上。
var negativeSentinel = []byte{0x00, 'N', 'E', 'G'}

// Key 构造统一规范的缓存 key：aiops:{domain}:{parts...}。
// 禁止在业务代码散落拼接 key，一律经由此构造器或 Versioner.VersionedKey。
func Key(domain string, parts ...string) string {
	return KeyPrefix + ":" + domain + ":" + strings.Join(parts, ":")
}

// Options 缓存参数。
type Options struct {
	L1Size      int                   // L1 容量，默认 1024
	L1TTL       time.Duration         // L1 逐条 TTL（L2 更长），默认 30s
	L2          redis.UniversalClient // L2 客户端，nil = 仅 L1 模式
	L2Timeout   time.Duration         // L2 读写超时，默认 200ms
	NegativeTTL time.Duration         // 空值哨兵 TTL，默认 30s
	Jitter      float64               // TTL 抖动比例 ±Jitter，默认 0.10
}

// entry L1 条目：expirable LRU 仅提供全局淘汰 TTL，逐条过期时刻自行判断。
type entry struct {
	val      []byte
	expireAt time.Time
}

// Cache 两级缓存。零值不可用，须 New 构造。
type Cache struct {
	l1        *expirable.LRU[string, entry]
	l1TTL     time.Duration
	l2        redis.UniversalClient
	l2Timeout time.Duration
	negTTL    time.Duration
	jitter    float64
	sf        singleflight.Group

	hitsL1       atomic.Uint64
	hitsL2       atomic.Uint64
	misses       atomic.Uint64
	loads        atomic.Uint64
	negativeHits atomic.Uint64
	l2Errors     atomic.Uint64
}

// New 构造缓存；未显式给出的参数取默认值。
func New(opts Options) *Cache {
	if opts.L1Size <= 0 {
		opts.L1Size = 1024
	}
	if opts.L1TTL <= 0 {
		opts.L1TTL = 30 * time.Second
	}
	if opts.L2Timeout <= 0 {
		opts.L2Timeout = 200 * time.Millisecond
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = 30 * time.Second
	}
	if opts.Jitter <= 0 {
		opts.Jitter = 0.10
	}
	return &Cache{
		// 全局淘汰 TTL 给一个远大于单条的兜底值，真实过期由 entry.expireAt 判断。
		l1:        expirable.NewLRU[string, entry](opts.L1Size, nil, 24*time.Hour),
		l1TTL:     opts.L1TTL,
		l2:        opts.L2,
		l2Timeout: opts.L2Timeout,
		negTTL:    opts.NegativeTTL,
		jitter:    opts.Jitter,
	}
}

// CacheStats 缓存命中统计快照（供指标导出）。
type CacheStats struct {
	HitsL1       uint64
	HitsL2       uint64
	Misses       uint64
	Loads        uint64
	NegativeHits uint64
	L2Errors     uint64
}

// Stats 返回统计快照。
func (c *Cache) Stats() CacheStats {
	return CacheStats{
		HitsL1:       c.hitsL1.Load(),
		HitsL2:       c.hitsL2.Load(),
		Misses:       c.misses.Load(),
		Loads:        c.loads.Load(),
		NegativeHits: c.negativeHits.Load(),
		L2Errors:     c.l2Errors.Load(),
	}
}

// Get 读取：L1 → L2（带独立超时）。任何故障/未命中都返回 miss，不报错。
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
	if e, ok := c.l1.Get(key); ok {
		if time.Now().After(e.expireAt) {
			c.l1.Remove(key)
		} else {
			c.hitsL1.Add(1)
			return e.val, true
		}
	}
	if c.l2 != nil {
		cctx, cancel := context.WithTimeout(ctx, c.l2Timeout)
		b, err := c.l2.Get(cctx, key).Bytes()
		cancel()
		if err == nil {
			c.hitsL2.Add(1)
			c.l1.Add(key, entry{val: b, expireAt: time.Now().Add(c.l1TTL)})
			return b, true
		}
		if !errors.Is(err, redis.Nil) {
			c.l2Errors.Add(1)
			slog.Debug("cache L2 读失败，按 miss 处理", "key", key, "err", err)
		}
	}
	c.misses.Add(1)
	return nil, false
}

// Set 写入 L1（TTL 取 min(L1TTL, ttl)）与 L2（TTL 带 ±Jitter 抖动）。
// L2 失败仅记日志与计数，绝不阻塞主路径。
func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	l1TTL := c.l1TTL
	if ttl < l1TTL {
		l1TTL = ttl
	}
	c.l1.Add(key, entry{val: val, expireAt: time.Now().Add(l1TTL)})
	if c.l2 != nil {
		cctx, cancel := context.WithTimeout(ctx, c.l2Timeout)
		err := c.l2.Set(cctx, key, val, c.jitterTTL(ttl)).Err()
		cancel()
		if err != nil {
			c.l2Errors.Add(1)
			slog.Debug("cache L2 写失败，忽略", "key", key, "err", err)
		}
	}
}

// Delete 删除 L1 与 L2；失败仅记日志。
func (c *Cache) Delete(ctx context.Context, keys ...string) {
	for _, k := range keys {
		c.l1.Remove(k)
	}
	if c.l2 != nil && len(keys) > 0 {
		cctx, cancel := context.WithTimeout(ctx, c.l2Timeout)
		if err := c.l2.Del(cctx, keys...).Err(); err != nil {
			c.l2Errors.Add(1)
			slog.Debug("cache L2 删除失败，忽略", "keys", keys, "err", err)
		}
		cancel()
	}
}

// IsNegative 判断值是否为空值哨兵。
func IsNegative(val []byte) bool {
	return bytes.Equal(val, negativeSentinel)
}

// jitterTTL 给 TTL 施加 ±jitter 的随机抖动，防止集中过期（防雪崩）。
func (c *Cache) jitterTTL(ttl time.Duration) time.Duration {
	if c.jitter <= 0 {
		return ttl
	}
	f := 1 + (rand.Float64()*2-1)*c.jitter
	if f < 0.1 {
		f = 0.1
	}
	return time.Duration(float64(ttl) * f)
}
