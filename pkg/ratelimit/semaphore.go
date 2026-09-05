package ratelimit

import (
	"context"
)

// Semaphore 并发闸：保护昂贵资源（LLM 调用、外部工具调用）的并发上限。
// 与令牌桶（速率控制）互补：这里限制的是同一时刻的硬并发数。
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore 构造并发上限为 limit 的闸；limit<=0 视为不限。
func NewSemaphore(limit int) *Semaphore {
	if limit <= 0 {
		return &Semaphore{ch: make(chan struct{}, 1<<30)}
	}
	return &Semaphore{ch: make(chan struct{}, limit)}
}

// Acquire 获取一个并发槽位，返回释放函数；ctx 取消/超时则报错。
// 释放函数幂等（重复调用不会多还槽位）。
func (s *Semaphore) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case s.ch <- struct{}{}:
		once := &releaseOnce{ch: s.ch}
		return once.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TryAcquire 立即尝试获取；拿不到返回 false（不等待）。释放函数同样幂等。
func (s *Semaphore) TryAcquire() (release func(), ok bool) {
	select {
	case s.ch <- struct{}{}:
		once := &releaseOnce{ch: s.ch}
		return once.release, true
	default:
		return nil, false
	}
}

// Len 当前占用数。
func (s *Semaphore) Len() int { return len(s.ch) }

// Cap 并发上限。
func (s *Semaphore) Cap() int { return cap(s.ch) }

// releaseOnce 保证释放幂等。
type releaseOnce struct {
	ch   chan struct{}
	done bool
}

func (r *releaseOnce) release() {
	if r.done {
		return
	}
	r.done = true
	<-r.ch
}
