// Package gateway 把七个模块接成一条请求流水线。
//
//	TraceID → ① 路由 → ② 鉴权 → ③ 限流 → ③ 熔断 → ④ 选节点 → ⑤ 转换 / ⑥ 转发 → ⑦ 指标 + 日志
//
// 顺序不是随便定的，两条硬约束：
//
//	· **鉴权必须在限流之前** —— 否则限流只能按 IP 计数（NAT 后面一栋楼共用一个桶）；
//	· **熔断必须在选节点之前** —— 熔断打开时要直接快速失败，不能再走一遍选节点与转发。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"gwlab/internal/auth"
	"gwlab/internal/balancer"
	"gwlab/internal/breaker"
	"gwlab/internal/config"
	"gwlab/internal/observability"
	"gwlab/internal/proxy"
	"gwlab/internal/ratelimit"
	"gwlab/internal/router"
	"gwlab/internal/transcode"
)

const (
	maxTranscodeBody = 1 << 20 // 协议转换读取请求体的上限：1MB
	accessLogKeep    = 200     // 内存中保留的访问日志条数
	debugLogTail     = 50      // /debug/logs 返回的条数
)

// Gateway 是实现了 http.Handler 的网关本体。
type Gateway struct {
	cfg      *config.Config
	router   *router.Router
	authn    *auth.Authenticator
	limiter  *ratelimit.Limiter
	grpcPool *transcode.Pool
	metrics  *observability.Metrics
	alog     *observability.AccessLog

	mu        sync.Mutex
	breakers  map[string]*breaker.Breaker
	balancers map[string]balancer.Balancer
	proxies   map[string]*proxy.Proxy
}

// New 装配一个网关。熔断器 / 均衡器 / 代理都是**按需惰性创建**并缓存的。
func New(cfg *config.Config) (*Gateway, error) {
	r, err := router.New(cfg.Routes)
	if err != nil {
		return nil, err
	}
	return &Gateway{
		cfg:       cfg,
		router:    r,
		authn:     auth.New(cfg.JWTSecret, cfg.APIKey),
		limiter:   ratelimit.NewLimiter(),
		grpcPool:  transcode.NewPool(),
		metrics:   observability.NewMetrics(),
		alog:      observability.NewAccessLog(accessLogKeep),
		breakers:  map[string]*breaker.Breaker{},
		balancers: map[string]balancer.Balancer{},
		proxies:   map[string]*proxy.Proxy{},
	}, nil
}

// Close 释放网关持有的资源（gRPC 连接池）。
func (g *Gateway) Close() { g.grpcPool.Close() }

func (g *Gateway) breakerFor(svc *config.Service) *breaker.Breaker {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.breakers[svc.Name]; ok {
		return b
	}
	b := breaker.New(svc.Breaker)
	g.breakers[svc.Name] = b
	return b
}

func (g *Gateway) balancerFor(svc *config.Service) balancer.Balancer {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.balancers[svc.Name]; ok {
		return b
	}
	b := balancer.New(svc)
	g.balancers[svc.Name] = b
	return b
}

func (g *Gateway) proxyFor(svc *config.Service, up *config.Upstream, strip string) *proxy.Proxy {
	key := svc.Name + "|" + up.Addr + "|" + strip
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.proxies[key]; ok {
		return p
	}
	p := proxy.New(up, strip)
	g.proxies[key] = p
	return p
}

// ---- 捕获状态码与字节数的 ResponseWriter 包装 ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush 必须转发：ReverseProxy 的流式响应（SSE）依赖它，否则会攒在缓冲区里。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}

func fail(w http.ResponseWriter, status int, code, msg, trace string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg, "trace": trace})
}

// ---------------------------------------------------------------------------
// 主流水线
// ---------------------------------------------------------------------------

