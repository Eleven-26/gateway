package balancer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gwlab/internal/config"
)

func leastConnService(addrs ...string) *config.Service {
	ups := make([]config.Upstream, 0, len(addrs))
	for _, a := range addrs {
		ups = append(ups, config.Upstream{Addr: a})
	}
	return &config.Service{Name: "t", Balance: "least_conn", Upstreams: ups}
}

// ejectService 造一个可配摘除参数的测试服务（不填 Ejection 就是不启用摘除）。
func ejectService(balance string, ej config.EjectionConfig, addrs ...string) *config.Service {
	ups := make([]config.Upstream, 0, len(addrs))
	for _, a := range addrs {
		ups = append(ups, config.Upstream{Addr: a})
	}
	return &config.Service{Name: "t", Balance: balance, Upstreams: ups, Ejection: ej}
}

// totalInflight 读全部节点的在途计数之和。
// 计数已并入四种算法共享的 health，测试改为走公开的 Stats()（意图不变：借了必须还）。
func totalInflight(b Balancer) int64 {
	var n int64
	for _, s := range b.Stats() {
		n += s.InFlight
	}
	return n
}

func assertAllZero(t *testing.T, b Balancer) {
	t.Helper()
	for _, s := range b.Stats() {
		if s.InFlight != 0 {
			t.Errorf("节点 %s 的在途计数应为 0，实际 %d", s.Addr, s.InFlight)
		}
	}
}

// statOf 取单个节点的快照，节点不存在则直接失败（Stats 漏节点本身就是 bug）。
func statOf(t *testing.T, b Balancer, addr string) NodeStat {
	t.Helper()
	for _, s := range b.Stats() {
		if s.Addr == addr {
			return s
		}
	}
	t.Fatalf("Stats() 里没有节点 %s", addr)
	return NodeStat{}
}

// pickAddrs 连续 Pick n 次（每次成对 Done，保持计数干净），返回选中的地址序列。
func pickAddrs(t *testing.T, b Balancer, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		up, err := b.Pick("k")
		if err != nil {
			t.Fatalf("第 %d 次 Pick 失败: %v", i, err)
		}
		out = append(out, up.Addr)
		b.Done(up.Addr)
	}
	return out
}

// countAddr 数一个地址在序列里出现了几次。
func countAddr(addrs []string, addr string) int {
	n := 0
	for _, a := range addrs {
		if a == addr {
			n++
		}
	}
	return n
}

// TestLeastConnPickDonePaired 校验 ④ 的核心不变式：Pick 与 Done 成对时在途计数必须回到 0。
// 主流水线用 `defer lb.Done(up.Addr)` 保证这件事（AGENTS.md §5 曾记录「只 Pick 不 Done」的缺口）。
func TestLeastConnPickDonePaired(t *testing.T) {
	b := New(leastConnService("a:1", "b:1", "c:1")).(*leastConn)

	for i := 0; i < 100; i++ {
		up, err := b.Pick("")
		if err != nil {
			t.Fatalf("第 %d 次 Pick 失败: %v", i, err)
		}
		b.Done(up.Addr)
	}
	if got := totalInflight(b); got != 0 {
		t.Fatalf("Pick/Done 成对后总在途应为 0，实际 %d", got)
	}
	assertAllZero(t, b)
}

// TestLeastConnConcurrentPairing 并发下同样要回到 0 —— 节点计数是 atomic，Pick 持锁自增、
// Done 取指针后自减，因此并发交错也不会漏还。
func TestLeastConnConcurrentPairing(t *testing.T) {
	b := New(leastConnService("a:1", "b:1", "c:1")).(*leastConn)

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up, err := b.Pick("")
			if err != nil {
				t.Errorf("Pick 失败: %v", err)
				return
			}
			time.Sleep(time.Millisecond) // 模拟在途
			b.Done(up.Addr)
		}()
	}
	wg.Wait()
	assertAllZero(t, b)
}

