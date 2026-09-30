// Command backend 是演示用后端：同时提供 HTTP 与 gRPC 两种接口。
//
//   - HTTP：回显收到的路径 / 关键请求头，用来验证网关有没有改对头；
//   - gRPC：order.OrderService/CreateOrder，用来验证 HTTP→gRPC 协议转换；
//   - /__control?fail=1|0：运行时切换「返回 500」，用来演示熔断器的三态迁移；
//   - 任意路径加 ?ms=800：故意慢响应，用来观察 `least_conn` 的在途计数（见 internal/balancer）。
//
// 用法：
//
//	go run ./cmd/backend -http 127.0.0.1:19001 -grpc 127.0.0.1:19100 -name A
//	go run ./cmd/backend -http 127.0.0.1:19002 -name B
//	go run ./cmd/backend -http 127.0.0.1:19003 -name C
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
)

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return "json" }

func init() { encoding.RegisterCodec(jsonCodec{}) }

var failing int32

// echoHeaders 关心的请求头 —— 正好是网关最容易改错的几个
var echoHeaders = []string{
	"X-Forwarded-For", "X-Real-IP", "X-Forwarded-Host", "X-Forwarded-Proto",
	"X-Forwarded-User", "X-Trace-Id", "X-Client",
	// W3C 链路头（审计 C5）：网关应当沿用上游 trace-id、为本跳换新 span-id，
	// 回显这两个头就能直接看出链路传播对不对（tracestate 是别人的私有数据，应原样透传）
	"traceparent", "tracestate",
	"X-Company-Id", "X-Tenant-Id", // ⚠️ 这两个应该被网关剥掉，回显里看不到才对
}

type backend struct {
	name     string
	httpAddr string
}

func (b *backend) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__control") {
		if v := r.URL.Query().Get("fail"); v != "" {
			n, _ := strconv.Atoi(v)
			atomic.StoreInt32(&failing, int32(n))
		}
		writeJSON(w, 200, map[string]any{"backend": b.name, "failing": atomic.LoadInt32(&failing) == 1})
		return
	}

	// ?ms=800：故意放慢响应。网关的 least_conn 只有在请求真的占住连接时才看得出来
	//（否则在途数永远是 0/1，效果和轮询一样）。上限 5s，仅演示用。
	if v := r.URL.Query().Get("ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 5000 {
				n = 5000
			}
			time.Sleep(time.Duration(n) * time.Millisecond)
		}
	}

	if atomic.LoadInt32(&failing) == 1 {
		writeJSON(w, 500, map[string]any{"backend": b.name, "error": "upstream injected failure"})
		return
	}

	seen := map[string]string{}
	for _, h := range echoHeaders {
		if v := r.Header.Get(h); v != "" {
			seen[h] = v
		}
	}
	writeJSON(w, 200, map[string]any{
		"backend": b.name,
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   r.URL.RawQuery,
		"headers": seen,
		"ts":      time.Now().Format("15:04:05.000"),
	})
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

// ---- gRPC 服务：order.OrderService/CreateOrder ----

type orderService interface {
	CreateOrder(context.Context, *map[string]any) (*map[string]any, error)
}

type orderImpl struct{ name string }

func (o orderImpl) CreateOrder(ctx context.Context, req *map[string]any) (*map[string]any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	trace := ""
	if v := md.Get("x-trace-id"); len(v) > 0 {
		trace = v[0]
	}
	return &map[string]any{
		"order_id":    fmt.Sprintf("SO-%s-%04d", time.Now().Format("20060102"), time.Now().UnixNano()%10000),
		"accepted_by": o.name,
		"received":    *req,
		"trace":       trace,
		"protocol":    "gRPC over HTTP/2 (json codec)",
	}, nil
}

func startGRPC(addr, name string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("grpc listen:", err)
		os.Exit(1)
	}
	desc := grpc.ServiceDesc{
		ServiceName: "order.OrderService",
		HandlerType: (*orderService)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "CreateOrder",
			Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := map[string]any{}
				if err := dec(&in); err != nil {
					return nil, err
				}
				return srv.(orderService).CreateOrder(ctx, &in)
			},
		}},
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(jsonCodec{}))
	srv.RegisterService(&desc, orderImpl{name: name})
	fmt.Printf("backend %s gRPC  listening on %s  (order.OrderService/CreateOrder)\n", name, addr)
	if err := srv.Serve(lis); err != nil {
		fmt.Println("grpc serve:", err)
	}
}

func main() {
	httpAddr := flag.String("http", "", "HTTP 监听地址，如 127.0.0.1:19001")
	grpcAddr := flag.String("grpc", "", "gRPC 监听地址（留空则不启）")
	name := flag.String("name", "A", "实例名，回显用")
	flag.Parse()

	b := &backend{name: *name, httpAddr: *httpAddr}
	if *grpcAddr != "" {
		go startGRPC(*grpcAddr, *name)
	}
	if *httpAddr != "" {
		fmt.Printf("backend %s HTTP  listening on %s\n", *name, *httpAddr)
		if err := http.ListenAndServe(*httpAddr, http.HandlerFunc(b.handler)); err != nil {
			fmt.Println("http serve:", err)
		}
	}
	select {}
}
