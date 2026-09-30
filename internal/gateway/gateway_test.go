package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gwlab/internal/config"
)

// newTestGateway 造一个「/healthz 自答 + 其余路径反代到 httptest 上游」的网关。
func newTestGateway(t *testing.T, h http.HandlerFunc) *Gateway {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)

	cfg := &config.Config{
		ListenAddr: "127.0.0.1:0",
		JWTSecret:  "test-secret",
		APIKey:     "test-key",
		Routes: []config.Route{
			{Name: "self", Host: "*", Path: "/healthz", PathType: config.PathExact, Upstream: "self"},
			{Name: "proxy", Host: "*", Path: "/", PathType: config.PathPrefix, Upstream: "svc"},
		},
		Services: map[string]*config.Service{
			"self": {Name: "self", Balance: "round_robin", Timeout: time.Second},
			"svc": {
				Name: "svc", Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: up.Listener.Addr().String()}},
				Timeout:   5 * time.Second,
				Breaker:   config.BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4, OpenFor: time.Second, HalfOpenMax: 1},
			},
		},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Close)
	return gw
}

// TestWebSocketUpgrade 协议升级必须成功 —— statusWriter 靠 Unwrap 把底层连接让给 ReverseProxy。
//
// 修复前实测：客户端拿到 "HTTP/1.1 502 Bad Gateway"（审计 P0-1）。
// 根因是 ReverseProxy 用 http.NewResponseController(rw).Hijack() 拿连接，而 Controller 只认
// Hijacker 或 Unwrap()；包装类两个都没有时就 ErrNotSupported。
func TestWebSocketUpgrade(t *testing.T) {
	gw := newTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("上游 ResponseWriter 不是 Hijacker")
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		line, _ := brw.ReadString('\n') // 升级后客户端发来的数据
		_, _ = brw.WriteString("echo:" + line)
		_ = brw.Flush()
	})

	front := httptest.NewServer(gw)
	defer front.Close()

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读响应行失败: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("协议升级失败：%q（期望 101 Switching Protocols）", strings.TrimSpace(statusLine))
	}

	// 升级后双向数据仍要通 —— 证明拿到的是同一条连接，而不是 101 之后就断了
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读响应头失败: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if !strings.Contains(echo, "echo:ping") {
		t.Errorf("升级后的双向数据不通：%q", strings.TrimSpace(echo))
	}
}

// panicWriter 让「写响应」这一步 panic，用来模拟 handler 内部崩溃。
type panicWriter struct{ http.ResponseWriter }

func (p panicWriter) Write([]byte) (int, error) { panic("boom: 模拟 handler 内部 panic") }

// TestPanicIsRecoveredAndCounted：panic 不能逃出 ServeHTTP，且必须能在指标里看到。
// 修复前：net/http 会替我们 recover，但客户端只看到连接重置，/metrics 与 /debug/logs 里什么都没有（审计 P0-2）。
func TestPanicIsRecoveredAndCounted(t *testing.T) {
	gw := newTestGateway(t, func(w http.ResponseWriter, r *http.Request) {})

	rec := httptest.NewRecorder()
	// 这里若 panic 逃逸，测试会直接失败 —— 那就是兜底没生效
	gw.ServeHTTP(panicWriter{rec}, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got := gw.metrics.Render(); !strings.Contains(got, "gw_panics_total 1") {
		t.Errorf("panic 未被计数，/metrics 输出：\n%s", got)
	}
}

// TestBodyLimitRejectsOversizedBody：Content-Length 超过上限时，在鉴权之前就以 413 拒绝（审计 P0-3）。
func TestBodyLimitRejectsOversizedBody(t *testing.T) {
	gw := newTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("超限请求不应到达上游")
	})

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("x"))
	req.ContentLength = maxBodyBytes + 1 // 不必真的发 8MB：网关先按 Content-Length 拒绝
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "body_too_large") {
		t.Errorf("响应体应带 body_too_large，实际 %s", rec.Body.String())
	}
}

// TestBodyUnderLimitProxied：没超限的请求照常转发 —— 证明上限没有误伤正常流量。
func TestBodyUnderLimitProxied(t *testing.T) {
	gw := newTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("upstream got " + string(body)))
	})

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("hello")))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream got hello") {
		t.Fatalf("正常转发失败：code=%d body=%s", rec.Code, rec.Body.String())
	}
}
