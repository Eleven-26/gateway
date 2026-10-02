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
	"net/http"
	"net/netip"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gwlab/internal/auth"
	"gwlab/internal/balancer"
	"gwlab/internal/breaker"
	"gwlab/internal/clientip"
	"gwlab/internal/config"
	"gwlab/internal/observability"
	"gwlab/internal/overload"
	"gwlab/internal/proxy"
	"gwlab/internal/ratelimit"
	"gwlab/internal/router"
	"gwlab/internal/transcode"
)

const (
	maxTranscodeBody = 1 << 20 // 协议转换读取请求体的上限：1MB
	maxBodyBytes     = 8 << 20 // 反代链路的请求体上限：8MB —— 流式转发若没有上限，单个请求就能打满上游/磁盘
	accessLogKeep    = 200     // 内存中保留的访问日志条数
	debugLogTail     = 50      // /debug/logs 返回的条数
)

// state 是一次配置快照：配置 + 编译好的路由表 + 可信代理 + 鉴权器。
//
// 热重载时**整体替换**（`atomic.Pointer`）：请求路径只 `Load()` 一次，既不与重载互相阻塞，
// 也不用给热路径加锁；同一个请求内的路由/鉴权/选节点一定取自同一份快照。
type state struct {
	cfg            *config.Config
	router         *router.Router
	trustedProxies []netip.Prefix // 可信代理网段：只有来自这些地址的请求才采信 XFF/X-Real-IP
	authn          *auth.Authenticator
}

// Gateway 是实现了 http.Handler 的网关本体。
type Gateway struct {
	st atomic.Pointer[state] // 配置快照：热重载时原子替换（见 Reload）

	rl       ratelimit.Backend // 限流后端：进程内令牌桶，或共享状态服务
	bshare   breaker.Store     // 共享熔断状态；nil = 单副本语义
	gate     *overload.Gate    // 负载保护闸门；nil = 不启用
	tripCh   chan tripEvent    // 本地跳闸的发布队列（有界，满了丢弃并打点）
	stop     chan struct{}     // 关闭信号：停掉后台同步协程
	wg       sync.WaitGroup
	grpcPool *transcode.Pool
	metrics  *observability.Metrics
	alog     *observability.AccessLog

	mu        sync.Mutex
	breakers  map[string]*breaker.Breaker
	balancers map[string]balancer.Balancer
	proxies   map[string]*proxy.Proxy
}

// snapshot 取当前配置快照（请求路径上只调一次，后续都用它）。
func (g *Gateway) snapshot() *state { return g.st.Load() }

// newState 由配置构造一份快照：校验 → 解析可信代理 → 编译路由表 → 构造鉴权器。
// 任何一步失败都不产生半成品，Reload 靠这一点做到「要么全生效、要么完全不变」。
func newState(cfg *config.Config) (*state, error) {
	// 启动/重载即校验：错误配置在启动期暴露，而不是等某个请求打进来变成 500
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}
	trusted, err := clientip.ParseTrusted(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("TrustedProxies 解析失败: %w", err)
	}
	r, err := router.New(cfg.Routes)
	if err != nil {
		return nil, err
	}
	return &state{
		cfg:            cfg,
		router:         r,
		trustedProxies: trusted,
		authn:          auth.New(cfg.JWTSecret, cfg.APIKey),
	}, nil
}

// New 装配一个网关。熔断器 / 均衡器 / 代理都是**按需惰性创建**并缓存的。
func New(cfg *config.Config) (*Gateway, error) {
	// 校验配置 → 解析可信代理 → 编译路由表 → 构造鉴权器，产出一份配置快照。
	st, err := newState(cfg)
	if err != nil {
		return nil, err
	}

	metrics := observability.NewMetrics()
	g := &Gateway{
		// 限流后端按配置选择：共享状态判定失败要计数，否则 fail-open 会把失效藏起来
		rl:        newLimitBackend(cfg, func(error) { metrics.IncStateError("ratelimit") }),
		grpcPool:  transcode.NewPool(),
		metrics:   metrics,
		alog:      observability.NewAccessLog(accessLogKeep),
		breakers:  map[string]*breaker.Breaker{},
		balancers: map[string]balancer.Balancer{},
		proxies:   map[string]*proxy.Proxy{},
	}
	if g.gate = overload.New(cfg.Overload.MaxInflight, cfg.Overload.MaxQueue); g.gate != nil {
		metrics.AttachOverload(g.gate.Limit(), g.gate.Stats)
	}
	g.st.Store(st)
	g.stop = make(chan struct{})
	if strings.TrimSpace(cfg.SharedState.URL) != "" {
		g.startSharedBreaker(cfg, metrics)
	}
	return g, nil
}

