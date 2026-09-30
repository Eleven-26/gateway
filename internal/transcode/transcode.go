// Package transcode 实现 ⑤ 协议转换：外部 REST/JSON → 内部 gRPC。
//
// 为什么值得在网关上做：客户端只会说 JSON，服务端只会说 protobuf，
// 若不在这里转换，这份「翻译」逻辑会散落到每个服务里（每个服务都要同时支持两种协议）。
//
// 两个工程要点：
//
//	① **连接必须复用** —— gRPC 连接建立要握手 + HTTP/2 SETTINGS，每请求新建会吃掉全部收益；
//	② **方法名是 /包.服务/方法** 的字符串（"/order.OrderService/CreateOrder"），
//	  不需要生成的 stub 也能调用（grpc.ClientConn.Invoke 接任意消息类型）。
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
	"time"

	"google.golang.org/grpc"
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
}

// NewPool 创建连接池。
func NewPool() *Pool { return &Pool{conns: map[string]*grpc.ClientConn{}} }

// Conn 取（或建）到 addr 的连接。
func (p *Pool) Conn(addr string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[addr]; ok {
		return c, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// 告诉 grpc-go：调用时用我们注册的 json codec
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype("json")),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, err
	}
	p.conns[addr] = c
	return c, nil
}

// Close 关闭全部连接。
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

// Transcode 把一次 HTTP 请求翻译成 gRPC 调用，并把结果转回 JSON。
//
// 映射规则（REST → gRPC）：
//
//	请求体 JSON      → gRPC 请求消息（字段名一一对应）
//	路径参数          → 由路由的正则捕获组补齐（如 /api/users/42 → param1=42）
//	部分请求头        → gRPC metadata（只透传白名单，防止把无意义的头塞进 metadata）
func Transcode(ctx context.Context, pool *Pool, addr string, t *config.TranscodePolicy,
	body []byte, groups []string, hdr map[string]string) (map[string]any, error) {

	conn, err := pool.Conn(addr)
	if err != nil {
		return nil, fmt.Errorf("连接上游 %s 失败: %w", addr, err)
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