// TestLeastConnWithoutDoneLeaks 把「只 Pick 不 Done」的旧行为固定成可复现的事实：
// 计数只增不减，此时 least_conn 退化成「按历史次数轮询」（与 round_robin 几乎无差别）。
// 这是修复前的缺口，保留此测试是为了说明修复到底修掉了什么。
func TestLeastConnWithoutDoneLeaks(t *testing.T) {
	b := New(leastConnService("a:1", "b:1", "c:1")).(*leastConn)

	for i := 0; i < 30; i++ {
		if _, err := b.Pick(""); err != nil {
			t.Fatalf("第 %d 次 Pick 失败: %v", i, err)
		}
	}
	if got := totalInflight(b); got != 30 {
		t.Fatalf("只 Pick 不 Done 时应累积到 30，实际 %d", got)
	}
}

// TestLeastConnTieBreakIsFair 回归「平局偏置」：在途数全相等时（串行流量的常态，
// 每次请求都从 0,0,0 开始），选择必须在节点间轮转，而不是集中在列表第一个节点。
//
// 修复前的实测（原实现按 map 迭代顺序取第一个最小值）：
//
//	全 0 平局下 1000 次 Pick 的分布: a:1=777  b:1=116  c:1=107   ← 78% 砸在首位
func TestLeastConnTieBreakIsFair(t *testing.T) {
	b := New(leastConnService("a:1", "b:1", "c:1")).(*leastConn)

	count := map[string]int{}
	const rounds = 999 // 3 的倍数，纯轮转时每个节点恰好 333 次
	for i := 0; i < rounds; i++ {
		up, err := b.Pick("")
		if err != nil {
			t.Fatalf("第 %d 次 Pick 失败: %v", i, err)
		}
		count[up.Addr]++
		b.Done(up.Addr)
	}
	t.Logf("全 0 平局下 %d 次 Pick 的分布: a:1=%d b:1=%d c:1=%d",
		rounds, count["a:1"], count["b:1"], count["c:1"])

	if len(count) != 3 {
		t.Fatalf("三个节点都该被用到，实际 %v", count)
	}
	for addr, got := range count {
		if want := rounds / 3; got != want {
			t.Errorf("节点 %s 拿到 %d 次，期望 %d 次（平局必须轮转）", addr, got, want)
		}
	}
}

// TestNewDispatchesByBalance 顺带确认工厂分发：least_conn 拿到的实现必须已初始化节点计数
// （漏在外部预置的话 Pick 会直接返回 ErrNoUpstream）。
func TestNewDispatchesByBalance(t *testing.T) {
	for _, tc := range []struct {
		balance string
		want    string
	}{
		{"round_robin", "round_robin"},
		{"weighted", "weighted"},
		{"least_conn", "least_conn"},
		{"consistent_hash", "consistent_hash"},
		{"", "round_robin"}, // 未知/空值兜底为轮询
	} {
		b := New(&config.Service{Name: "t", Balance: tc.balance,
			Upstreams: []config.Upstream{{Addr: "a:1"}, {Addr: "b:1"}}})
		if got := b.Name(); got != tc.want {
			t.Errorf("Balance=%q 时得到 %q，期望 %q", tc.balance, got, tc.want)
		}
		if _, err := b.Pick("k"); err != nil {
			t.Errorf("Balance=%q 时 Pick 失败: %v", tc.balance, err)
		}
	}
}

// ---- 以下为节点级被动摘除 + 节点级指标的用例 ----

// TestStatsFollowUpstreamOrder Stats 必须按 Service.Upstreams 的顺序返回（/metrics 的行序
// 因此稳定），且初始状态下没人被摘、计数为 0。
func TestStatsFollowUpstreamOrder(t *testing.T) {
	b := New(ejectService("least_conn",
		config.EjectionConfig{FailThreshold: 2, Cooldown: time.Minute}, "b:1", "a:1", "c:1"))

	want := []string{"b:1", "a:1", "c:1"}
	got := b.Stats()
	if len(got) != len(want) {
		t.Fatalf("Stats() 应有 %d 个节点，实际 %d", len(want), len(got))
	}
	for i, s := range got {
		if s.Addr != want[i] {
			t.Errorf("第 %d 个节点应为 %s，实际 %s（顺序必须跟 Upstreams 一致）", i, want[i], s.Addr)
		}
		if s.Ejected || s.InFlight != 0 || s.Failures != 0 {
			t.Errorf("初始快照应全为 0 且未摘除，实际 %+v", s)
		}
	}
}

