// Package proxy 实现 ⑥ 反向代理：转发请求。
//
// 用标准库 httputil.ReverseProxy —— 它已经把最难的几件事做对了：
//
//	· 逐跳头（Hop-by-hop）剔除：Connection / Keep-Alive / TE / Transfer-Encoding 等不能转发；
//	· 流式转发请求体与响应体（不会把整个大文件读进内存）；
//	· 支持 HTTP/1.1 升级（WebSocket）。
//
// 我们要自己做的只有三件事：
//
//	① Director：改写目标 URL（含 StripPrefix）、补齐 X-Forwarded-*；
//	② **剥离客户端伪造的内部头** —— 否则任何人加一个 X-Company-Id 就能越权（多租户系统的必修项）；
//	③ ErrorHandler：把上游错误转成可读的 502/504，并区分「超时」与「连不上」。
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gwlab/internal/auth"
	"gwlab/internal/clientip"
	"gwlab/internal/config"
	"gwlab/internal/observability"
)

// InternalHeaders 这些头只允许由可信内部产生，入站一律剥掉。
//
// 为什么必须剥：它们全是**客户端可以随便写**的普通请求头，后端却会把它们当成
// 「网关已经验证过的身份 / 路由决策」来信任，于是伪造一个就能越权或绕过路由限制。
//
//	· X-Company-Id / X-Tenant-Id / X-User-Id：多租户隔离与身份，伪造即横向越权；
//	· X-Internal-Call：内部调用标记，伪造即可冒充「内部流量」跳过业务侧校验；
//	· X-Forwarded-User：网关鉴权后写入的可信用户，入站同名头必须先剥（见 Director ②）；
//	· Forwarded（RFC 7239）：XFF 的标准化写法，等价于可伪造的客户端 IP / 协议信息，
//	  只在剥离列表里处理——网关自己重写的是 X-Real-IP / X-Forwarded-Host / X-Forwarded-Proto，
//	  它们语义上属于「网关重写」而不是「入站剥离」；
//	· X-Original-URL / X-Rewrite-URL：nginx / IIS 系常用注入面，部分后端框架（或前端
//	  反向代理串联时）会按它重写实际处理的路径，从而绕过网关的路由与鉴权规则。
var InternalHeaders = []string{
	"X-Company-Id", "X-Tenant-Id", "X-User-Id", "X-Internal-Call", "X-Forwarded-User",
	"Forwarded", "X-Original-URL", "X-Rewrite-URL",
}

// Proxy 一个（上游节点 × 剥前缀）组合对应的反向代理。
//
// 超时不由本结构负责：请求的 context 已带上 Service.Timeout，随请求一起传播。
type Proxy struct {
	inner *httputil.ReverseProxy
}

