// Package transcode 实现 ⑤ 协议转换：外部 REST/JSON → 内部 gRPC。
//
// 为什么值得在网关上做：客户端只会说 JSON，服务端只会说 protobuf，
// 若不在这里转换，这份「翻译」逻辑会散落到每个服务里（每个服务都要同时支持两种协议）。
//
// 三个工程要点：
//
//	① **连接必须复用** —— gRPC 连接建立要握手 + HTTP/2 SETTINGS，每请求新建会吃掉全部收益；
//	② **方法名是 /包.服务/方法** 的字符串（"/order.OrderService/CreateOrder"），
//	  不需要生成的 stub 也能调用（grpc.ClientConn.Invoke 接任意消息类型）；
//	③ **建连不能占请求路径，也不能锁全局**（审计 P1-4）—— 见 Pool.Conn：非阻塞建连 + 按地址分锁。
//
// ⚠️ 本机没有 protoc，所以这里注册了一个 JSON codec 代替 protobuf——
// 传输层仍是真正的 gRPC（HTTP/2 + 帧 + 多路复用 + 超时传播），只是编解码换成 JSON。
// 生产用 protobuf 时，只需去掉 codec 注册、换成生成的消息结构体，其余不动。
package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"

	"gwlab/internal/config"
)

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return "json" }

func init() { encoding.RegisterCodec(jsonCodec{}) }

// Pool 按地址复用 gRPC 连接。
type Pool struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn

	// dialing 为每个地址保存一把「拨号锁」（审计 P1-4）。
	//
	// 为什么值得做：Conn 原来在持有 p.mu 的情况下 DialContext(WithBlock, 3s)。
	// 后果有两个：①一个慢/黑洞上游会把**所有**地址的转码请求一起卡住（头阻塞），
	// 因为全局互斥锁被一次建连占满 3s；②首次请求要把最长 3s 的建连放在请求路径上。
	// 把串行化粒度收敛到「同一地址」之后，Conn(addrA) 的拨号不再影响 Conn(addrB)。
	//
	// 容易做错的地方：
	//   - per-addr 锁必须在 p.mu **之外**获取/释放。如果在持 p.mu 时去抢 per-addr 锁，
	//     全局串行又回来了，P1-4 等于没修。
	//   - 拿到 per-addr 锁后必须**再查一次** p.conns（双重检查），否则同一地址在并发首访时
	//     会建出多条连接，多余的那些既无人使用也无人关闭（连接泄漏）。
	//   - dialing 里的锁只增不删：地址集合来自编译期配置，数量有界，留着比为每个地址
	//     做引用计数/回收更简单，也不会随请求增长。
	dialing map[string]*sync.Mutex
}

// NewPool 创建连接池。
func NewPool() *Pool {
	return &Pool{
		conns:   map[string]*grpc.ClientConn{},
		dialing: map[string]*sync.Mutex{},
	}
}

// lookup 只回答「这个地址已经建好连接了吗」，命中时完全不碰 dialing。
func (p *Pool) lookup(addr string) *grpc.ClientConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[addr]
}

// dialLock 取出（必要时创建）addr 专属的拨号锁。只做一次 map 访问，持 p.mu 的时间可以忽略。
func (p *Pool) dialLock(addr string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.dialing[addr]
	if !ok {
		l = &sync.Mutex{}
		p.dialing[addr] = l
	}
	return l
}