// Close 释放网关持有的资源（gRPC 连接池）。
func (g *Gateway) Close() {
	if g.stop != nil {
		close(g.stop) // 先停后台同步（它可能正在用共享状态存储）
		g.wg.Wait()
	}
	g.grpcPool.Close()
}

// newLimitBackend 按配置选择限流后端：
// RateLimit.URL 为空 → 进程内令牌桶（默认，单副本语义）；否则 → 共享状态服务。
//
// onErr 在共享后端判定失败（超时/连不上/非 2xx）时被调用，用来打 gw_shared_state_errors_total。
func newLimitBackend(cfg *config.Config, onErr func(error)) ratelimit.Backend {
	if strings.TrimSpace(cfg.SharedState.URL) == "" {
		return ratelimit.NewLocalBackend()
	}
	failOpen := true // 默认 fail-open：限流暂时失效，好过把自己的服务打成全量 503
	if cfg.SharedState.FailOpen != nil {
		failOpen = *cfg.SharedState.FailOpen
	}
	hb := ratelimit.NewHTTPBackend(cfg.RateLimitEndpoint(), time.Duration(cfg.SharedState.Timeout), failOpen)
	hb.OnError = onErr
	return hb
}
func (g *Gateway) breakerFor(svc *config.Service) *breaker.Breaker {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.breakers[svc.Name]; ok {
		return b
	}
	b := breaker.New(svc.Breaker)
	if g.bshare != nil {
		name := svc.Name
		// 只在**本地跳闸**时发布（采纳远端状态不回调，否则副本之间会互相转发形成风暴）
		b.OnTrip = func(until time.Time) { g.publishTrip(name, until) }
	}
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

// Wrote 报告响应是否已经开始写出。重试判定要用它：任何已经写出字节的失败
// 都不能再换节点重来 —— 客户端已经收到部分内容，重试只会让响应变成两段拼接。
func (w *statusWriter) Wrote() bool { return w.wrote }

// Unwrap 让 http.ResponseController 能剥到包装下面的原始 ResponseWriter。
//
// ⚠️ 这层不是可选的：ReverseProxy 处理 101 协议升级时走
// `http.NewResponseController(rw).Hijack()`（net/http/httputil/reverseproxy.go:838-841），
// 而 ResponseController 只认 `http.Hijacker` 或 `Unwrap() http.ResponseWriter`
// （net/http/responsecontroller.go:66-75）。两个都不提供时它返回 ErrNotSupported，
// 升级请求会被 ErrorHandler 答成 502 —— 即「宣称支持 WebSocket，实际 100% 失败」（已实测复现）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ReadFrom 转发底层的零拷贝路径（sendfile），否则大响应体会退化成用户态缓冲拷贝。
// 必须转发到**原始 writer**：转发到 w 自己会因为 w 也实现了 ReaderFrom 而无限递归。
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += n
	return n, err
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
// 管理面（独立监听）
// ---------------------------------------------------------------------------

// AdminHandler 返回管理端点的 handler：`/metrics`、`/debug/logs`、`/readyz`。
//
// 为什么要独立监听：这三个端点原本排在 ServeHTTP 的**路由与鉴权之前**，任何能访问业务端口的人
// 都能读到内部状态（上游地址、熔断状态、限流拒绝情况）。拆开之后生产可以只让管理端口绑定
// 回环或内网；`AdminListenAddr` 留空时才退回「与业务同端口」的老行为。
func (g *Gateway) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) { g.handleMetrics(w) })
	mux.HandleFunc("/debug/logs", func(w http.ResponseWriter, r *http.Request) { g.handleDebugLogs(w) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { g.handleReady(w) })
	return mux
}

// handleMetrics 输出 Prometheus 文本：全局指标 + **节点级**指标。
func (g *Gateway) handleMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = io.WriteString(w, g.metrics.Render())
	_, _ = io.WriteString(w, g.nodeMetrics())
}

