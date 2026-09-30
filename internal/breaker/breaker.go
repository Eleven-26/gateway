// Package breaker 实现 ③ 熔断器：closed → open → half-open 三态机。
//
// 限流是「保护自己不被压垮」，熔断是「保护自己不被上游拖死」——两者方向相反：
//
//	· 限流看的是**入口速率**；
//	· 熔断看的是**上游错误率**。
//
// 三条容易做错的地方：
//
//	① 打开后必须**快速失败**（不再发起真实调用），否则熔断没有任何意义；
//	② 必须经 half-open 探测再恢复，不能超时后直接回到 closed ——
//	   否则一个还没恢复的上游会被瞬间打满，反复"雪崩—熔断"；
//	③ 失败样本要**滑动窗口**，不能用累计计数，否则一次历史抖动会永久影响判定。
//
// 熔断粒度是「服务」而非「节点」：同一物理节点被多个服务共享时互不干扰。
package breaker

import (
	"errors"
	"sync"
	"time"

	"gwlab/internal/config"
)

// State 熔断器状态。
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	default:
		return "half-open"
	}
}

// ErrOpen 表示熔断器打开、本次调用被快速失败。
var ErrOpen = errors.New("熔断器打开，快速失败")

// Breaker 一个服务的熔断器。
type Breaker struct {
	mu  sync.Mutex
	cfg config.BreakerConfig

	state    State
	window   []bool // 环形窗口，true 表示失败
	idx      int
	filled   int
	failures int

	openedAt     time.Time
	halfInFlight int
	halfOK       int

	// 观测用
	lastTrip time.Time

	// OnTrip 在**本地**跳闸（不是采纳远端状态）时被调用，参数是"打开到什么时候"。
	// 多副本部署时由 gateway 接上共享状态存储，把这次跳闸发布出去（审计 C4）。
	// ⚠️ 只有一个方向：本地跳闸 → 发布。采纳远端状态时**不**回调，否则两个副本会互相
	// 转发同一件事，形成发布风暴（A 发布 → B 采纳 → B 又发布 → A 采纳 → …）。
	OnTrip func(until time.Time)
}

// OpenUntil 返回"当前打开到什么时候"；未打开时返回零值。
// 多副本部署时用它把状态发布出去（审计 C4）。
func (b *Breaker) OpenUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != StateOpen {
		return time.Time{}
	}
	return b.openedAt.Add(b.cfg.OpenFor)
}

// ForceOpenUntil 采纳远端决定的打开状态：在 until 之前一律快速失败（审计 C4）。
//
// 实现方式是把 openedAt 往回拨到 `until - OpenFor`，这样：
//   - 现在 < until 时，冷却判断 `since(openedAt) < OpenFor` 成立 → 继续拒绝；
//   - 到了 until，冷却自然到期 → 进入半开，按正常的单探测流程恢复。
//
// 这样"远端打开"与"本地打开"共用同一套恢复逻辑，不需要第二套状态机（少一套状态就少一类 bug）。
// 若本地已经是打开状态且时间更晚，则不改（就晚不就早）。
func (b *Breaker) ForceOpenUntil(until time.Time) {
	if until.IsZero() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && !b.openedAt.Add(b.cfg.OpenFor).Before(until) {
		return // 本地已经打开到更晚，不用缩短
	}
	b.state = StateOpen
	b.openedAt = until.Add(-b.cfg.OpenFor)
	b.halfInFlight, b.halfOK = 0, 0
}

// New 构造熔断器。WindowSize / OpenFor / HalfOpenMax 为零时取默认值；
// ⚠️ FailRatio 与 MinRequests 不兜底，零值语义见 config.BreakerConfig 的说明。
func New(cfg config.BreakerConfig) *Breaker {
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = 10
	}
	if cfg.OpenFor <= 0 {
		cfg.OpenFor = 3 * time.Second
	}
	if cfg.HalfOpenMax <= 0 {
		cfg.HalfOpenMax = 1
	}
	return &Breaker{cfg: cfg, window: make([]bool, cfg.WindowSize)}
}

// Allow 判断这次请求能不能发出去。
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateOpen:
		// 冷却结束 → 进入半开，放少量探测流量
		if time.Since(b.openedAt) >= b.cfg.OpenFor {
			b.toHalfOpenLocked()
			b.halfInFlight++
			return nil
		}
		return ErrOpen // ① 快速失败

	case StateHalfOpen:
		if b.halfInFlight >= b.cfg.HalfOpenMax {
			return ErrOpen
		}
		b.halfInFlight++
		return nil

	default:
		return nil
	}
}

// Report 上报这次调用结果。
//
// ⚠️ OnTrip 回调在**释放锁之后**才触发（见下面 report 的拆分）：跳闸往往要通知别的副本，
// 而在持锁时做网络 IO 会把该服务的所有请求一起卡住 —— 这类"持锁回调"是并发代码里
// 最隐蔽的故障源之一，所以宁可多拆一个函数。
func (b *Breaker) Report(success bool) {
	trip := b.report(success)
	if !trip.IsZero() {
		if cb := b.OnTrip; cb != nil {
			cb(trip)
		}
	}
}

// report 在锁内完成状态迁移；若本次**本地跳闸**，返回"打开到什么时候"（否则零值）。
func (b *Breaker) report(success bool) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		// ② 探测结果决定去 closed 还是回 open
		if b.halfInFlight > 0 {
			b.halfInFlight--
		}
		if !success {
			return b.toOpenLocked()
		}
		b.halfOK++
		if b.halfOK >= b.cfg.HalfOpenMax {
			b.toClosedLocked()
		}
		return time.Time{}
	}

	// closed：滑动窗口统计
	if b.window[b.idx] {
		b.failures--
	}
	b.window[b.idx] = !success
	if !success {
		b.failures++
	}
	b.idx = (b.idx + 1) % len(b.window)
	if b.filled < len(b.window) {
		b.filled++
	}
	// ③ 样本足够、**确实有过失败**、且失败率超阈值才打开。
	//
	// ⚠️ `b.failures > 0` 这一条不能省：`FailRatio` 的零值语义是"任何失败率都达阈值"，
	// 而**零失败时 ratio = 0 >= 0 也成立** —— 少了这个判断，一个**成功的**请求都会把熔断器
	// 打开（本机实测踩到：没配 breaker 的服务，第一次成功请求后 `breaker=open`，
	// 随后正常流量全被 503 快速失败，而且每次半开探测成功又会立刻被下一次成功请求重新打开）。
	if b.failures > 0 && b.filled >= b.cfg.MinRequests && b.filled > 0 {
		ratio := float64(b.failures) / float64(b.filled)
		if ratio >= b.cfg.FailRatio {
			return b.toOpenLocked()
		}
	}
	return time.Time{}
}

// Snapshot 返回 (状态, 窗口内失败数, 窗口内样本数)，给指标与日志用。
func (b *Breaker) Snapshot() (State, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.failures, b.filled
}

func (b *Breaker) toOpenLocked() time.Time {
	b.state = StateOpen
	b.openedAt = time.Now()
	b.lastTrip = b.openedAt
	b.halfInFlight, b.halfOK = 0, 0
	return b.openedAt.Add(b.cfg.OpenFor)
}

func (b *Breaker) toHalfOpenLocked() {
	b.state = StateHalfOpen
	b.halfInFlight, b.halfOK = 0, 0
}

func (b *Breaker) toClosedLocked() {
	b.state = StateClosed
	b.failures = 0
	b.filled = 0
	b.idx = 0
	for i := range b.window {
		b.window[i] = false
	}
	b.halfInFlight, b.halfOK = 0, 0
}
