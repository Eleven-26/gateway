package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