// TestEjectionAfterConsecutiveFailures 连续失败到阈值 → 摘除，且 Pick 不再选它。
// 四种算法都要跳过被摘节点（least_conn / round_robin 是需求点名的，另两种一并测）。
func TestEjectionAfterConsecutiveFailures(t *testing.T) {
	for _, balance := range []string{"round_robin", "weighted", "least_conn", "consistent_hash"} {
		t.Run(balance, func(t *testing.T) {
			// 冷却给足 1 分钟：本用例只关心「摘没摘」，不关心冷却（冷却到期见下一个用例）。
			b := New(ejectService(balance,
				config.EjectionConfig{FailThreshold: 3, Cooldown: time.Minute},
				"a:1", "b:1", "c:1"))

			b.Report("a:1", false)
			b.Report("a:1", false) // 只到 2 次：未达阈值
			if s := statOf(t, b, "a:1"); s.Ejected {
				t.Fatalf("连续失败 2 次（阈值 3）就摘除了：%+v", s)
			}
			if s := statOf(t, b, "a:1"); s.Failures != 2 {
				t.Fatalf("累计失败应为 2（指标用），实际 %d", s.Failures)
			}

			b.Report("a:1", false) // 第 3 次连续失败 → 摘除
			if s := statOf(t, b, "a:1"); !s.Ejected {
				t.Fatalf("连续失败达到阈值 3 后应被摘除：%+v", s)
			}
			if s := statOf(t, b, "a:1"); s.Failures != 3 {
				t.Errorf("摘除后累计失败数仍应保留（只给指标看），实际 %d", s.Failures)
			}
			for _, addr := range []string{"b:1", "c:1"} {
				if s := statOf(t, b, addr); s.Ejected {
					t.Errorf("摘除只针对连续失败的那个地址，%s 不该被牵连：%+v", addr, s)
				}
			}

			for i, addr := range pickAddrs(t, b, 60) {
				if addr == "a:1" {
					t.Fatalf("第 %d 次 Pick 仍选中被摘除的 a:1（%s 算法没有跳过它）", i, balance)
				}
			}
		})
	}
}

// TestSuccessClearsEjectionAndStreak 成功一次立即回列；判定用的是「连续」失败（成功即清零），
// 累计失败数只进指标、永不清零 —— 否则长期运行的节点迟早被历史失败数摘掉。
func TestSuccessClearsEjectionAndStreak(t *testing.T) {
	b := New(ejectService("round_robin",
		config.EjectionConfig{FailThreshold: 3, Cooldown: time.Minute}, "a:1", "b:1"))

	for i := 0; i < 3; i++ {
		b.Report("a:1", false)
	}
	if !statOf(t, b, "a:1").Ejected {
		t.Fatalf("连续失败 3 次后应被摘除")
	}

	b.Report("a:1", true) // 成功一次 → 立即回列
	s := statOf(t, b, "a:1")
	if s.Ejected {
		t.Fatalf("成功一次后应立即回列（不等冷却）：%+v", s)
	}
	if s.Failures != 3 {
		t.Errorf("累计失败次数不该被清零（只给指标看），实际 %d", s.Failures)
	}
	if countAddr(pickAddrs(t, b, 8), "a:1") == 0 {
		t.Errorf("回列后 a:1 应能重新被选中")
	}

	// 连续语义：2 次失败 → 成功（清零）→ 2 次失败 仍然不构成「连续 3 次」
	b.Report("a:1", false)
	b.Report("a:1", false)
	b.Report("a:1", true)
	b.Report("a:1", false)
	b.Report("a:1", false)
	if s := statOf(t, b, "a:1"); s.Ejected {
		t.Errorf("成功已把连续失败清零，2+2 次失败不该摘除：%+v", s)
	}
	b.Report("a:1", false) // 真正的连续第 3 次
	if s := statOf(t, b, "a:1"); !s.Ejected {
		t.Errorf("连续 3 次失败应摘除：%+v", s)
	}
}

