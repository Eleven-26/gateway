package balancer

import (
	"sync"
	"sync/atomic"
	"time"

	"gwlab/internal/config"
)

// NodeStat 一个节点的运行时快照（/metrics 用）。
type NodeStat struct {
	Addr     string
	InFlight int64 // 当前在途请求数
	Failures int64 // 累计失败次数（只给指标看，不参与判定）
	Ejected  bool  // 当前是否被摘除
}

// health 四种均衡算法**共享**的节点运行时状态：在途计数 + 连续失败 + 被动摘除 + 探测租约。
//
// 为什么值得做：
//
//   - 改造前 least_conn 只数在途、round_robin 什么都不看，一个坏节点会持续吃掉 1/N 流量，
//     直到**整个服务**被熔断 —— 熔断的粒度是服务（见 internal/breaker 的包注释），
//     于是「坏一个节点」被放大成「整个服务不可用」。节点级摘除把两者分开：
//     先摘单点（其余节点继续服务），只有整片都不行时才轮到熔断。
//   - 摘除的判定原料（连续失败）与在途计数在这里统一维护，所以
//     gw_upstream_inflight / gw_upstream_failures_total / gw_upstream_ejected
//     对四种算法（含 round_robin）都有意义，而不再只有 least_conn 才有计数。
//
// 三条容易做错的地方：
//
//  1. **摘除必须有冷却与探测。** 摘了就永久不回列，等于把一次网络抖动放大成永久的容量损失；
//     这里的规则是「冷却期内跳过 → 到期只放行**一个**探测 → 探测成功才回列，失败则续期冷却」。
//     探测还要防「请求中途 panic 导致永不结算」：探测租约超过一个 Cooldown 未结算就自动作废，
//     允许下一个请求去探 —— 否则一次 panic 就让这个节点永远回不了列。
//  2. **全摘必须 fail-open。** 所有节点都在冷却时，Pick 必须回退到全量（按原算法照常返回一个），
//     否则摘除机制会把整个服务打死 —— 上游全挂时，「把请求发出去赌一把」永远好过「就地 503」。
//  3. **计数要按「连续」失败而不是累计。** 累计失败数只给指标看；判定用 streak，
//     成功一次立刻清零，否则一个长期运行的节点迟早会被历史失败数摘掉（累计值只增不减）。
//
// 并发口径：health.mu 同时是**各算法 Pick 的临界区**。这不是偷懒，而是必需 ——
// 「查健康 + 选节点 + 占用探测租约」必须是一个原子动作，否则冷却到期时两个并发请求
// 会同时被当成探测放行（探测就不再是「一个」）。least_conn 的平局轮转起点 next、
// weightedRR 的 current 权重、hashRing 的环数据也都落在这个锁里（各自算法实例私有，不会互相争抢）。
//
// Done / Report / Stats / inflight 自己处理同步，**不得在持有 h.mu 时调用**（会自锁死）。
// nodes / order 在 newHealth 之后只读，因此无锁读 map 是安全的。
type health struct {
	mu        sync.Mutex
	order     []string // 按 Service.Upstreams 的顺序，Stats 的输出顺序
	nodes     map[string]*nodeState
	failLimit int           // <=0 表示不启用摘除（fail-open）
	cooldown  time.Duration // 摘除时长，同时也是探测租约的时长
	now       func() time.Time
}

type nodeState struct {
	addr     string
	inflight atomic.Int64 // 在途请求数（Pick +1 / Done -1），四种算法共用
	failures atomic.Int64 // 累计失败（只进指标，不参与判定）

	// 以下字段由 health.mu 保护。
	streak       int       // 连续失败次数（成功一次清零）
	ejectedUntil time.Time // 摘除到期时间；零值 = 不在摘除流程中
	probing      bool      // 冷却到期后放行的那一个探测是否仍在飞
	probeStart   time.Time // 探测租约起始时刻（超过 Cooldown 未结算即作废）
}

func newHealth(ups []config.Upstream, ej config.EjectionConfig) *health {
	h := &health{
		order:     make([]string, 0, len(ups)),
		nodes:     make(map[string]*nodeState, len(ups)),
		failLimit: ej.FailThreshold,
		cooldown:  ej.Cooldown,
		now:       time.Now,
	}
	// Cooldown 配了阈值却没配（或配成 0）时兜底 1s：否则「摘除到期」与「摘除瞬间」同一时刻，
	// 节点会立刻被当成探测放行，摘除实际不生效 —— 静默失效比一个保守的默认值危险得多。
	if h.cooldown <= 0 {
		h.cooldown = time.Second
	}
	for _, u := range ups {
		if _, ok := h.nodes[u.Addr]; ok {
			continue // 同地址重复配置只记一次，避免指标出现重复行
		}
		h.order = append(h.order, u.Addr)
		h.nodes[u.Addr] = &nodeState{addr: u.Addr}
	}
	return h
}

