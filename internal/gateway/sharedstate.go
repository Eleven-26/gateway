package gateway

import (
	"time"

	"gwlab/internal/breaker"
	"gwlab/internal/config"
	"gwlab/internal/observability"
)

// 共享熔断状态：
//   - 本地跳闸 → 入队 → 后台协程 POST /breaker 发布；
//   - 周期性 GET /breaker → 把"别人已经熔断的服务"在本地也置成"打开到 T"。
//
// 两个刻意的设计点：
//  1. 发布走**队列 + 后台协程**，绝不在请求路径上做网络 IO（跳闸恰好发生在请求里）。
//     队列满时丢弃并打点 —— 丢一次发布只会让别的副本晚一个周期知道，本地熔断不受影响。
//  2. 拉取是**周期**而不是每次请求：熔断在热路径上，每请求远程问一遍既慢、又会让状态服务
//     成为瓶颈；代价是副本之间生效有一个 SyncInterval 的窗口（默认 1s）。
//
// 降级策略：读不到/发不出共享状态时，熔断器**照常按本地状态机工作**（退回单副本语义），
// 只是把失败计入 gw_shared_state_errors_total{backend="breaker"}。宁可退化成单副本，也不要因为
// 状态服务抖动而让所有请求失败。
const tripQueueSize = 64

type tripEvent struct {
	service string
	until   time.Time
}

// startSharedBreaker 启动共享熔断状态的后台同步（配置了 SharedState.URL 时才调用）。
func (g *Gateway) startSharedBreaker(cfg *config.Config, metrics *observability.Metrics) {
	store := breaker.NewHTTPStore(cfg.SharedState.URL, time.Duration(cfg.SharedState.Timeout))
	store.OnError(func(error) { metrics.IncStateError("breaker") })
	g.bshare = store
	g.tripCh = make(chan tripEvent, tripQueueSize)

	interval := time.Duration(cfg.SharedState.SyncInterval)
	if interval <= 0 {
		interval = time.Second // 默认 1s：足够快，又不至于把状态服务打成热点
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-g.stop:
				return
			case ev := <-g.tripCh:
				_ = g.bshare.Publish(ev.service, ev.until) // 失败已由 OnError 计数
			case <-tick.C:
				g.adoptSharedBreakerState()
			}
		}
	}()
}

// adoptSharedBreakerState 把共享状态采纳到本地熔断器（Task：一个副本学到的结论，其它副本也生效）。
func (g *Gateway) adoptSharedBreakerState() {
	if g.bshare == nil {
		return
	}
	snap, err := g.bshare.Snapshot()
	if err != nil {
		return // 读不到就退回本地状态机；错误已在 OnError 里计数
	}
	services := g.snapshot().cfg.Services
	now := time.Now()
	for name, until := range snap {
		svc := services[name]
		if svc == nil || !until.After(now) {
			continue // 配置里没有这个服务（可能是别的副本的旧配置），或已经过期
		}
		g.breakerFor(svc).ForceOpenUntil(until)
	}
}

// publishTrip 把一次本地跳闸放进发布队列。**非阻塞**：状态服务再慢也不能拖住请求。
func (g *Gateway) publishTrip(service string, until time.Time) {
	select {
	case g.tripCh <- tripEvent{service: service, until: until}:
	default:
		if g.metrics != nil {
			g.metrics.IncStateError("breaker-publish-dropped")
		}
	}
}