// New 构造代理。scheme / TLS 决定怎么连上游：
//   - scheme 空或 http → 明文；
//   - https → 用 upstream.TLSClientConfig()（自定义 CA / SNI / mTLS / 跳过校验）。
//
// 每个（节点 × 剥前缀）组合持有自己的 Transport：TLS 参数是**按节点**配的，共用会串味；
// 连接池粒度因此是「每节点」，与 MaxIdleConnsPerHost 的语义一致。
func New(upstream *config.Upstream, stripPrefix string) *Proxy {
	target, _ := url.Parse(upstream.SchemeOrDefault() + "://" + upstream.Addr)

	transport := &http.Transport{
		Proxy:                 nil, // 明确不走环境变量里的代理
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50, // 默认只有 2，网关这种场景必须调大
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if target.Scheme == "https" {
		tlsCfg, err := upstream.TLSClientConfig()
		if err != nil {
			// 启动期 config.Validate() 已经拦过一遍；这里再失败通常是热重载后证书被挪走。
			// 用一份「校验必然失败」的配置让握手报出明确的 x509 错误 ——
			// **绝不能**静默降级成跳过校验或明文，那是真正危险的失败模式。
			fmt.Fprintf(os.Stderr, "上游 %s 的 TLS 配置加载失败，握手将直接失败: %v\n", upstream.Addr, err)
			tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		transport.TLSClientConfig = tlsCfg
	}

	inner := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host

			// 剥前缀只在**路径段边界**上做：path == prefix 或 path 以 prefix+"/" 开头。
			// ⚠️ 原实现是裸 strings.HasPrefix + TrimPrefix，于是 stripPrefix=/api 时
			// `/apifoo` 会被误剥成 `foo`（前缀撞车）——上游收到一个谁都没定义过的路径。
			// 剥完为空则置 "/"（保留原有行为：/api → /）；剥不掉时路径原样透传。
			if stripPrefix != "" && (req.URL.Path == stripPrefix || strings.HasPrefix(req.URL.Path, stripPrefix+"/")) {
				req.URL.Path = strings.TrimPrefix(req.URL.Path, stripPrefix)
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}
			}
			// ① 真实客户端信息
			// ⚠️ **X-Forwarded-For 不要在这里手动设** —— ReverseProxy 在 Director 之后
			// 自己会把 RemoteAddr 追加到 XFF 上；两边都做就会出现 "127.0.0.1, 127.0.0.1"（本机实测踩过）。
			// 其余几个头 ReverseProxy 不管，必须自己补。
			//
			// ⚠️ 注意：入站 XFF 并不会被清理，因此 XFF 可以伪造（「客户端值, 网关IP」）。
			// 取真实客户端 IP 必须用 X-Real-IP。
			//
			// ⚠️⚠️ 但 X-Real-IP **写谁**不能从 RemoteAddr 推：它是**入站时的直接对端**，
			// 网关在 LB 后面时那就是 LB 的地址 —— 入口刚用 clientip.Resolve 解出的真实客户端，
			// 会在出口被写回成代理。所以优先用入口挂到 ctx 上的解析结果（clientip.WithClient），
			// 只有 ctx 里没有时（直接调用本包 / 测试）才退回 RemoteAddr。
			req.Header.Set("X-Real-IP", clientIPOf(req))
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("X-Forwarded-Proto", scheme(req))

			// ③ 链路追踪：把**本跳**的 traceparent 写给下游（W3C 标准格式），
			// 同时保留 X-Trace-Id 兼容老后端。注意这是"覆盖"而不是"透传"：
			// 我们为本跳生成了新的 span-id，客户端的 span-id 不该出现在这一段链路里。
			if tr, ok := observability.TraceFrom(req.Context()); ok {
				req.Header.Set("traceparent", tr.Header())
				if tr.TraceState != "" {
					req.Header.Set("tracestate", tr.TraceState)
				}
				req.Header.Set(observability.TraceHeader, tr.TraceID)
			}
			// ② 剥离伪造的内部头
			for _, h := range InternalHeaders {
				req.Header.Del(h)
			}
			// 把网关鉴权出来的身份**重新**写进去（此刻它才是可信的）
			if id := auth.FromContext(req.Context()); id != nil {
				req.Header.Set("X-Forwarded-User", id.Subject)
			}
			req.Host = target.Host
		},
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond, // SSE / 流式响应更平滑
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// ③ 区分超时与连不上：运维排障时这两个的处置完全不同
			status, msg := http.StatusBadGateway, "upstream error"
			switch {
			case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err):
				status, msg = http.StatusGatewayTimeout, "upstream timeout"
			case isConnRefused(err):
				status, msg = http.StatusBadGateway, "upstream refused"
			default:
				// 请求体超限：gateway 用 http.MaxBytesReader 兜住 chunked / 谎报 Content-Length
				// 的情况，错误只在真正读取时才暴露，这里把它映射成 413 而不是 502。
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					status, msg = http.StatusRequestEntityTooLarge, "request body too large"
				}
			}
			// 把错误记进 context（Header 传不回来，见 ErrFrom 的说明）——
			// 这样访问日志的 err= 终于能拿到代理链路的错误，重试循环也要靠它。
			markErr(r, msg)

			// 带重试的路由：只要**还没写出任何字节**，就把错误响应的写法交给上层，
			// 由重试循环决定"换个节点再试"还是"最终报错"。
			// 已经写出去（响应中途失败）就没法重试，只能就地结束。
			if deferred(r) && !wroteAlready(w) {
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"` + msg + `","trace":"` + r.Header.Get(observability.TraceHeader) + `"}`))
		},
	}
	return &Proxy{inner: inner}
}

// ServeHTTP 转发一次请求。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.inner.ServeHTTP(w, r) }

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func isConnRefused(err error) bool {
	return strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "No connection could be made")
}

// ErrHeader 是「本次转发的错误」的兼容载体头。
//
// ⚠️ 它其实**传不回来**：ReverseProxy 把出站请求 Clone 了一份，Header 是深拷贝，
// 在 ErrorHandler 里改它对主流程不可见（这就是"proxy 的上游错误不落访问日志"那个老缺口的成因）。
// 错误经 context 传递（`ErrHolder`），ErrFrom 优先读 context，Header 只作兼容兜底。
const ErrHeader = "X-GW-Upstream-Error"

// ErrHolder 承载「本次转发的错误」。用 context 而不是 Header 的原因见 ErrHeader。
type ErrHolder struct {
	mu  sync.Mutex
	msg string
}

type errHolderKey struct{}

// WithErrHolder 给请求挂一个错误容器（调用方在交给 ReverseProxy 之前调用）。
func WithErrHolder(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), errHolderKey{}, &ErrHolder{}))
}

// WithDeferredError 标记「上游错误先别写响应，交给调用方决定」（重试用）。
func WithDeferredError(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), deferredKey{}, true))
}

func deferred(r *http.Request) bool {
	v, _ := r.Context().Value(deferredKey{}).(bool)
	return v
}

type deferredKey struct{}

// clientIPOf 取要写进 X-Real-IP 的地址。
//
// 优先用入口（gateway.ServeHTTP）解析好并挂到 ctx 上的真实客户端 IP —— 那是唯一
// 考虑过「对端是否可信、XFF 链怎么走」的结论；ctx 里没有时才退回 RemoteAddr
// （直接调用本包、或测试里手工构造的请求走这一支）。
//
// 退回时按「主机部分」取（剥端口）；SplitHostPort 失败则整串当主机 ——
// 这样无端口的 "1.2.3.4" 不会写出一个空值，而 IPv6 的 "[::1]:1234" 也能剥对。
func clientIPOf(req *http.Request) string {
	if ip, ok := clientip.FromClient(req.Context()); ok {
		return ip
	}
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		return host
	}
	return req.RemoteAddr
}

// markErr 记录本次转发的错误（ErrorHandler 里调用；context 能穿过 Clone，Header 不能）。
func markErr(r *http.Request, msg string) {
	if h, ok := r.Context().Value(errHolderKey{}).(*ErrHolder); ok {
		h.mu.Lock()
		h.msg = msg
		h.mu.Unlock()
	}
}

// ErrFrom 读取本次转发的错误（调用方在 ServeHTTP 返回之后调用）。
func ErrFrom(r *http.Request) string {
	if h, ok := r.Context().Value(errHolderKey{}).(*ErrHolder); ok {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.msg
	}
	return r.Header.Get(ErrHeader) // 兼容：老路径没有挂 ErrHolder
}

// wroteReporter 由 gateway 的 statusWriter 实现，用于判断「响应是否已经开始写出」——
// 已经写出去的响应没法重试。
type wroteReporter interface{ Wrote() bool }

func wroteAlready(w http.ResponseWriter) bool {
	if wr, ok := w.(wroteReporter); ok {
		return wr.Wrote()
	}
	return false
}
