package transcode

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"gwlab/internal/config"
)

// connBudget 是单次 Conn 的耗时上限。
//
// 为什么是 1s：审计 P1-4 里旧实现的建连超时是 3s（WithBlock + context.WithTimeout），
// 所以「1s 内返回」足以区分新旧行为 —— 旧实现撞上黑洞上游必然要等满 3s 才返回。
const connBudget = time.Second

// blackhole 是一个「只 accept、不说话」的 TCP 上游：TCP 三次握手能完成，
// 但服务端永远不回 HTTP/2 SETTINGS 帧，客户端也就永远等不到握手完成。
//
// 这正是 P1-4 要防的场景：旧实现 grpc.DialContext(..., WithBlock) 会在这里死等 3s，
// 而且因为当时持着 Pool.mu，所有地址的转码请求一起被卡住（头阻塞）。
type blackhole struct {
	ln net.Listener

	mu    sync.Mutex
	conns []net.Conn
}

func newBlackhole(t *testing.T) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听黑洞上游失败: %v", err)
	}
	b := &blackhole{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // 监听器已关闭，退出
			}
			// 收下连接后什么都不做：不读、不写、不回 SETTINGS。
			b.mu.Lock()
			b.conns = append(b.conns, c)
			b.mu.Unlock()
		}
	}()
	t.Cleanup(b.Close)
	return b
}

func (b *blackhole) Addr() string { return b.ln.Addr().String() }

func (b *blackhole) Close() {
	_ = b.ln.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.conns = nil
}

// TestConnNonBlocking 校验审计 P1-4 的核心修复：建连不再占用请求路径。
//
//	① Conn(黑洞地址) 必须立刻返回且不报错 —— 连接是后台异步建的；
//	② 紧接着 Conn(正常地址) 也必须立刻返回，不能被 ① 拖住（per-addr 锁，没有头阻塞）。
//
// 正常地址直接用 net.Listen("tcp", "127.0.0.1:0") 的地址即可：改成 grpc.NewClient 之后
// 不再需要真的跑一个 gRPC 服务，因为没有 WithBlock，没人会去等握手。
func TestConnNonBlocking(t *testing.T) {
	hole := newBlackhole(t)

	goodLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听正常上游失败: %v", err)
	}
	t.Cleanup(func() { _ = goodLn.Close() })

	p := NewPool()
	defer p.Close()

	start := time.Now()
	c1, err := p.Conn(&config.Upstream{Addr: hole.Addr()})
	if err != nil {
		t.Fatalf("对黑洞上游 Conn 不应报错（建连已改为异步），实际: %v", err)
	}
	if c1 == nil {
		t.Fatal("Conn(黑洞) 返回了 nil 连接")
	}
	holeCost := time.Since(start)
	if holeCost > connBudget {
		t.Fatalf("Conn(黑洞) 耗时 %v，超过 %v —— 建连又跑到请求路径上了（P1-4 回归）", holeCost, connBudget)
	}

	start = time.Now()
	c2, err := p.Conn(&config.Upstream{Addr: goodLn.Addr().String()})
	if err != nil {
		t.Fatalf("Conn(正常地址) 报错: %v", err)
	}
	if c2 == nil {
		t.Fatal("Conn(正常地址) 返回了 nil 连接")
	}
	goodCost := time.Since(start)
	if goodCost > connBudget {
		t.Fatalf("Conn(正常地址) 耗时 %v，超过 %v —— 被黑洞地址的建连阻塞了（头阻塞，P1-4 回归）",
			goodCost, connBudget)
	}

	// 别让上面的计时断言变成空断言：两次 Conn 必须真的各建了一条连接。
	// 两个地址不同 → 指针必须不同；池里必须恰好两条。
	if c1 == c2 {
		t.Fatalf("黑洞 %s 与正常 %s 不应返回同一条连接", hole.Addr(), goodLn.Addr())
	}
	p.mu.Lock()
	pooled := len(p.conns)
	p.mu.Unlock()
	if pooled != 2 {
		t.Fatalf("两个不同地址应各缓存一条连接，实际池中 %d 条", pooled)
	}

	t.Logf("黑洞=%s 正常=%s", hole.Addr(), goodLn.Addr())
	t.Logf("Conn(黑洞)=%v (%dns)，Conn(正常)=%v (%dns)，均 < %v（旧实现黑洞地址要等满 3s）",
		holeCost, holeCost.Nanoseconds(), goodCost, goodCost.Nanoseconds(), connBudget)
}

