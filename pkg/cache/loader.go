package cache

import (
	"context"
	"time"
)

// GetOrLoad 读穿透加载（三防核心）：
//   - 命中（含空值哨兵）直接返回，fromCache=true；
//   - 未命中时 singleflight 合并同 key 并发回源（防击穿），load 只执行一次；
//   - load 返回 (nil, nil) 视为「确认不存在」，写空值哨兵防穿透；
//   - load 返回错误不缓存、向上透传；
//   - Set 内部对 TTL 做随机抖动（防雪崩）。
//
// 注意：合并等待的调用方共享首个请求的 ctx；若首个请求被取消，等待方会得到
// 相同的取消错误。对取消语义敏感的调用方应传入自身派生的、可独立取消的 ctx。
func (c *Cache) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load func(context.Context) ([]byte, error)) ([]byte, bool, error) {
	if v, ok := c.Get(ctx, key); ok {
		if IsNegative(v) {
			c.negativeHits.Add(1)
			return nil, true, nil
		}
		return v, true, nil
	}

	v, err, _ := c.sf.Do(key, func() (any, error) {
		val, err := load(ctx)
		if err != nil {
			return nil, err // 出错不缓存，直接透传
		}
		c.loads.Add(1)
		if val == nil {
			c.Set(ctx, key, negativeSentinel, c.negTTL)
			return nil, nil
		}
		c.Set(ctx, key, val, ttl)
		return val, nil
	})
	if err != nil {
		return nil, false, err
	}
	b, _ := v.([]byte)
	return b, false, nil
}
