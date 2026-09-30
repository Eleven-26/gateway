package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gwlab/internal/config"

	"gwlab/internal/observability"
)

// TestTraceparentPropagatedWithNewSpan 是 C5 的核心验收：
// 入站的 W3C traceparent 要**沿用 trace-id**，但网关这一跳必须换成**新的 span-id**
// （把客户端 span-id 原样透传会让下游以为"自己就是客户端那一跳"）。
// 同时验证 tracestate 原样透传、旧的 X-Trace-Id 仍然发出去（兼容老后端）。
func TestTraceparentPropagatedWithNewSpan(t *testing.T) {
	const (
		inTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		inSpan  = "00f067aa0ba902b7"
	)
	var (
		mu  sync.Mutex
		got http.Header
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()

	addr := up.Listener.Addr().String()
	gw, err := New(singleUpstreamConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	req := httptest.NewRequest(http.MethodGet, "/r", nil)
	req.Header.Set("traceparent", "00-"+inTrace+"-"+inSpan+"-01")
	req.Header.Set("tracestate", "vendor=abc")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("上游没有收到请求")
	}
	tp := got.Get("traceparent")
	tid, sid, flags, ok := observability.ParseTraceparent(tp)
	if !ok {
		t.Fatalf("上游收到的 traceparent 不合法: %q", tp)
	}
	if tid != inTrace {
		t.Errorf("应当沿用上游 trace-id %s，实际 %s", inTrace, tid)
	}
	if sid == inSpan {
		t.Error("必须为本跳生成新的 span-id（不能复用客户端的）")
	}
	if flags != 1 {
		t.Errorf("采样标志应当透传，实际 %d", flags)
	}
	if got.Get("tracestate") != "vendor=abc" {
		t.Errorf("tracestate 应当原样透传，实际 %q", got.Get("tracestate"))
	}
	if got.Get(observability.TraceHeader) != inTrace {
		t.Errorf("X-Trace-Id 应当保持兼容发出，实际 %q", got.Get(observability.TraceHeader))
	}

	// 访问日志里也要能看到本跳的 span
	lines := gw.alog.Tail(1)
	if len(lines) == 0 || !strings.Contains(lines[0], "trace="+inTrace) || !strings.Contains(lines[0], "span="+sid) {
		t.Fatalf("访问日志应当带上 trace 与 span，实际: %v", lines)
	}
}

// TestInvalidTraceparentStartsFreshTrace：非法 traceparent 必须"当作没传"——
// 自己新起一条链路，而不是把非法值透传给下游。
func TestInvalidTraceparentStartsFreshTrace(t *testing.T) {
	var (
		mu  sync.Mutex
		got http.Header
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()

	addr := up.Listener.Addr().String()
	gw, err := New(singleUpstreamConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	req := httptest.NewRequest(http.MethodGet, "/r", nil)
	req.Header.Set("traceparent", "00-"+strings.Repeat("0", 32)+"-00f067aa0ba902b7-01") // trace-id 全 0：非法
	gw.ServeHTTP(httptest.NewRecorder(), req)

	mu.Lock()
	defer mu.Unlock()
	tid, sid, _, ok := observability.ParseTraceparent(got.Get("traceparent"))
	if !ok {
		t.Fatalf("下游应当收到合法的 traceparent，实际 %q", got.Get("traceparent"))
	}
	if strings.Repeat("0", 32) == tid || sid == "00f067aa0ba902b7" {
		t.Fatalf("非法入站值不该被沿用：tid=%s sid=%s", tid, sid)
	}
}

// singleUpstreamConfig 造一条"单节点"配置（校验不允许同一服务里出现重复节点）。
func singleUpstreamConfig(addr string) *config.Config {
	return &config.Config{
		ListenAddr: "127.0.0.1:0",
		Routes: []config.Route{{
			Name: "r", Host: "*", Path: "/r", PathType: config.PathExact, Upstream: "svc",
		}},
		Services: map[string]*config.Service{
			"svc": {Name: "svc", Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: addr}}, Timeout: 2 * time.Second},
		},
	}
}
