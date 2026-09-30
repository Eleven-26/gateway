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
func (b *Breaker) Report(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		// ② 探测结果决定去 closed 还是回 open
		if b.halfInFlight > 0 {
			b.halfInFlight--
		}
		if !success {
			b.toOpenLocked()
			return
		}
		b.halfOK++
		if b.halfOK >= b.cfg.HalfOpenMax {
			b.toClosedLocked()
		}
		return
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
	// ③ 样本足够且失败率超阈值才打开
	if b.filled >= b.cfg.MinRequests && b.filled > 0 {
		ratio := float64(b.failures) / float64(b.filled)
		if ratio >= b.cfg.FailRatio {
			b.toOpenLocked()
		}
	}
}

// Snapshot 返回 (状态, 窗口内失败数, 窗口内样本数)，给指标与日志用。
func (b *Breaker) Snapshot() (State, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.failures, b.filled
}

func (b *Breaker) toOpenLocked() {
	b.state = StateOpen
	b.openedAt = time.Now()
	b.lastTrip = b.openedAt
	b.halfInFlight, b.halfOK = 0, 0
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
