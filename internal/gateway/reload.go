package gateway

import (
	"fmt"
	"slices"
	"sort"

	"gwlab/internal/balancer"
	"gwlab/internal/breaker"
	"gwlab/internal/config"
)

// ReloadSummary 描述一次热重载做了什么（给日志与测试用）。
type ReloadSummary struct {
	Routes          int
	Services        int
	BreakerKept     []string // 复用原有熔断器（状态不丢）
	BalancerRebuilt []string // 算法 / 节点列表 / 摘除参数变了 → 重建（节点级摘除计数会丢，几秒内自愈）
	BalancerKept    []string // 拓扑没变 → 原样保留（在途计数与摘除状态都不丢）
	Removed         []string // 新配置里已不存在的服务
	ProxiesPruned   int      // 被清理的旧代理缓存条目
}

// String 一行打完，便于日志。
func (s *ReloadSummary) String() string {
	return fmt.Sprintf("routes=%d services=%d 熔断器复用=%d 均衡器重建=%v 拓扑未变=%v 服务删除=%v 代理清理=%d",
		s.Routes, s.Services, len(s.BreakerKept), s.BalancerRebuilt, s.BalancerKept, s.Removed, s.ProxiesPruned)
}

// Reload 用新配置原子替换运行时状态（审计 C1：配置外置 + 热重载）。
//
// 三个必须遵守的坑（`AGENTS.md` §4 记的就是它们）：
//  1. **熔断器按服务名复用，绝不重建** —— 重建会把刚打开的熔断器重置回 closed，
//     流量立刻又打到还没恢复的上游，等于把熔断白做了；
//  2. 均衡器只在「算法 / 节点列表 / 摘除参数」变了时重建 —— 重建会丢节点级摘除计数，
//     那属于可自愈状态（几秒内重新学出来），但熔断器状态不可重建，见上一条；
//  3. **代理缓存按新配置清理** —— `service|addr|strip` 的 key 只增不减，改了上游地址
//     就会留下永不释放的旧 `proxy.Proxy`（这条以前只是"小心点"，现在由代码保证）。
//
// 还有一点：这里是**原地替换**而不是新建 Gateway —— gRPC 连接池、限流桶、指标、访问日志
// 都继续用同一份，所以不需要 `Close()` 旧实例（若改成整体替换，务必先 Close，否则连接池泄漏）。
//
// 失败语义：校验/编译任何一步出错就直接返回错误，**当前快照保持不变** ——
// 宁可继续跑旧配置，也不要让网关处于"半新半旧"的状态。
func (g *Gateway) Reload(cfg *config.Config) (*ReloadSummary, error) {
	st, err := newState(cfg)
	if err != nil {
		return nil, err
	}
	old := g.st.Load()
	sum := &ReloadSummary{Routes: len(cfg.Routes), Services: len(cfg.Services)}

	g.mu.Lock()
	defer g.mu.Unlock()

	var oldServices map[string]*config.Service
	if old != nil && old.cfg != nil {
		oldServices = old.cfg.Services
	}

	for name, svc := range cfg.Services {
		if svc == nil {
			continue
		}
		if _, ok := g.breakers[name]; !ok {
			g.breakers[name] = breaker.New(svc.Breaker)
		}
		sum.BreakerKept = append(sum.BreakerKept, name)

		oldSvc := oldServices[name]
		topologyChanged := oldSvc == nil ||
			oldSvc.Balance != svc.Balance ||
			oldSvc.Ejection != svc.Ejection ||
			!slices.Equal(oldSvc.Upstreams, svc.Upstreams)
		if topologyChanged {
			g.balancers[name] = balancer.New(svc)
			sum.BalancerRebuilt = append(sum.BalancerRebuilt, name)
		} else {
			sum.BalancerKept = append(sum.BalancerKept, name)
		}
	}

	for name := range g.breakers {
		if _, ok := cfg.Services[name]; !ok {
			delete(g.breakers, name)
			sum.Removed = append(sum.Removed, name)
		}
	}
	for name := range g.balancers {
		if _, ok := cfg.Services[name]; !ok {
			delete(g.balancers, name)
		}
	}

	// 代理缓存：只保留新配置里仍然可能用到的 key
	allowed := map[string]bool{}
	for _, rt := range cfg.Routes {
		svc := cfg.Services[rt.Upstream]
		if svc == nil {
			continue
		}
		for _, up := range svc.Upstreams {
			allowed[svc.Name+"|"+up.Addr+"|"+rt.StripPrefix] = true
		}
	}
	for k := range g.proxies {
		if !allowed[k] {
			delete(g.proxies, k)
			sum.ProxiesPruned++
		}
	}

	// 最后一步才让请求看到新配置：上面任何一步失败都不会留下半成品
	g.st.Store(st)

	sort.Strings(sum.BreakerKept)
	sort.Strings(sum.BalancerRebuilt)
	sort.Strings(sum.BalancerKept)
	sort.Strings(sum.Removed)
	return sum, nil
}

// BalancerStats 返回各服务当前的均衡器快照（`/readyz` 与测试用；服务名排序）。
func (g *Gateway) BalancerStats() map[string][]balancer.NodeStat {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string][]balancer.NodeStat, len(g.balancers))
	for name, lb := range g.balancers {
		out[name] = lb.Stats()
	}
	return out
}
