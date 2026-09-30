// Package balancer 实现 ④ 负载均衡：轮询 / 加权轮询 / 最少连接 / 一致性哈希。
//
// 「选哪个节点」看着简单，四种算法的差别只在两处：
//
//	· **是否需要会话粘性** —— 需要就得上哈希（同一 key 恒定落到同一节点）；
//	· **节点能力是否相同** —— 不同就得上权重（或最少连接）；
//	· **平局怎么选** —— least_conn 的在途数**经常相等**（低并发时全是 0），
//	  若按 map 迭代顺序取第一个最小值，实测 78% 的请求会砸在列表第一个节点上
//	  （Go 小 map 的迭代起点随机，但偏移分布让首元素占优，见 leastConn.Pick 的注释）。
//
// 还有一件四种算法都必须做的事：**跳过已经坏掉的节点**。选路算法只回答「谁更合适」，
// 不回答「谁还活着」—— 那件事交给四种算法共享的 health（见 health.go）：
// 连续失败到阈值就把该节点摘除一段时间，到期只放行一个探测，探测成功才回列。
// 没有它，一个坏节点会一直吃掉 1/N 流量，直到整个服务被熔断（审计 P1-2）。
//
// 一致性哈希的关键**不是**「哈希」，而是两件事：
//
//	· **哈希函数的雪崩性** —— 雪崩差的哈希会让虚拟节点全部挤在环的一处，加了等于没加（见 hashOf 的注释）；
//	· **虚拟节点** —— 3 个真实节点直接上环时分布极不均（实测最大偏差 90.7%），
//	  每个节点映射成 150 个虚拟节点后降到 3.9%，且扩容时只有约 1/N 的 key 需要迁移。
package balancer

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"

	"gwlab/internal/config"
)

// ErrNoUpstream 表示服务下没有可用节点。
var ErrNoUpstream = errors.New("上游没有可用节点")

// Balancer 选节点策略。
type Balancer interface {
	Pick(key string) (*config.Upstream, error)
	Done(addr string)                 // 与 Pick 成对：在途计数 -1
	Report(addr string, success bool) // 上报一次结果：驱动节点级被动摘除
	Stats() []NodeStat                // 节点级快照（按 Service.Upstreams 的顺序）
	Name() string
}

// New 按服务配置构造对应的均衡器。
//
// 注意两点：
//   - least_conn 需要预置节点列表，四种算法的节点表（在途计数 / 连续失败 / 摘除状态）
//     都在这里由 newHealth 一次性建好 —— 若把这步漏在外部，计数器永远是空的，
//     Pick 会直接返回 ErrNoUpstream；
//   - 四种算法**共用同一个 health 实例**（每个 Balancer 一个），所以 /metrics 的
//     gw_upstream_inflight 对 round_robin 同样有意义。
func New(s *config.Service) Balancer {
	h := newHealth(s.Upstreams, s.Ejection)
	switch s.Balance {
	case "weighted":
		return newWeightedRR(s.Upstreams, h)
	case "least_conn":
		return newLeastConn(s.Upstreams, h)
	case "consistent_hash":
		return newHashRing(s.Upstreams, h)
	default:
		return &roundRobin{ups: s.Upstreams, h: h}
	}
}

// ---- 轮询 ----

type roundRobin struct {
	ups []config.Upstream
	n   atomic.Uint64
	h   *health
}

func (b *roundRobin) Name() string { return "round_robin" }

