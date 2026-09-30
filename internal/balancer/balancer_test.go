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

// totalInflight 读全部节点的在途计数之和（测试内直接读 conns，生产代码不需要这个口子）。
func totalInflight(b *leastConn) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for _, c := range b.conns {
		n += atomic.LoadInt64(c)
	}
	return n
}

func assertAllZero(t *testing.T, b *leastConn) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for addr, c := range b.conns {
		if got := atomic.LoadInt64(c); got != 0 {
			t.Errorf("节点 %s 的在途计数应为 0，实际 %d", addr, got)
		}
	}
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