// TestBlackholeFixtureActuallyStalls 是「测测试本身」：先证明 fixtures 里的黑洞上游真的会卡住握手，
// 否则上面 TestConnNonBlocking 的「1s 内返回」可能只是空断言（比如监听器被立刻拒连）。
//
// 这里故意用已经废弃的阻塞式 grpc.DialContext + WithBlock 复现**旧行为**：
// 给 300ms 预算，它必须超时失败。如果这个测试反而成功了，说明黑洞不黑，
// TestConnNonBlocking 就失去了回归价值，要回来改 fixtures。
func TestBlackholeFixtureActuallyStalls(t *testing.T) {
	hole := newBlackhole(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	conn, err := grpc.DialContext(ctx, hole.Addr(), //nolint:staticcheck // 故意用旧 API 复现 P1-4 的旧行为
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	cost := time.Since(start)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("黑洞上游竟然在 %v 内握手成功，fixtures 无效，TestConnNonBlocking 失去回归价值", cost)
	}
	t.Logf("旧行为复现成功：WithBlock 撞上黑洞耗时 %v 后失败（%v）", cost, err)
}

// TestConnReuse 同一地址必须复用同一个 *grpc.ClientConn：
// 连接复用是 gRPC 转码的全部收益所在，per-addr 锁 + 双重检查改错就会退化成每次新建连接。
func TestConnReuse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()

	p := NewPool()
	defer p.Close()

	first, err := p.Conn(&config.Upstream{Addr: addr})
	if err != nil {
		t.Fatalf("第一次 Conn 失败: %v", err)
	}
	second, err := p.Conn(&config.Upstream{Addr: addr})
	if err != nil {
		t.Fatalf("第二次 Conn 失败: %v", err)
	}
	if first != second {
		t.Fatalf("同一地址两次 Conn 应返回同一指针：first=%p second=%p", first, second)
	}
}

// TestConnConcurrentSameAddr 并发首访同一地址时，双重检查必须保证只建一条连接。
//
// 本机 go test -race 不可用（CGO_ENABLED=0 无 gcc，见 AGENTS.md §3），
// 所以这里用「所有 goroutine 拿到的指针必须完全相同」来间接守住不变式：
// 少了一次锁内复查，就会出现多个不同的 *ClientConn，多出来的那些永远不会被 Close。
func TestConnConcurrentSameAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()

	p := NewPool()
	defer p.Close()

	const n = 16
	got := make([]*grpc.ClientConn, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让 n 个 goroutine 同时涌进 Conn
			c, err := p.Conn(&config.Upstream{Addr: addr})
			if err != nil {
				t.Errorf("并发 Conn 失败: %v", err)
				return
			}
			got[i] = c
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range got {
		if c == nil || c != got[0] {
			t.Fatalf("并发首访 %s 应只建一条连接：got[0]=%p got[%d]=%p", addr, got[0], i, c)
		}
	}

	// per-addr 锁表有界：地址集合来自编译期配置，不应该随请求增长。
	p.mu.Lock()
	locked := len(p.dialing)
	p.mu.Unlock()
	if locked != 1 {
		t.Fatalf("同一地址只应留下 1 把 dialing 锁，实际 %d", locked)
	}
}

// testEcho 只用来满足 grpc.ServiceDesc.HandlerType 必须是接口类型的要求；
// 处理器本身不做类型断言，所以空接口就够。
type testEcho interface{}

// echoHandler 用注册好的 json codec 收/发 map，把收到的请求与 metadata 原样回显，
// 让测试能断言「body / paramN / metadata 白名单」整条链路都通了。
func echoHandler(_ any, ctx context.Context, dec func(any) error,
	_ grpc.UnaryServerInterceptor) (any, error) {

	in := map[string]any{}
	if err := dec(&in); err != nil {
		return nil, err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	return map[string]any{"echo": in, "x_trace_id": md.Get("x-trace-id")}, nil
}

// TestTranscodeEndToEnd 是一条真正的端到端用例：起一个最小 gRPC 服务（注册 json codec 的
// ServiceDesc，无需 protoc），走完整 Transcode。
//
// 为什么必须有它：P1-4 把 DialContext 换成 NewClient，改的不只是「阻不阻塞」——
// 两者的默认解析器/scheme 与连接策略不同。只测「Conn 秒回」不足以证明转码还能成功，
// 这里用真实 RPC 把 json codec、metadata 透传、paramN 补参、Invoke 全部串起来验一遍。
func TestTranscodeEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "order.OrderService",
		HandlerType: (*testEcho)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "CreateOrder",
			Handler:    echoHandler,
		}},
	}, struct{}{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	p := NewPool()
	defer p.Close()

	out, err := Transcode(context.Background(), p, &config.Upstream{Addr: ln.Addr().String()},
		&config.TranscodePolicy{Service: "order.OrderService", Method: "CreateOrder"},
		[]byte(`{"sku":"A-1","qty":2}`),
		[]string{"42"},
		map[string]string{"X-Trace-Id": "trace-abc"},
	)
	if err != nil {
		t.Fatalf("Transcode 失败: %v", err)
	}

	echo, ok := out["echo"].(map[string]any)
	if !ok {
		t.Fatalf("回包缺少 echo 对象: %#v", out)
	}
	if echo["sku"] != "A-1" {
		t.Errorf("请求体字段应原样送达，sku=%v", echo["sku"])
	}
	if echo["param1"] != "42" {
		t.Errorf("路径捕获组应补成 param1=42，实际 %v", echo["param1"])
	}
	ids, ok := out["x_trace_id"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "trace-abc" {
		t.Errorf("metadata 白名单应透传 x-trace-id，实际 %#v", out["x_trace_id"])
	}
}