// ServeHTTP 是网关的请求入口。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	traceID := observability.EnsureTraceID(r.Header)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	entry := observability.Entry{TraceID: traceID, Method: r.Method, Path: r.URL.Path, Host: r.Host}

	g.metrics.IncInflight(1)

	// ⑦ 收口：无论从哪个分支返回，这里都会执行一次
	defer func() {
		g.metrics.IncInflight(-1)
		entry.Status, entry.Latency, entry.Bytes = sw.status, time.Since(start), sw.bytes
		entry.Err = proxy.ErrFrom(r)
		g.metrics.Observe(entry.Route, entry.Status, entry.Latency)
		g.alog.Write(entry)
	}()

	// ===== 网关自身的端点：不经过路由与鉴权 =====
	switch r.URL.Path {
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(g.metrics.Render()))
		return
	case "/debug/logs":
		writeJSON(sw, http.StatusOK, map[string]any{"lines": g.alog.Tail(debugLogTail)})
		return
	}

	// ===== ① 路由匹配 =====
	m, ok := g.router.Match(r)
	if !ok {
		entry.Route = "unmatched"
		fail(sw, http.StatusNotFound, "route_not_found", "没有匹配的路由", traceID)
		return
	}
	entry.Route, entry.MatchWhy = m.Route.Name, m.Reason

	if m.Route.Upstream == "self" {
		writeJSON(sw, http.StatusOK, map[string]string{"status": "ok", "trace": traceID})
		return
	}

	// ===== ② 鉴权 =====
	identity, err := g.authn.Authenticate(r, m.Route.Auth)
	if err != nil {
		entry.Err = err.Error()
		fail(sw, auth.Status(err), auth.Code(err), err.Error(), traceID)
		return
	}
	entry.Identity = identity.Subject
	r = r.WithContext(auth.WithIdentity(r.Context(), identity))

	// ===== ③ 限流（在鉴权之后，才能按用户维度限）=====
	if p := m.Route.Limit; p.RatePerSec > 0 {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if allow, retry := g.limiter.Bucket(ratelimit.Key(m.Route.Name, identity.Subject, ip), p).Allow(1); !allow {
			entry.LimitHit = true
			g.metrics.IncLimit(m.Route.Name)
			sw.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			sw.Header().Set("X-RateLimit-Policy", fmt.Sprintf("%.0f/s burst=%d", p.RatePerSec, p.Burst))
			fail(sw, http.StatusTooManyRequests, "rate_limited", "触发限流", traceID)
			return
		}
	}

	svc := g.cfg.Service(m.Route.Upstream)
	if svc == nil {
		fail(sw, http.StatusInternalServerError, "bad_config", "上游 "+m.Route.Upstream+" 未定义", traceID)
		return
	}

	// ===== ③ 熔断检查 =====
	cb := g.breakerFor(svc)
	if err := cb.Allow(); err != nil {
		st, _, _ := cb.Snapshot()
		entry.BreakerSt = st.String()
		entry.Err = err.Error()
		fail(sw, http.StatusServiceUnavailable, "circuit_open", "上游熔断中，请稍后重试", traceID)
		return
	}
	st, _, _ := cb.Snapshot()
	entry.BreakerSt = st.String()

	// ===== ④ 选节点 =====
	lb := g.balancerFor(svc)
	hashKey := ""
	if svc.HashKeyFrom != "" {
		hashKey = r.Header.Get(svc.HashKeyFrom)
	}
	if hashKey == "" {
		hashKey, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	up, err := lb.Pick(hashKey)
	if err != nil {
		entry.Err = err.Error()
		fail(sw, http.StatusServiceUnavailable, "no_upstream", err.Error(), traceID)
		return
	}
	entry.Upstream = up.Addr
	// ④ 的收尾：Pick 与 Done 必须成对 —— `least_conn` 完全靠这对调用维护「在途请求数」，
	// 只 Pick 不 Done 的话计数只增不减，节点会被永久判定为「最忙」（AGENTS.md §5 记录过这个缺口）。
	// 用 defer 而不是在两条分支里各写一次：下面 ⑤ 协议转换与 ⑥ 反向代理是互斥分支，
	// 各自都可能在中间提前 return，只有 defer 能保证「有借必有还」。
	defer lb.Done(up.Addr)

	// ===== ⑤ 协议转换 / ⑥ 反向代理 =====
	ctx, cancel := context.WithTimeout(r.Context(), svc.Timeout)
	defer cancel()

	if m.Route.Transcode != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxTranscodeBody))
		_ = r.Body.Close()

		out, err := transcode.Transcode(ctx, g.grpcPool, up.Addr, m.Route.Transcode, body, m.Groups,
			map[string]string{observability.TraceHeader: traceID, "x-user-id": identity.Subject})
		if err != nil {
			entry.Err = "grpc:" + err.Error()
			cb.Report(false)
			fail(sw, http.StatusBadGateway, "grpc_error", err.Error(), traceID)
			return
		}
		cb.Report(true)
		writeJSON(sw, http.StatusOK, map[string]any{"code": 0, "data": out, "trace": traceID, "via": "grpc"})
		return
	}

	// 反向代理路径：把带超时的 ctx 写回请求后交给 ReverseProxy
	r = r.WithContext(ctx)
	g.proxyFor(svc, up, m.Route.StripPrefix).ServeHTTP(sw, r)
	cb.Report(sw.status < 500) // 5xx 视为「上游不健康」
	if st, _, _ := cb.Snapshot(); st != breaker.StateClosed && entry.BreakerSt == breaker.StateClosed.String() {
		entry.BreakerSt = st.String()
		g.metrics.IncTrip(svc.Name)
	}
}