// Pick 从轮转计数器开始往后找**第一个健康节点**（最多扫 n 个）。
//
// 全都摘除了就按原逻辑返回（fail-open）：宁可打到一个正在冷却的节点，也不能让整个服务
// 一个节点都选不出来 —— 那时候「发出去赌一把」好过「就地 503」（见 health 的注释）。
func (b *roundRobin) Pick(string) (*config.Upstream, error) {
	n := len(b.ups)
	if n == 0 {
		return nil, ErrNoUpstream
	}
	start := int((b.n.Add(1) - 1) % uint64(n))
	if !b.h.enabled() {
		// 没启用摘除：走无锁快路径，行为与改造前完全一致（只多记一次在途）。
		u := b.ups[start]
		b.h.addInflight(u.Addr)
		return &u, nil
	}
	b.h.mu.Lock()
	defer b.h.mu.Unlock()
	idx := -1
	for k := 0; k < n; k++ {
		if i := (start + k) % n; b.h.availableLocked(b.ups[i].Addr) {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = start // 全摘：fail-open
	}
	b.h.acquireLocked(b.ups[idx].Addr)
	u := b.ups[idx]
	return &u, nil
}

func (b *roundRobin) Done(addr string) { b.h.done(addr) }

func (b *roundRobin) Report(addr string, success bool) { b.h.report(addr, success) }

func (b *roundRobin) Stats() []NodeStat { return b.h.stats() }

// ---- 加权轮询（Nginx 的平滑算法）----
//
// 每次选「当前权重」最大的节点，选完把它减去总权重、其余节点加上自身权重。
// 这样权重 3:1 的分布是 A A B A，而不是 A A A B（后者会造成瞬时倾斜）。
type weightedRR struct {
	ups     []config.Upstream
	current []int
	total   int
	h       *health
}

func newWeightedRR(ups []config.Upstream, h *health) *weightedRR {
	total := 0
	for _, u := range ups {
		total += weightOf(u)
	}
	return &weightedRR{ups: ups, current: make([]int, len(ups)), total: total, h: h}
}

func (b *weightedRR) Name() string { return "weighted" }

// Pick 在**健康节点**里跑平滑加权（每次选当前权重最大的，选完减去参与节点的总权重）。
//
// 被摘除的节点不参与本轮，并把它的 current 清零：既不让它在冷却期间偷偷攒权重、
// 回列的瞬间连吃几发，也不让它背着负数回来（零值起步 = 公平回列）。
//
// ⚠️ 清零只发生在「有健康节点」的分支里。全被摘除时要原样退回改造前的算法（在全部节点上
// 平滑加权，fail-open）—— 若在扫描阶段无条件清零，current 每轮都从 0,0,0 起步，
// 平滑加权会退化成「永远选第一个节点」（本机实测：30 次 Pick 全部落到 a:1）。
func (b *weightedRR) Pick(string) (*config.Upstream, error) {
	if len(b.ups) == 0 {
		return nil, ErrNoUpstream
	}
	b.h.mu.Lock()
	defer b.h.mu.Unlock()

	anyHealthy := false
	for _, u := range b.ups {
		if b.h.availableLocked(u.Addr) {
			anyHealthy = true
			break
		}
	}

	best, bestIdx, healthyTotal := -1<<31, -1, 0
	if anyHealthy {
		for i, u := range b.ups {
			if !b.h.availableLocked(u.Addr) {
				b.current[i] = 0
				continue
			}
			w := weightOf(u)
			b.current[i] += w
			healthyTotal += w
			if b.current[i] > best {
				best, bestIdx = b.current[i], i
			}
		}
	} else {
		// fail-open：全被摘除 → 原逻辑（全部节点参与，权重不丢）
		best, bestIdx = -1<<31, 0
		for i, u := range b.ups {
			b.current[i] += weightOf(u)
			if b.current[i] > best {
				best, bestIdx = b.current[i], i
			}
		}
		healthyTotal = b.total
	}
	b.current[bestIdx] -= healthyTotal
	b.h.acquireLocked(b.ups[bestIdx].Addr)
	u := b.ups[bestIdx]
	return &u, nil
}

func (b *weightedRR) Done(addr string) { b.h.done(addr) }

func (b *weightedRR) Report(addr string, success bool) { b.h.report(addr, success) }

func (b *weightedRR) Stats() []NodeStat { return b.h.stats() }

// ---- 最少连接 ----

type leastConn struct {
	ups  []config.Upstream // 保留配置顺序：平局要靠它轮转，map 不够
	next int               // 下一次遍历的起点（平局轮转用），由 h.mu 保护
	h    *health           // 在途计数与摘除状态都在这里，本结构不再自带 map
}

func newLeastConn(ups []config.Upstream, h *health) *leastConn {
	return &leastConn{ups: ups, h: h}
}

func (b *leastConn) Name() string { return "least_conn" }

// Pick 挑「在途最少」的**健康**节点。
//
// ⚠️ 关键在平局：在途数相等是最常见的情况（串行流量下每次都是 0,0,0），
// 如果直接 `for addr, c := range b.conns` 取第一个最小值，胜者就由 map 迭代顺序决定。
// Go 的小 map 虽然随机化迭代起点，但偏移落点让**最先插入的节点**占优 ——
// 本机实测 1000 次全 0 平局：a=777 / b=116 / c=107，即 78% 砸在第一个节点上，
// 此时 least_conn 名存实亡（见 balancer_test.go 的平局测试）。
//
// 因此按下标遍历，且每次从 `next` 开始轮转：第一个遇到的最小值获胜 → 平局自然轮流。
// 摘除只是在这个遍历里多一个 `continue`，平局轮转的起点语义完全不变（回归测试
// TestLeastConnTieBreakIsFair 要求健康节点之间仍是 333/333/333）。
//
// 全被摘除时按同一套逻辑在**全部**节点里取最小在途（fail-open）。
func (b *leastConn) Pick(string) (*config.Upstream, error) {
	n := len(b.ups)
	if n == 0 {
		return nil, ErrNoUpstream
	}
	b.h.mu.Lock()
	defer b.h.mu.Unlock()

	bestIdx, bestN := -1, int64(1<<62)
	for k := 0; k < n; k++ {
		i := (b.next + k) % n
		addr := b.ups[i].Addr
		if !b.h.availableLocked(addr) {
			continue
		}
		if c := b.h.inflight(addr); c < bestN {
			bestN, bestIdx = c, i
		}
	}
	if bestIdx < 0 {
		// 所有节点都在冷却：回退到全量，不能因为「都不健康」就地返回 ErrNoUpstream。
		for k := 0; k < n; k++ {
			if c := b.h.inflight(b.ups[(b.next+k)%n].Addr); c < bestN {
				bestN, bestIdx = c, (b.next+k)%n
			}
		}
	}
	b.next = (bestIdx + 1) % n

	u := b.ups[bestIdx]
	b.h.acquireLocked(u.Addr)
	return &u, nil
}

// Done 回收一次在途计数 —— ⚠️ 必须与 Pick 成对调用，否则计数只增不减。
func (b *leastConn) Done(addr string) { b.h.done(addr) }

func (b *leastConn) Report(addr string, success bool) { b.h.report(addr, success) }

func (b *leastConn) Stats() []NodeStat { return b.h.stats() }

// ---- 一致性哈希（含虚拟节点）----

// vnodesPerNode 每个节点映射的虚拟节点基数，实际数量为 该值 × 节点权重。
const vnodesPerNode = 150

type hashRing struct {
	keys   []uint64          // 环上位置，升序
	owner  map[uint64]string // 位置 → 真实节点
	origin []config.Upstream
	h      *health // 环数据与摘除状态共用一把锁：选路 + 占用探测租约必须是原子的
}

func newHashRing(ups []config.Upstream, h *health) *hashRing {
	r := &hashRing{owner: map[uint64]string{}, origin: ups, h: h}
	r.rebuild(ups)
	return r
}

func (r *hashRing) rebuild(ups []config.Upstream) {
	r.keys = r.keys[:0]
	r.owner = map[uint64]string{}
	for _, u := range ups {
		for i := 0; i < vnodesPerNode*weightOf(u); i++ {
			h := hashOf(u.Addr + "#" + strconv.Itoa(i))
			r.keys = append(r.keys, h)
			r.owner[h] = u.Addr
		}
	}
	sort.Slice(r.keys, func(i, j int) bool { return r.keys[i] < r.keys[j] })
}

func (r *hashRing) Name() string { return "consistent_hash" }

// Pick 先按 key 命中环上的节点；命中节点被摘除时，沿环**顺时针**继续找下一个健康节点
// （最多绕一圈，找不到就仍用命中的那个 —— fail-open）。
//
// 注意这确实破坏了一点会话粘性：摘除期间同一个 key 会落到别的节点。这正是摘除的目的
// （总比一直打到坏节点上好），也正因为如此，摘除必须配冷却 + 探测，不能让节点长期缺席。
//
// 锁的口径：原来这里是 RLock，现在与 health 共用独占锁。理由是把「查健康 + 选节点 +
// 认领探测租约」做成一个原子动作；一致性哈希本身（MD5 + 二分）比一次锁竞争贵得多，
// 这点代价可以接受，换来的是 SetUpstreams 与 Pick 之间也不需要第二把锁。
func (r *hashRing) Pick(key string) (*config.Upstream, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if len(r.keys) == 0 {
		return nil, ErrNoUpstream
	}
	h := hashOf(key)
	// 顺时针找第一个 >= h 的虚拟节点，找不到就绕回环首
	i := sort.Search(len(r.keys), func(i int) bool { return r.keys[i] >= h })
	if i == len(r.keys) {
		i = 0
	}
	addr := r.owner[r.keys[i]]
	if !r.h.availableLocked(addr) {
		// 虚拟节点很多，这里按环位置逐个走，靠 cand != addr 去重，绕一圈即止。
		for k := 1; k < len(r.keys); k++ {
			cand := r.owner[r.keys[(i+k)%len(r.keys)]]
			if cand != addr && r.h.availableLocked(cand) {
				addr = cand
				break
			}
		}
	}
	r.h.acquireLocked(addr)
	for _, u := range r.origin {
		if u.Addr == addr {
			uu := u
			return &uu, nil
		}
	}
	return &config.Upstream{Addr: addr}, nil
}

func (r *hashRing) Done(addr string) { r.h.done(addr) }

func (r *hashRing) Report(addr string, success bool) { r.h.report(addr, success) }

func (r *hashRing) Stats() []NodeStat { return r.h.stats() }

// SetUpstreams 在线更新节点：环重建后只有约 1/N 的 key 会换节点。
//
// 与 Pick 共用 h.mu：环数据（keys/owner）在 Pick 里被直接读，重建必须与选路互斥。
// 注意 origin 与 health 的节点表仍是构造时的快照（保持改造前的行为不变）。
func (r *hashRing) SetUpstreams(ups []config.Upstream) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.rebuild(ups)
}

