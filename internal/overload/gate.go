// Package overload 实现负载保护（审计 C6）：并发上限 + 有界排队 + 过载快速拒绝。
//
// 为什么值得做：网关前面没有闸门时，上游一慢，请求就会在网关里堆积 ——
// goroutine、连接、内存一起涨，最后连健康检查和本来能跑完的请求都挤不进来，
// 表现就是"网关把自己压死了"（经典的雪崩）。负载保护干两件事：
//
//  1. **限制同时在处理的请求数**：超出的排队或拒绝。宁可少服务一部分，
//     也要保证已经进来的请求能跑完 —— 这是"服务质量换吞吐"的取舍；
//  2. **排队必须有界、有时限**：无限排队只是把超时往后推，客户端看到的是"一直转圈"，
//     还不如立刻告诉它"稍后重试"（503 + Retry-After）。
//
// 与限流的区别（这两个机制经常被混为一谈）：
//
//	限流管的是**速率**（每秒允许多少），维度是"谁在用"（用户/IP/路由）；
//	负载保护管的是**并发**（此刻有多少在飞），维度是"网关和上游此刻扛不扛得住"。
//
// 速率完全正常、但上游慢了 10 倍的时候，只有并发保护能救你 —— 请求没变多，是每个都变慢了。
package overload

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Reason 是过载拒绝的原因（会写进响应头与日志，便于排障时区分两种情况）。
type Reason string

const (
	ReasonQueueFull    Reason = "queue_full"    // 队列已满，连排队都不让排
	ReasonQueueTimeout Reason = "queue_timeout" // 排了队但等到超时也没轮上
)

// ErrQueueFull / ErrQueueTimeout 与上面两个 Reason 对应。
var (
	ErrQueueFull    = errors.New("并发已满且队列已满")
	ErrQueueTimeout = errors.New("排队等待超时")
)

// Gate 并发闸门。零值不可用，请用 New 构造；nil 表示"不启用"（Acquire 会直接放行）。
type Gate struct {
	slots    chan struct{} // 容量 = 并发上限；channel 的等待队列天然 FIFO
	maxQueue int64         // 允许同时排队的请求数
	waiting  atomic.Int64
	inflight atomic.Int64
}

// New 创建闸门。maxInflight <= 0 返回 nil（表示不启用负载保护）。
//
// maxQueue 的含义：0 = 不排队（并发满了立刻 503）。排队发生在"并发已满"的瞬间，
// 所以它实际能吸收的是**突发**，而不是持续的过载。
func New(maxInflight, maxQueue int) *Gate {
	if maxInflight <= 0 {
		return nil
	}
	if maxQueue < 0 {
		maxQueue = 0
	}
	return &Gate{slots: make(chan struct{}, maxInflight), maxQueue: int64(maxQueue)}
}

// Limit 返回并发上限（未启用时返回 0）。
func (g *Gate) Limit() int {
	if g == nil {
		return 0
	}
	return cap(g.slots)
}

// Stats 返回 (在处理中的请求数, 正在排队的请求数)，给指标用。
func (g *Gate) Stats() (inflight, waiting int64) {
	if g == nil {
		return 0, 0
	}
	return g.inflight.Load(), g.waiting.Load()
}

// Acquire 取一个并发槽位。
//
// 返回的 release 函数**必须**被调用（`defer release()`）：漏掉一次就等于永久少一个并发额度，
// 最后闸门会自己把自己关死 —— 这也是为什么 release 内部用 sync.Once 兜住重复调用。
//
// ctx 用于排队超时：调用方应传入带 QueueTimeout 的 context。
func (g *Gate) Acquire(ctx context.Context) (release func(), reason Reason, err error) {
	if g == nil {
		return func() {}, "", nil
	}

	// ① 先试非阻塞地拿到槽位：绝大多数请求走这条路，不进排队逻辑
	select {
	case g.slots <- struct{}{}:
		g.inflight.Add(1)
		return g.releaseFunc(), "", nil
	default:
	}

	// ② 需要排队：先看队列还有没有位置。
	// ⚠️ 这个计数是"准入控制"而不是精确的队列长度（下面 select 成功后立刻减回去），
	// 所以高并发下允许轻微超发；换来的是不用再加一把锁 —— 这里的误差不影响保护效果。
	if g.waiting.Add(1) > g.maxQueue {
		g.waiting.Add(-1)
		return nil, ReasonQueueFull, ErrQueueFull
	}
	defer g.waiting.Add(-1)

	select {
	case g.slots <- struct{}{}:
		g.inflight.Add(1)
		return g.releaseFunc(), "", nil
	case <-ctx.Done():
		return nil, ReasonQueueTimeout, ErrQueueTimeout
	}
}

// releaseFunc 返回幂等的归还函数。
func (g *Gate) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.slots
			g.inflight.Add(-1)
		})
	}
}
