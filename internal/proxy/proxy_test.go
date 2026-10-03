package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gwlab/internal/clientip"
	"gwlab/internal/config"
)

// forgedHeaders 是「客户端可以随便写、后端却会信」的内部头（对应 InternalHeaders）。
var forgedHeaders = []string{
	"X-Company-Id", "X-Tenant-Id", "X-User-Id", "X-Internal-Call", "X-Forwarded-User",
	"Forwarded", "X-Original-URL", "X-Rewrite-URL",
}

// rewrittenHeaders 是网关自己在 Director 里**重写**的真实客户端信息：
// 反向控制用 —— 它们必须仍然到达上游（剥离列表里没有它们）。
var rewrittenHeaders = []string{"X-Real-IP", "X-Forwarded-Host", "X-Forwarded-Proto"}

// echoedHeaders = 上游需要回显的两组头。
var echoedHeaders = append(append([]string{}, forgedHeaders...), rewrittenHeaders...)

// newEchoUpstream 起一个回显上游：r.URL.Path 同时回显到 X-Echo-Path 与响应体，
// 关心的请求头回显到 X-Echo-<头名>（值为空则不设，便于断言「头根本不存在」）。
func newEchoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Path", r.URL.Path)
		for _, h := range echoedHeaders {
			if v := r.Header.Get(h); v != "" {
				w.Header().Set("X-Echo-"+h, v)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// upstreamAddr 把 httptest 的 URL 转成 config.Upstream 要的 host:port。
func upstreamAddr(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

func echoOf(res *http.Response, h string) string { return res.Header.Get("X-Echo-" + h) }

// TestStripPrefixSegmentBoundary 盯住剥前缀的**路径段边界**：
// 原实现是裸 strings.HasPrefix + TrimPrefix，stripPrefix=/api 时 `/apifoo`
// 会被误剥成 `foo` —— 上游收到一个谁都没定义过的路径。
func TestStripPrefixSegmentBoundary(t *testing.T) {
	up := newEchoUpstream(t)
	p := New(&config.Upstream{Addr: upstreamAddr(up)}, "/api")

	cases := []struct {
		name string
		req  string
		want string
	}{
		{"段边界内的前缀被剥掉", "/api/order/1", "/order/1"},
		{"前缀撞车：/apifoo 不是 /api 的一段，保持原样", "/apifoo", "/apifoo"},
		{"剥完为空则置 /（保留原有行为）", "/api", "/"},
		{"无关路径原样透传", "/health", "/health"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.req, nil))
			res := w.Result()
			defer res.Body.Close()

			if got := echoOf(res, "Path"); got != tc.want {
				t.Errorf("上游收到的路径 = %q，期望 %q", got, tc.want)
			}
			if got := w.Body.String(); got != tc.want {
				t.Errorf("上游响应体 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestStripPrefixEmpty 空 stripPrefix 不做任何改写（旧行为不能回退）。
func TestStripPrefixEmpty(t *testing.T) {
	up := newEchoUpstream(t)
	p := New(&config.Upstream{Addr: upstreamAddr(up)}, "")

	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/order/1", nil))
	if got := echoOf(w.Result(), "Path"); got != "/api/order/1" {
		t.Errorf("stripPrefix 为空时路径不该变，实际 %q", got)
	}
}

// TestInternalHeadersStripped 盯住「可伪造的内部头必须剥离」：
// 多租户场景下伪造一个 X-Company-Id 就能横向越权；Forwarded / X-Original-URL /
// X-Rewrite-URL 则是客户端伪造客户端 IP、或诱使后端按伪造路径重写路由的注入面。
func TestInternalHeadersStripped(t *testing.T) {
	up := newEchoUpstream(t)
	p := New(&config.Upstream{Addr: upstreamAddr(up)}, "")

	req := httptest.NewRequest(http.MethodGet, "/api/order/1", nil)
	for _, h := range forgedHeaders {
		req.Header.Set(h, "evil-forged-value")
	}
	req.Header.Set("Forwarded", "for=1.2.3.4;proto=https")
	req.Header.Set("X-Original-URL", "/admin")
	req.Header.Set("X-Rewrite-URL", "/admin")

	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	res := w.Result()
	defer res.Body.Close()

	for _, h := range forgedHeaders {
		if got := echoOf(res, h); got != "" {
			t.Errorf("入站伪造的 %s 泄漏到了上游：%q", h, got)
		}
	}
	// 反向控制：网关自己重写的真实客户端信息必须仍在（别把「网关重写」和「入站剥离」搞混）。
	for _, h := range rewrittenHeaders {
		if got := echoOf(res, h); got == "" {
			t.Errorf("网关重写的 %s 不该被剥掉，实际为空", h)
		}
	}
}

// TestRealIPFromContext 盯住「出口写 X-Real-IP 用入口解析好的地址」。
//
// 为什么必须有这条：网关在 LB 后面时入站 RemoteAddr 是 LB，若从它推，
// 入口刚用 clientip.Resolve 解出来的真实客户端会在出口被写回成代理地址 ——
// 上游按 IP 限流/审计时看到的又是 LB，问题只是换了个位置复发。
func TestRealIPFromContext(t *testing.T) {
	up := newEchoUpstream(t)
	p := New(&config.Upstream{Addr: upstreamAddr(up)}, "")

	cases := []struct {
		name       string
		remoteAddr string
		ctxIP      string // "" = 不挂 ctx；"(empty)" = 挂一个空值
		want       string
	}{
		{"ctx 有解析结果 → 用它，而不是 RemoteAddr", "10.0.1.5:5000", "9.9.9.9", "9.9.9.9"},
		{"客户端是 IPv6 → 原样写出", "10.0.1.5:5000", "2001:db8::1", "2001:db8::1"},
		{"ctx 没挂过 → 退回 RemoteAddr 的主机部分", "10.0.1.5:5000", "", "10.0.1.5"},
		{"ctx 挂的是空值 → 同样退回（WithClient 不挂空值）", "10.0.1.5:5000", "(empty)", "10.0.1.5"},
		{"RemoteAddr 无端口 → 整串当主机，不能写出空值", "10.0.1.5", "", "10.0.1.5"},
		{"RemoteAddr 是 IPv6 方括号形式 → 剥掉方括号与端口", "[::1]:5000", "", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/order/1", nil)
			req.RemoteAddr = tc.remoteAddr
			switch tc.ctxIP {
			case "":
				// 什么都不挂：走「直接调用本包」那一支
			case "(empty)":
				req = req.WithContext(clientip.WithClient(req.Context(), ""))
			default:
				req = req.WithContext(clientip.WithClient(req.Context(), tc.ctxIP))
			}

			w := httptest.NewRecorder()
			p.ServeHTTP(w, req)
			res := w.Result()
			defer res.Body.Close()

			if got := echoOf(res, "X-Real-IP"); got != tc.want {
				t.Errorf("上游看到的 X-Real-IP = %q，期望 %q", got, tc.want)
			}
		})
	}
}