// TestCooldownAllowsSingleProbe 摘除不是永久的：冷却到期后放行**一个**探测，
// 探测成功才完全回列。用很小的 Cooldown + Sleep，保持测试快。
func TestCooldownAllowsSingleProbe(t *testing.T) {
	const cooldown = 50 * time.Millisecond
	b := New(ejectService("round_robin",
		config.EjectionConfig{FailThreshold: 2, Cooldown: cooldown}, "a:1", "b:1"))

	b.Report("a:1", false)
	b.Report("a:1", false)
	if !statOf(t, b, "a:1").Ejected {
		t.Fatalf("连续失败 2 次后应被摘除")
	}

	// 冷却期内：一次都不选它
	for _, addr := range pickAddrs(t, b, 10) {
		if addr == "a:1" {
			t.Fatalf("冷却期内不该选中 a:1")
		}
	}

	time.Sleep(3 * cooldown) // 冷却到期

	// 两个连续 Pick 的轮转起点不同 → a:1 恰好被放行一次（那一个探测）
	if got := countAddr(pickAddrs(t, b, 2), "a:1"); got != 1 {
		t.Fatalf("冷却到期后应恰好放行 1 个探测，实际 %d", got)
	}
	// 探测还在飞（没有 Report 结算）：租约有效期内不再放第二个
	for _, addr := range pickAddrs(t, b, 6) {
		if addr == "a:1" {
			t.Fatalf("探测租约仍在有效期内，不该再放行第二个探测")
		}
	}
	if s := statOf(t, b, "a:1"); !s.Ejected {
		t.Errorf("探测在飞时 Ejected 应为 true（Pick 此刻仍会跳过它）：%+v", s)
	}

	// 探测成功 → 完全回列（不必等下一个冷却）
	b.Report("a:1", true)
	if s := statOf(t, b, "a:1"); s.Ejected {
		t.Fatalf("探测成功后应完全回列：%+v", s)
	}
	if countAddr(pickAddrs(t, b, 4), "a:1") == 0 {
		t.Errorf("回列后 a:1 应能重新被选中")
	}
}

// TestProbeLeaseExpiresWithoutReport 防「探测请求 panic 后永不结算」：
// 探测租约超过一个 Cooldown 未结算就自动作废，允许下一个请求去探 ——
// 否则一次 panic 就让这个节点永远回不了列。
func TestProbeLeaseExpiresWithoutReport(t *testing.T) {
	const cooldown = 50 * time.Millisecond
	b := New(ejectService("round_robin",
		config.EjectionConfig{FailThreshold: 1, Cooldown: cooldown}, "a:1", "b:1"))

	b.Report("a:1", false) // 阈值 1：一次失败即摘除
	time.Sleep(3 * cooldown)

	if got := countAddr(pickAddrs(t, b, 2), "a:1"); got != 1 {
		t.Fatalf("冷却到期后应恰好放行 1 个探测，实际 %d", got)
	}
	// 模拟这个探测请求 panic/卡死：既不 Report 也不 Done 之外的事情，租约到期后应自动失效
	time.Sleep(3 * cooldown)
	if got := countAddr(pickAddrs(t, b, 2), "a:1"); got != 1 {
		t.Fatalf("探测租约超时后应允许下一个探测，实际放行 %d 次", got)
	}
}

// TestProbeLeaseIsSingleUnderConcurrency 「冷却到期只放行一个探测」必须在并发下也成立：
// 判定健康与认领租约在同一个临界区里，否则 50 个 goroutine 会一起涌向刚恢复的节点。
func TestProbeLeaseIsSingleUnderConcurrency(t *testing.T) {
	const cooldown = 100 * time.Millisecond // 给并发留足余量，避免租约中途过期
	b := New(ejectService("round_robin",
		config.EjectionConfig{FailThreshold: 1, Cooldown: cooldown}, "a:1", "b:1"))

	b.Report("a:1", false)
	time.Sleep(3 * cooldown) // 冷却到期

	var probes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up, err := b.Pick("k")
			if err != nil {
				t.Errorf("Pick 失败: %v", err)
				return
			}
			if up.Addr == "a:1" {
				probes.Add(1)
			}
			b.Done(up.Addr)
		}()
	}
	wg.Wait()

	if got := probes.Load(); got != 1 {
		t.Errorf("冷却到期后只应放行 1 个探测，实际 %d（探测租约不是原子的）", got)
	}
}

