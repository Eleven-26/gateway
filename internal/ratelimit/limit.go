// Package ratelimit 实现 ③ 限流：令牌桶。
//
// 桶的两条不变式：
//
//	① 令牌以固定速率补充，桶容量封顶（容量 = 允许的突发量）；
//	② 「补充」不是定时任务，而是**按需惰性计算**——
//	   记录 last 时刻的令牌数，下次请求时按经过的时间补。这样空闲的连接不占资源。
//
// 放在网关上的关键问题是「按什么维度限流」：IP 粒度会误伤 NAT 后的整栋楼，
// 用户粒度需要在鉴权之后才能拿到身份 —— 所以限流必须挂在鉴权下游。
package ratelimit

import (
	"sync"
	"time"

	"gwlab/internal/config"
)

// TokenBucket 单个桶。
type TokenBucket struct {
	mu     sync.Mutex
	rate   float64 // 每秒补充的令牌
	burst  float64 // 桶容量
	tokens float64
	last   time.Time
	seen   time.Time // 最后一次使用时间，用于回收空闲桶
}

func newBucket(rate float64, burst int) *TokenBucket {
	now := time.Now()
	return &TokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now, seen: now}
}

// Allow 取 n 个令牌。返回是否放行，以及如果被拒还需要等多久（用于 Retry-After）。
func (b *TokenBucket) Allow(n float64) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	// ① 惰性补充
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	b.seen = now

	// ② 够就扣，不够就拒
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	need := n - b.tokens
	if b.rate <= 0 {
		return false, 0
	}
	return false, time.Duration(need / b.rate * float64(time.Second))
}

// Limiter 按 key 管理一批令牌桶。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*TokenBucket
	ttl     time.Duration
}

// NewLimiter 创建限流器并启动空闲桶回收协程。
func NewLimiter() *Limiter {
	l := &Limiter{buckets: map[string]*TokenBucket{}, ttl: 3 * time.Minute}
	go l.gc()
	return l
}

// Bucket 取（或建）某个 key 的桶。key 形如 "route:order-prefix|user:u-1"。
func (l *Limiter) Bucket(key string, p config.LimitPolicy) *TokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		return b
	}
	b := newBucket(p.RatePerSec, p.Burst)
	l.buckets[key] = b
	return b
}

// Len 返回当前桶数量（观测用）。
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// gc 回收长期空闲的桶 —— 不限流回收的话，每个 IP 一个桶会把内存吃光（这是限流器最常见的泄漏点）。
func (l *Limiter) gc() {
	for range time.Tick(time.Minute) {
		cut := time.Now().Add(-l.ttl)
		l.mu.Lock()
		for k, b := range l.buckets {
			b.mu.Lock()
			idle := b.seen.Before(cut)
			b.mu.Unlock()
			if idle {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// Key 决定限流维度：已登录按用户，未登录回落到 IP。
func Key(routeName, subject, ip string) string {
	if subject != "" && subject != "anonymous" {
		return "route:" + routeName + "|user:" + subject
	}
	return "route:" + routeName + "|ip:" + ip
}