// handleDebugLogs 返回内存里最近若干条访问日志。
func (g *Gateway) handleDebugLogs(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"lines": g.alog.Tail(debugLogTail)})
}

// handleReady 就绪检查：`/healthz` 只说明「进程活着」，能不能接流量还要看上游是否可用。
// 判定规则：任一**已被使用过**的服务处于熔断（open / half-open），或该服务所有节点都被摘除 → 503。
// 还没被请求过的服务不算不健康（均衡器是惰性创建的，没被用过就没有状态）。
func (g *Gateway) handleReady(w http.ResponseWriter) {
	type svcState struct {
		Service   string `json:"service"`
		Breaker   string `json:"breaker"`
		Upstreams int    `json:"upstreams"`
		Ejected   int    `json:"ejected"`
	}

	g.mu.Lock()
	states := make([]svcState, 0, len(g.balancers))
	reasons := []string{}
	for name, lb := range g.balancers {
		st := svcState{Service: name}
		stats := lb.Stats()
		st.Upstreams = len(stats)
		for _, s := range stats {
			if s.Ejected {
				st.Ejected++
			}
		}
		if cb, ok := g.breakers[name]; ok {
			bs, _, _ := cb.Snapshot()
			st.Breaker = bs.String()
			if bs != breaker.StateClosed {
				reasons = append(reasons, name+" 熔断状态="+bs.String())
			}
		}
		if st.Upstreams > 0 && st.Ejected == st.Upstreams {
			reasons = append(reasons, name+" 所有节点都被摘除")
		}
		states = append(states, st)
	}
	g.mu.Unlock()

	sort.Slice(states, func(i, j int) bool { return states[i].Service < states[j].Service })
	if len(reasons) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unready", "reasons": reasons, "services": states,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "services": states})
}

// nodeMetrics 输出节点级指标：在途、累计失败、是否被摘除。
//
// 这些数字来自均衡器的运行时状态，所以**只有被请求过的服务**才会出现（惰性创建）；
// 服务名排序输出，保证 /metrics 的文本稳定、可 diff、可断言。
func (g *Gateway) nodeMetrics() string {
	g.mu.Lock()
	names := make([]string, 0, len(g.balancers))
	for name := range g.balancers {
		names = append(names, name)
	}
	g.mu.Unlock()
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# HELP gw_upstream_inflight 每个上游节点的在途请求数\n")
	b.WriteString("# TYPE gw_upstream_inflight gauge\n")
	type row struct {
		svc  string
		stat balancer.NodeStat
	}
	var rows []row
	g.mu.Lock()
	for _, name := range names {
		for _, s := range g.balancers[name].Stats() {
			rows = append(rows, row{svc: name, stat: s})
		}
	}
	g.mu.Unlock()
	for _, r := range rows {
		fmt.Fprintf(&b, "gw_upstream_inflight{service=%q,addr=%q} %d\n", r.svc, r.stat.Addr, r.stat.InFlight)
	}
	b.WriteString("# HELP gw_upstream_failures_total 每个上游节点的累计失败数（被动摘除的原始计数）\n")
	b.WriteString("# TYPE gw_upstream_failures_total counter\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "gw_upstream_failures_total{service=%q,addr=%q} %d\n", r.svc, r.stat.Addr, r.stat.Failures)
	}
	b.WriteString("# HELP gw_upstream_ejected 每个上游节点当前是否被摘除（1=已摘除，正在冷却或探测）\n")
	b.WriteString("# TYPE gw_upstream_ejected gauge\n")
	for _, r := range rows {
		v := 0
		if r.stat.Ejected {
			v = 1
		}
		fmt.Fprintf(&b, "gw_upstream_ejected{service=%q,addr=%q} %d\n", r.svc, r.stat.Addr, v)
	}
	return b.String()
}

