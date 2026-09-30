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
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"gwlab/internal/auth"
	"gwlab/internal/config"
	"gwlab/internal/observability"
)

// InternalHeaders 这些头只允许由可信内部产生，入站一律剥掉。
var InternalHeaders = []string{
	"X-Company-Id", "X-Tenant-Id", "X-User-Id", "X-Internal-Call", "X-Forwarded-User",
}

// Proxy 一个（上游节点 × 剥前缀）组合对应的反向代理。
//
// 超时不由本结构负责：请求的 context 已带上 Service.Timeout，随请求一起传播。
type Proxy struct {
	inner *httputil.ReverseProxy
}

// New 构造代理。
func New(upstream *config.Upstream, stripPrefix string) *Proxy {
	target, _ := url.Parse("http://" + upstream.Addr)

	inner := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host

			if stripPrefix != "" && strings.HasPrefix(req.URL.Path, stripPrefix) {
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
			clientIP, _, _ := net.SplitHostPort(req.RemoteAddr)
			req.Header.Set("X-Real-IP", clientIP)
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("X-Forwarded-Proto", scheme(req))

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
		Transport: &http.Transport{
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
		},
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
				// 的情况，错误只在真正读取时才暴露，这里把它映射成 413 而不是 502（审计 P0-3）。
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					status, msg = http.StatusRequestEntityTooLarge, "request body too large"
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"` + msg + `","trace":"` + r.Header.Get(observability.TraceHeader) + `"}`))
			// ⚠️ 已知问题：这里的 r 是 ReverseProxy 内部 req.Clone(ctx) 出来的**出站请求**，
			// 其 Header 是深拷贝，所以对它的修改**不会**传回主流程持有的原始请求
			//（net/http/httputil/reverseproxy.go: outreq := req.Clone(ctx) → getErrorHandler()(rw, outreq, err)）。
			// 结果：代理链路的错误信息不会落进访问日志的 err= 字段（鉴权/限流/熔断分支不受影响）。
			// 若要修，应改用 context 传递错误（context 是共享的，Clone 不会拷贝）。
			r.Header.Set(ErrHeader, err.Error())
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

// ErrHeader 是「本次转发的错误」的载体头（见 ErrorHandler 中的已知问题说明）。
const ErrHeader = "X-GW-Upstream-Error"

// ErrFrom 读取本次转发的错误。
func ErrFrom(r *http.Request) string { return r.Header.Get(ErrHeader) }