// hashOf 把 key 映射到环上的位置。
//
// ⚠️ 这里**不能图省事用 hash/fnv**：FNV-1a 的雪崩效应很差，
// "A#0"…"A#9" 这种只差末尾一个字符的字符串，哈希值几乎连成一片 ——
// 于是同一节点的 150 个虚拟节点全挤在环的同一小段上，虚拟节点等于白加。
//
// 本机实测（3 节点 / 每节点 150 个虚拟节点 / 10 万个 key）：
//
//	FNV-1a        A=17310  B=39560  C=43130   最大偏差 48.1%   ❌
//	MD5(前 8 字节) A=33344  B=34625  C=32031   最大偏差  3.9%   ✅
//
// 而且 FNV 下「每节点 1 个」与「每节点 10 个」的分布**完全一致**，正是虚拟节点失效的直接证据。
// 生产上更常用 xxhash / murmur3（更快且雪崩好）；这里用标准库 MD5 截断，避免引依赖。
func hashOf(s string) uint64 {
	sum := md5.Sum([]byte(s))
	return binary.BigEndian.Uint64(sum[:8])
}

// weightOf 权重兜底：未配置或非正数一律按 1 处理。
func weightOf(u config.Upstream) int {
	if u.Weight <= 0 {
		return 1
	}
	return u.Weight
}