// ServeHTTP 是网关的请求入口。
// net/http 每收到一个请求，就在连接的 goroutine 里调用 ServeHTTP(w, r)。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// 链路上下文：优先 W3C traceparent，其次兼容 X-Trace-Id，都没有就新生成；
	// 并为本跳生成 span-id。放进 ctx 是为了让 proxy.Director 能写出正确的 traceparent。
	tr := observability.StartTrace(r.Header)
	traceID := tr.TraceID
	r = r.WithContext(observability.WithTrace(r.Context(), tr))
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	entry := observability.Entry{TraceID: traceID, SpanID: tr.SpanID, Method: r.Method, Path: r.URL.Path, Host: r.Host}
	// 整个请求共用这一份快照：热重载只换指针，不会出现「路由用新配置、选节点用旧配置」
	snap := g.snapshot()
	// 真实客户端 IP：只有直连对端本身是可信代理时才采信 XFF / X-Real-IP。
	// 只解析一次，限流、日志与「哈希键回落」三处共用，避免口径不一致。
	clientIP := clientip.Resolve(r.RemoteAddr, r.Header, snap.trustedProxies)
	entry.ClientIP = clientIP

	// 在线请求数 +1。
	g.metrics.IncInflight(1)

	// ⑦ 收口：无论从哪个分支返回，这里都会执行一次
	defer func() {
		// 在线请求数 -1。
		g.metrics.IncInflight(-1)
		entry.Status, entry.Latency, entry.Bytes = sw.status, time.Since(start), sw.bytes
		// 只在真的取到上游错误时才覆盖 Err —— 否则会把 panic 兜底写下的错误信息抹掉
		if e := proxy.ErrFrom(r); e != "" {
			entry.Err = e
		}
		g.metrics.Observe(entry.Route, entry.Status, entry.Latency)
		g.alog.Write(entry)
	}()

	// ⑦′ panic 兜底：注册在收口之后 —— defer 是 LIFO，所以它会**先**执行，再把结果交给收口记录。
	//
	// 为什么必须有：net/http 自己在每个连接 goroutine 上有 recover（server.go 的 conn.serve），
	// 所以 panic 不会让进程崩掉，但客户端只会看到连接被重置（HTTP 000 / curl exit 52），
	// 而 /metrics 与 /debug/logs 里**什么都留不下** —— 这比崩溃更难排查（本机实测过）。
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		g.metrics.IncPanic()
		entry.Err = fmt.Sprintf("panic: %v", rec)
		fmt.Fprintf(os.Stderr, "gateway panic: %v\n%s\n", rec, debug.Stack())
		// 响应头已经发出去了就补不了 500（客户端已收到部分内容），此时只留日志与指标。
		if !sw.wrote {
			fail(sw, http.StatusInternalServerError, "internal_error", "网关内部错误", traceID)
		}
	}()

	// ===== 管理端点：默认走独立监听的 AdminHandler；
	//        只有配置里没给 AdminListenAddr 时才退回业务端口 =====
	if snap.cfg.AdminListenAddr == "" {
		switch r.URL.Path {
		case "/metrics":
			g.handleMetrics(sw)
			return
		case "/debug/logs":
			g.handleDebugLogs(sw)
			return
		case "/readyz":
			g.handleReady(sw)
			return
		}
	}

	// ===== ① 路由匹配 =====
	// 按 Host、Path、Method、Header 匹配，返回路由、匹配原因、路径参数。不匹配直接 404。
	m, ok := snap.router.Match(r)
	if !ok {
		entry.Route = "unmatched"
		fail(sw, http.StatusNotFound, "route_not_found", "没有匹配的路由", traceID)
		return
	}
	entry.Route, entry.MatchWhy = m.Route.Name, m.Reason

	// ===== 请求体上限（放在鉴权之前：明显超限的请求不必再花鉴权/限流的成本）=====
	// 反代是流式转发的，没有上限就等于允许任意大的请求体打满上游/磁盘。
	// 两段式：先按 Content-Length 快速拒绝，再用 MaxBytesReader 兜住 chunked / 谎报长度的情况
	//（后者只在真正读取时才暴露，由 proxy 映射成 413；必须传原始 w 让 requestTooLarge() 断言生效）。
	if r.ContentLength > maxBodyBytes {
		entry.Err = "body_too_large"
		fail(sw, http.StatusRequestEntityTooLarge, "body_too_large",
			fmt.Sprintf("请求体超过上限 %d 字节", int64(maxBodyBytes)), traceID)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	// self 路由短路，/healthz 这类路由由网关自己处理，不转发。
	if m.Route.Upstream == "self" {
		writeJSON(sw, http.StatusOK, map[string]string{"status": "ok", "trace": traceID})
		return
	}

	// ===== 负载保护：并发上限 + 有界排队 =====
	// 位置刻意选在**路由与 self 之后、鉴权之前**：
	//   - self 路由（/healthz）不参与保护 —— 负载高时把健康检查也拒了，LB 会把实例摘掉，
	//     剩下来的实例压力更大，反而加速雪崩；
	//   - 鉴权/限流/转码/反代才是真正吃资源的部分，闸门放在它们前面就够了。
	if g.gate != nil {
		queueTO := time.Duration(snap.cfg.Overload.QueueTimeout)
		if queueTO <= 0 {
			queueTO = time.Second
		}
		retryAfter := time.Duration(snap.cfg.Overload.RetryAfter)
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		qctx, cancelQueue := context.WithTimeout(r.Context(), queueTO)
		release, reason, qerr := g.gate.Acquire(qctx)
		cancelQueue() // 排队结束就撤掉这个 ctx（后面的转发用自己的超时）
		if qerr != nil {
			entry.Err = "overload:" + string(reason)
			g.metrics.IncOverload(m.Route.Name)
			sw.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			sw.Header().Set("X-Load-Shed", string(reason))
			fail(sw, http.StatusServiceUnavailable, "overloaded", "网关过载，请稍后重试", traceID)
			return
		}
		defer release() // 必须归还：漏一次就永久少一个并发额度
	}

	// ===== ② 鉴权 =====
	// 根据路由的 Auth 策略校验 JWT 或 API Key。
	// 失败返回 401 或 403。
	// 成功把身份注入 context。
	identity, err := snap.authn.Authenticate(r, m.Route.Auth)
	if err != nil {
		entry.Err = err.Error()
		fail(sw, auth.Status(err), auth.Code(err), err.Error(), traceID)
		return
	}
	entry.Identity = identity.Subject
	r = r.WithContext(auth.WithIdentity(r.Context(), identity))

	// ===== ③ 限流（在鉴权之后，才能按用户维度限）=====
	if p := m.Route.Limit; p.RatePerSec > 0 {
		// 限流 key = 路由名 + 用户身份 + IP。
		if allow, retry := g.rl.Allow(ratelimit.Key(m.Route.Name, identity.Subject, clientIP), p); !allow {
			entry.LimitHit = true
			g.metrics.IncLimit(m.Route.Name)
			sw.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			sw.Header().Set("X-RateLimit-Policy", fmt.Sprintf("%.0f/s burst=%d", p.RatePerSec, p.Burst))
			fail(sw, http.StatusTooManyRequests, "rate_limited", "触发限流", traceID)
			return
		}
	}

	// 按名字查服务定义，不存在说明配置错误。
	svc := snap.cfg.Service(m.Route.Upstream)
	if svc == nil {
		fail(sw, http.StatusInternalServerError, "bad_config", "上游 "+m.Route.Upstream+" 未定义", traceID)
		return
	}

	// ===== ③ 熔断检查 =====
	// 在选节点之前：熔断打开时直接快速失败，不走选节点和转发。
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

	// Allow 与 Report 必须成对：下面还要经过动态选节点与两条分支，
	// 任何提前 return（乃至 panic）都该算作「这次调用没成功」，用 defer 兜住 ——
	// 否则半开状态的探测槽位会泄漏，恢复流程可能被永久卡住。
	breakerReported := false
	defer func() {
		if !breakerReported {
			cb.Report(false)
		}
	}()

	// ===== ④ 选节点 → ⑤ 协议转换 / ⑥ 反向代理（可重试）=====
	lb := g.balancerFor(svc)
	hashKey := ""
	if svc.HashKeyFrom != "" {
		hashKey = r.Header.Get(svc.HashKeyFrom)
	}
	if hashKey == "" {
		hashKey = clientIP
	}

	// 请求级总预算：每路由超时优先，其次是服务超时。所有尝试都挂在这个 ctx 下，
	// 所以"重试把总耗时拖长"是有限度的（PerTryTimeout 只是把总预算再切细）。
	total := svc.Timeout
	if m.Route.Timeout > 0 {
		total = time.Duration(m.Route.Timeout)
	}
	reqCtx, cancelAll := context.WithTimeout(r.Context(), total)
	defer cancelAll()

	plan := planRetry(m.Route, r)

	// 上游往返耗时：覆盖所有尝试，收口时写进访问日志的 upstream_ms
	upstreamStart := time.Now()
	defer func() { entry.UpstreamMs = time.Since(upstreamStart).Milliseconds() }()

	// 协议转换要读请求体：**在循环外读一次**（循环里读的话第二次尝试就只能拿到空 body）
	var grpcBody []byte
	if m.Route.Transcode != nil {
		grpcBody, _ = io.ReadAll(io.LimitReader(r.Body, maxTranscodeBody))
		_ = r.Body.Close()
	}

	// baseReq 保留"没挂过任何 per-attempt ctx"的请求：每次尝试都从它派生，
	// 避免 ctx 在重试之间层层嵌套。
	baseReq := r

	for attempt := 0; attempt < plan.attempts; attempt++ {
		up, perr := lb.Pick(hashKey)
		if perr != nil {
			entry.Err = perr.Error()
			fail(sw, http.StatusServiceUnavailable, "no_upstream", perr.Error(), traceID)
			return
		}
		entry.Upstream = up.Addr

		// 单次尝试在闭包里跑：Pick/Done 必须严格配对（least_conn 靠它维护在途数），
		// 而 defer 在循环里只会在函数返回时执行 —— 所以用闭包把作用域收窄。
		committed, attemptErr, healthy := func() (bool, string, bool) {
			defer lb.Done(up.Addr)

			attemptCtx, cancelAttempt := reqCtx, context.CancelFunc(func() {})
			if m.Route.Retry != nil && m.Route.Retry.PerTryTimeout > 0 {
				attemptCtx, cancelAttempt = context.WithTimeout(reqCtx, time.Duration(m.Route.Retry.PerTryTimeout))
			}
			defer cancelAttempt()

			// ErrHolder 让上游错误能被读出来（context 才能穿过 ReverseProxy 的 Clone）
			req := proxy.WithErrHolder(baseReq.WithContext(attemptCtx))
			if plan.deferErr {
				req = proxy.WithDeferredError(req) // 还有重试机会：先别写 502
			}
			// 回写外层 r：收口那段 defer 读的是它，ErrHolder 不在原始请求上就读不到错误
			r = req

			if m.Route.Transcode != nil {
				out, terr := transcode.Transcode(attemptCtx, g.grpcPool, up, m.Route.Transcode, grpcBody, m.Groups,
					map[string]string{observability.TraceHeader: traceID, "x-user-id": identity.Subject})
				if terr != nil {
					// gRPC 失败同样是"一个字节都没写出去"，所以也可以重试
					return false, "grpc:" + terr.Error(), false
				}
				writeJSON(sw, http.StatusOK, map[string]any{"code": 0, "data": out, "trace": traceID, "via": "grpc"})
				return true, "", true
			}

			g.proxyFor(svc, up, m.Route.StripPrefix).ServeHTTP(sw, req)
			if sw.wrote {
				// 响应已经写出（正常响应，或响应中途失败后 ErrorHandler 补的 5xx）：不能再重试
				return true, proxy.ErrFrom(req), sw.status < 500
			}
			return false, proxy.ErrFrom(req), false
		}()

		if committed {
			breakerReported = true
			cb.Report(healthy)
			lb.Report(up.Addr, healthy) // 节点级被动摘除：连续失败到阈值就摘掉这个地址
			if st, _, _ := cb.Snapshot(); st != breaker.StateClosed && entry.BreakerSt == breaker.StateClosed.String() {
				entry.BreakerSt = st.String()
				g.metrics.IncTrip(svc.Name)
			}
			// 这次成功是"重试换节点"换来的：把上一次的错误标成重试痕迹，
			// 免得日志里出现 `status=200` 却带 `err=` 的迷惑组合。
			if attempt > 0 && entry.Err != "" {
				entry.Err = "retried(" + entry.Err + ")"
			}
			return
		}

		// 一个字节都没写出去 → 这次尝试彻底失败。节点级上报失败，然后看还能不能再试。
		lb.Report(up.Addr, false)
		if attemptErr == "" {
			attemptErr = "upstream error"
		}
		entry.Err = attemptErr
		if attempt < plan.attempts-1 && reqCtx.Err() == nil {
			continue // 换一个节点（Pick 会把刚才失败的算进去，天然分散）
		}

		breakerReported = true
		cb.Report(false)
		status := http.StatusBadGateway
		if strings.Contains(attemptErr, "timeout") {
			status = http.StatusGatewayTimeout
		}
		fail(sw, status, "upstream_error", attemptErr, traceID)
		return
	}
}
