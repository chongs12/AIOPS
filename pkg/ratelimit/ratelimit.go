// Package ratelimit 实现分布式令牌桶限流（对标 PreHire pkg/ratelimit）。
// Redis 侧用一段 Lua 在单次 EVAL 内原子完成「读余量-按时间补充-扣减-续期」，
// 桶 key 自带 TTL 自动回收（根除桶 map 无限增长）。
// 降级语义（核心，务必继承）：
//   - 限流判定设独立总超时（默认 100ms），限流绝不比业务慢；
//   - Redis 故障 → 本地令牌桶兜底（同参数）→ 仍失败 fail-open 放行并计数；
//   - 非法规则 fail-safe 放行。
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketLua 单次 EVAL 原子完成：读桶 → 按流逝时间补充令牌 → 扣减 → 带 TTL 续期。
// KEYS[1]=桶 key；ARGV: 1=rate(令牌/秒) 2=burst(容量) 3=now(秒,浮点) 4=requested
// 返回: {是否放行, 剩余令牌}
var tokenBucketLua = redis.NewScript(`
local key    = KEYS[1]
local rate    = tonumber(ARGV[1])
local burst   = tonumber(ARGV[2])
local now     = tonumber(ARGV[3])
local request = tonumber(ARGV[4])

local tokens, last
local data = redis.call('GET', key)
if data then
    local sep = string.find(data, ':')
    tokens = tonumber(string.sub(data, 1, sep - 1))
    last   = tonumber(string.sub(data, sep + 1))
else
    tokens = burst
    last   = now
end

local elapsed = math.max(0, now - last)
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
if tokens >= request then
    tokens = tokens - request
    allowed = 1
end

-- 桶 TTL 取「充满耗时*2+60s」，空闲自动回收，免 SCAN/删除
local ttl = math.ceil(burst / rate * 2 + 60)
redis.call('SET', key, string.format('%.6f', tokens) .. ':' .. string.format('%.6f', now), 'EX', ttl)
return {allowed, math.floor(tokens)}
`)

// Decision 限流判定结果；Degraded=true 表示 Redis 故障走了本地兜底或 fail-open。
type Decision struct {
	Allowed    bool
	Limit      int64 // 桶容量
	Remaining  int64 // 剩余令牌（向下取整）
	RetryAfter time.Duration
	Degraded   bool
}

// Limiter 分布式限流器。零值不可用，须 New 构造。
type Limiter struct {
	rdb     redis.UniversalClient // 可为 nil（直接本地桶模式）
	timeout time.Duration         // 限流判定总超时
	local   *localBuckets
}

// Options 限流参数。
type Options struct {
	RDB     redis.UniversalClient // 可为 nil
	Timeout time.Duration         // 判定总超时，默认 100ms
	// NowSec 供测试注入当前时间；nil 用 time.Now。
	NowSec func() float64
}

// New 构造限流器。
func New(opts Options) *Limiter {
	if opts.Timeout <= 0 {
		opts.Timeout = 100 * time.Millisecond
	}
	lb := newLocalBuckets()
	lb.now = opts.NowSec
	return &Limiter{rdb: opts.RDB, timeout: opts.Timeout, local: lb}
}

// Allow 判定一次请求是否放行。
// rate: 每秒补充令牌数；burst: 桶容量（也是初始满桶令牌数）。
func (l *Limiter) Allow(ctx context.Context, key string, rate float64, burst int64) Decision {
	if rate <= 0 || burst <= 0 {
		// 非法规则 fail-safe 放行（与 PreHire 语义一致）。
		return Decision{Allowed: true, Limit: burst, Degraded: true}
	}
	if l.rdb == nil {
		return l.local.allow(key, rate, burst)
	}

	cctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	now := nowSec(l.local.now)
	res, err := tokenBucketLua.Run(cctx, l.rdb, []string{key},
		rate, burst, now, 1,
	).Slice()
	if err == nil && len(res) == 2 {
		allowed, _ := res[0].(int64)
		remain, _ := res[1].(int64)
		d := Decision{Allowed: allowed == 1, Limit: burst, Remaining: remain}
		if !d.Allowed {
			d.RetryAfter = retryAfter(rate, burst, remain)
		}
		return d
	}

	// Redis 故障 → 本地桶兜底（同参数）。
	slog.Debug("ratelimit Redis 不可用，降级本地桶", "key", key, "err", err)
	d := l.local.allow(key, rate, burst)
	d.Degraded = true
	return d
}

// retryAfter 估算需等待多久才有令牌。
func retryAfter(rate float64, burst, remaining int64) time.Duration {
	if rate <= 0 {
		return time.Second
	}
	need := float64(1)
	if remaining < 0 {
		need = float64(1 - remaining)
	}
	d := time.Duration(need / rate * float64(time.Second))
	if d <= 0 {
		d = time.Second
	}
	return d
}

func nowSec(now func() float64) float64 {
	if now != nil {
		return now()
	}
	return float64(time.Now().UnixNano()) / 1e9
}

// localBuckets 进程内令牌桶（Redis 故障兜底）：按 key 维护 {tokens, last}，
// 惰性补充令牌，带 TTL 回收避免 map 无限增长。
type localBuckets struct {
	mu  sync.Mutex
	now func() float64
	m   map[string]localBucket
}

type localBucket struct {
	tokens    float64
	last      float64
	expiresAt time.Time
}

func newLocalBuckets() *localBuckets {
	return &localBuckets{m: make(map[string]localBucket)}
}

// sweepExpired 回收过期桶，防 map 无限增长；惰性触发（桶数超过阈值时）。
func (lb *localBuckets) sweepExpired() {
	now := time.Now()
	for k, b := range lb.m {
		if now.After(b.expiresAt) {
			delete(lb.m, k)
		}
	}
}

func (lb *localBuckets) allow(key string, rate float64, burst int64) Decision {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if len(lb.m) > 1024 {
		lb.sweepExpired()
	}
	now := nowSec(lb.now)
	b, ok := lb.m[key]
	if !ok {
		b = localBucket{tokens: float64(burst), last: now}
	}
	elapsed := math.Max(0, now-b.last)
	b.tokens = math.Min(float64(burst), b.tokens+elapsed*rate)
	b.last = now
	b.expiresAt = time.Now().Add(time.Duration(float64(burst)/rate*2+60) * time.Second)

	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	lb.m[key] = b

	d := Decision{Allowed: allowed, Limit: burst, Remaining: int64(math.Floor(b.tokens))}
	if !allowed {
		d.RetryAfter = retryAfter(rate, burst, d.Remaining)
	}
	return d
}
