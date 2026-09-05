package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// versionTTL 版本号本地缓存时长：读 key 时不必每次都打 Redis。
const versionTTL = 5 * time.Second

// Versioner 版本号失效（对标 PreHire pkg/cache）：
// 写路径 BumpVersion（INCR 版本 key），读路径把版本号拼进 key；
// 旧版本 key 靠 TTL 自然消亡，免 SCAN 前缀删除。
// 故障语义：版本读取失败时沿用本地已知的上一次值（可能过期，最坏多一次回源），
// 绝不因 Redis 故障阻塞读路径。
type Versioner struct {
	l2 redis.UniversalClient

	mu    sync.Mutex
	local map[string]cachedVersion
}

type cachedVersion struct {
	version   int64
	fetchedAt time.Time
}

// NewVersioner 构造版本器；l2 为 nil 时永远使用版本 0（退化但可用）。
func NewVersioner(l2 redis.UniversalClient) *Versioner {
	return &Versioner{l2: l2, local: make(map[string]cachedVersion)}
}

func versionKey(domain string) string {
	return KeyPrefix + ":" + domain + ":__ver"
}

// BumpVersion 使 domain 的版本号 +1（写路径在数据变更后调用）。
func (v *Versioner) BumpVersion(ctx context.Context, domain string) (int64, error) {
	if v.l2 == nil {
		return 0, errors.New("versioner: 无 L2，版本失效不可用")
	}
	n, err := v.l2.Incr(ctx, versionKey(domain)).Result()
	if err != nil {
		return 0, fmt.Errorf("versioner INCR %s: %w", domain, err)
	}
	v.mu.Lock()
	v.local[domain] = cachedVersion{version: n, fetchedAt: time.Now()}
	v.mu.Unlock()
	return n, nil
}

// Version 读取 domain 当前版本号：本地缓存 5s 内直接用；过期才查 Redis；
// Redis 故障沿用旧值；从未有值则返回 0。
func (v *Versioner) Version(ctx context.Context, domain string) int64 {
	v.mu.Lock()
	cv, ok := v.local[domain]
	v.mu.Unlock()
	now := time.Now()
	if ok && now.Sub(cv.fetchedAt) < versionTTL {
		return cv.version
	}
	if v.l2 != nil {
		cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		n, err := v.l2.Get(cctx, versionKey(domain)).Int64()
		cancel()
		if err == nil {
			v.remember(domain, n, now)
			return n
		}
		if !errors.Is(err, redis.Nil) {
			slog.Debug("versioner 读版本失败，沿用本地已知值", "domain", domain, "err", err)
		} else {
			v.remember(domain, 0, now) // 从未 bump 过 → 版本 0
		}
	}
	if ok {
		return cv.version
	}
	return 0
}

// VersionedKey 构造带版本号的 key：aiops:{domain}:v{N}:{parts...}。
// 版本读取故障时退化为本地已知版本，保证构造永不失败。
func (v *Versioner) VersionedKey(ctx context.Context, domain string, parts ...string) string {
	ver := v.Version(ctx, domain)
	return KeyPrefix + ":" + domain + ":" + fmt.Sprintf("v%d", ver) + ":" + strings.Join(parts, ":")
}

func (v *Versioner) remember(domain string, version int64, now time.Time) {
	v.mu.Lock()
	v.local[domain] = cachedVersion{version: version, fetchedAt: now}
	v.mu.Unlock()
}