// Conn 取（或建）到该节点的连接。
//
// 本函数**立即返回**（审计 P1-4）：grpc.NewClient 只创建 ClientConn（解析 target、
// 拉起后台连接 goroutine），不等 TCP + HTTP/2 SETTINGS 握手完成；真正的建连由后续 RPC
// 放进该请求自己的 ctx 超时里完成，3s 建连不再占用请求路径，也不再有头阻塞。
//
// 行为上的取舍（调用方需知道）：上游不可达不再由 Conn 报错，而是由 Transcode 里
// conn.Invoke 在该请求的 ctx 上失败 —— 失败边界从「与请求无关的固定 3s」变成「本请求的超时」，
// 这对网关是对的方向（Transcode 的签名和错误包装都没有改）。
//
// scheme=https 的节点走 TLS（审计 C2），TLS 语义与 HTTP 上游**共用** config.Upstream.TLSClientConfig，
// 避免两条链路各写一份而漂移。
func (p *Pool) Conn(up *config.Upstream) (*grpc.ClientConn, error) {
	addr := up.Addr
	if c := p.lookup(addr); c != nil {
		return c, nil
	}
	dl := p.dialLock(addr)
	dl.Lock()
	defer dl.Unlock()
	// 双重检查：等锁期间别的 goroutine 可能已经把这条连接建好并放进 p.conns。
	if c := p.lookup(addr); c != nil {
		return c, nil
	}

	opts := []grpc.DialOption{
		// 告诉 grpc-go：调用时用我们注册的 json codec
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype("json")),
	}
	if up.SchemeOrDefault() == "https" {
		tlsCfg, err := up.TLSClientConfig()
		if err != nil {
			return nil, fmt.Errorf("上游 %s 的 TLS 配置加载失败: %w", addr, err)
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// 非阻塞建连。⚠️ 千万不要加 grpc.WithBlock()（NewClient 也不支持它）：
	// 一旦阻塞等待握手，本函数又变成长达数秒的调用，P1-4 直接回归。
	c, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.conns[addr] = c
	p.mu.Unlock()
	return c, nil
}

// Close 关闭全部连接。
//
// 顺手清空 conns：Close 之后再调 Conn 不会拿到已关闭的连接（会重建一条），
// 重复 Close 也不会对同一个 *ClientConn 二次 Close。
//
// dialing 有意保留不清空 —— 它只是一张锁表、不持有任何系统资源；清空它反而会在
// 「Close 与某次 Conn 并发」时制造出两把不同的 per-addr 锁，让同一地址被并发建连。
// 已知边界（本批次不改）：Close 与并发 Conn 之间没有额外的状态机，正在建连的那次
// 仍可能把新连接写进已清空的 map。Pool 由 Gateway 持有，Close 只在进程退出 /
// 热重载整体换实例时调用，生产路径上不存在这种并发；真要修需要引入 closed 标志与新错误值，
// 超出 P1-4 的范围。
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[string]*grpc.ClientConn{}
}

// Transcode 把一次 HTTP 请求翻译成 gRPC 调用，并把结果转回 JSON。
//
// 映射规则（REST → gRPC）：
//
//	请求体 JSON      → gRPC 请求消息（字段名一一对应）
//	路径参数          → 由路由的正则捕获组补齐（如 /api/users/42 → param1=42）
//	部分请求头        → gRPC metadata（只透传白名单，防止把无意义的头塞进 metadata）
func Transcode(ctx context.Context, pool *Pool, up *config.Upstream, t *config.TranscodePolicy,
	body []byte, groups []string, hdr map[string]string) (map[string]any, error) {

	conn, err := pool.Conn(up)
	if err != nil {
		return nil, fmt.Errorf("连接上游 %s 失败: %w", up.Addr, err)
	}

	// 请求体解析成 map；空体也允许（用 groups 补参数）
	req := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
		}
	}
	for i, g := range groups {
		req[fmt.Sprintf("param%d", i+1)] = g
	}

	// 只透传白名单 metadata，避免把 Cookie / User-Agent 之类塞进 gRPC 头
	if len(hdr) > 0 {
		kv := make([]string, 0, len(hdr)*2)
		for k, v := range hdr {
			kv = append(kv, strings.ToLower(k), v)
		}
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(kv...))
	}

	out := map[string]any{}
	method := "/" + t.Service + "/" + t.Method
	if err := conn.Invoke(ctx, method, &req, &out); err != nil {
		return nil, err
	}
	return out, nil
}