// TestAllEjectedFailsOpen 全摘必须 fail-open：所有节点都在冷却时 Pick 仍要成功，
// 否则摘除机制会把整个服务打死（上游全挂时，「发出去赌一把」好过「就地 503」）。
func TestAllEjectedFailsOpen(t *testing.T) {
	for _, balance := range []string{"round_robin", "weighted", "least_conn", "consistent_hash"} {
		t.Run(balance, func(t *testing.T) {
			b := New(ejectService(balance,
				config.EjectionConfig{FailThreshold: 1, Cooldown: time.Minute},
				"a:1", "b:1", "c:1"))
			valid := map[string]bool{"a:1": true, "b:1": true, "c:1": true}

			for _, addr := range []string{"a:1", "b:1", "c:1"} {
				b.Report(addr, false)
			}
			for _, s := range b.Stats() {
				if !s.Ejected {
					t.Fatalf("节点 %s 应处于摘除中：%+v", s.Addr, s)
				}
			}

			addrs := pickAddrs(t, b, 30) // 全摘状态下也要能 Pick 成功
			seen := map[string]int{}
			for _, a := range addrs {
				if !valid[a] {
					t.Fatalf("Pick 返回了配置里没有的地址 %s", a)
				}
				seen[a]++
			}
			// 回退口径是「全量」：轮转类算法此时三个节点都会重新被用到。
			// consistent_hash 同一个 key 恒定打同一节点，不做这条断言。
			if balance != "consistent_hash" && len(seen) != 3 {
				t.Errorf("fail-open 应回退到全量节点，实际分布 %v", seen)
			}
		})
	}
}

// TestFailThresholdZeroNeverEjects FailThreshold=0 表示不启用：可以无限失败，永不摘除，
// 但累计失败数仍要记账（/metrics 的 gw_upstream_failures_total 靠它）。
func TestFailThresholdZeroNeverEjects(t *testing.T) {
	for _, balance := range []string{"round_robin", "least_conn"} {
		t.Run(balance, func(t *testing.T) {
			b := New(ejectService(balance,
				config.EjectionConfig{FailThreshold: 0, Cooldown: time.Minute}, "a:1", "b:1"))

			for i := 0; i < 100; i++ {
				b.Report("a:1", false)
			}
			for _, s := range b.Stats() {
				if s.Ejected {
					t.Fatalf("FailThreshold=0 表示不启用摘除，%s 却被摘了：%+v", s.Addr, s)
				}
			}
			if s := statOf(t, b, "a:1"); s.Failures != 100 {
				t.Errorf("累计失败仍要记账（指标用），实际 %d", s.Failures)
			}
			if countAddr(pickAddrs(t, b, 6), "a:1") == 0 {
				t.Errorf("未启用摘除时不该跳过 a:1")
			}
		})
	}
}

// TestInflightStatsCounting：四种算法都要有节点级在途计数（含 round_robin），
// Pick 与 Done 成对后回到 0。单节点服务让断言与算法无关。
func TestInflightStatsCounting(t *testing.T) {
	for _, balance := range []string{"round_robin", "weighted", "least_conn", "consistent_hash"} {
		t.Run(balance, func(t *testing.T) {
			b := New(ejectService(balance, config.EjectionConfig{}, "a:1"))

			for i := 0; i < 2; i++ {
				up, err := b.Pick("k")
				if err != nil {
					t.Fatalf("第 %d 次 Pick 失败: %v", i, err)
				}
				if up.Addr != "a:1" {
					t.Fatalf("单节点服务只该返回 a:1，实际 %s", up.Addr)
				}
			}
			if got := statOf(t, b, "a:1").InFlight; got != 2 {
				t.Fatalf("Pick 两次应在途 2，实际 %d", got)
			}
			b.Done("a:1")
			if got := statOf(t, b, "a:1").InFlight; got != 1 {
				t.Fatalf("Done 一次应在途 1，实际 %d", got)
			}
			b.Done("a:1")
			if got := statOf(t, b, "a:1").InFlight; got != 0 {
				t.Fatalf("Done 两次应在途 0，实际 %d", got)
			}
			if got := totalInflight(b); got != 0 {
				t.Fatalf("总在途应为 0，实际 %d", got)
			}
		})
	}
}