// enabled 摘除是否启用。构造后 failLimit 不再变化，因此快路径上不加锁直接读也安全。
func (h *health) enabled() bool { return h.failLimit > 0 }

// addInflight 只记在途 +1（摘除未启用时的快路径：没有任何摘除状态要查，省掉一次互斥）。
func (h *health) addInflight(addr string) {
	if n := h.nodes[addr]; n != nil {
		n.inflight.Add(1)
	}
}

// inflight 读单个节点的在途数（atomic，无需持锁）。
func (h *health) inflight(addr string) int64 {
	if n := h.nodes[addr]; n != nil {
		return n.inflight.Load()
	}
	return 0
}

// availableLocked 报告 addr 此刻能否被选中。调用方必须持有 h.mu。
//
// 判定规则（从「不能选」的角度看只有两种情形）：
//   - 冷却未到 → 跳过；
//   - 冷却已到但上一个探测的租约还在有效期内 → 依然跳过（探测只能有一个）；
//     租约超过一个 Cooldown 仍未结算，视为那个探测请求 panic 了，作废它、放行下一个。
func (h *health) availableLocked(addr string) bool {
	if !h.enabled() {
		return true
	}
	n := h.nodes[addr]
	if n == nil || n.ejectedUntil.IsZero() {
		return true
	}
	now := h.now()
	if now.Before(n.ejectedUntil) {
		return false
	}
	return !(n.probing && now.Sub(n.probeStart) < h.cooldown)
}

// acquireLocked 记一次在途 +1；若该节点处于「冷却已到」状态，就把这次请求认领成那次唯一的探测。
// 调用方必须持有 h.mu —— 认领必须与 availableLocked 的判定在同一个临界区里。
func (h *health) acquireLocked(addr string) {
	n := h.nodes[addr]
	if n == nil {
		return
	}
	n.inflight.Add(1)
	if !h.enabled() || n.ejectedUntil.IsZero() {
		return
	}
	if now := h.now(); !now.Before(n.ejectedUntil) {
		n.probing = true
		n.probeStart = now
	}
}

// done 与 Pick 成对：在途 -1。摘除状态不受影响 —— 判定只认 Report（成功/失败是业务结论，
// 「还回计数」不是）。只 Pick 不 Done 会让计数只增不减，least_conn 会永久判定该节点最忙。
func (h *health) done(addr string) {
	n := h.nodes[addr]
	if n == nil {
		return
	}
	if n.inflight.Add(-1) < 0 {
		// 防御：配置热重载会重建均衡器（见 gateway.Reload），重建前被 Pick 的请求回到新实例上
		// Done 时就成了"多还一次"。计数变负会让 least_conn 把这个节点误判成最闲，
		// 所以在 0 处夹住 —— 宁可少算在途，也不要让调度被负数带偏。
		n.inflight.Store(0)
	}
}

// report 结算一次请求结果，驱动节点级被动摘除。
//
//   - 成功：连续失败清零 + 立即回列（探测成功即视为节点恢复，不必等冷却走完）；
//   - 失败：累计失败 +1（指标），连续失败 streak +1；streak 达到阈值就摘到 now+Cooldown。
//     已经被摘除的节点再失败会**续期**（streak 不清零，所以必然 >= 阈值）——
//     这就是「探测失败 → 重新冷却」的实现，避免探测失败一次就混回池子。
//   - FailThreshold == 0：fail-open，只记累计失败，永不摘除。
func (h *health) report(addr string, ok bool) {
	n := h.nodes[addr]
	if n == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		n.streak = 0
		n.probing = false
		n.ejectedUntil = time.Time{}
		return
	}
	n.failures.Add(1)
	if !h.enabled() {
		return
	}
	n.streak++
	if n.streak >= h.failLimit {
		n.ejectedUntil = h.now().Add(h.cooldown)
		n.probing = false
	}
}

// stats 按 Service.Upstreams 的顺序返回节点快照。
//
// Ejected 的口径是「此刻 Pick 会不会跳过它」：冷却期内、以及冷却已到但探测在飞时为 true；
// 冷却已到且没有探测在飞时为 false（它已经可以被当作探测放出去了，此时它不算「被摘」）。
func (h *health) stats() []NodeStat {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]NodeStat, 0, len(h.order))
	for _, addr := range h.order {
		n := h.nodes[addr]
		out = append(out, NodeStat{
			Addr:     addr,
			InFlight: n.inflight.Load(),
			Failures: n.failures.Load(),
			Ejected:  !h.availableLocked(addr),
		})
	}
	return out
}
