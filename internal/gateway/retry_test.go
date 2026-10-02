package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gwlab/internal/config"
)

// deadAddr 返回一个**必然连不上**的地址：监听后立刻关闭，端口随即空出来。
// 比"随便写个高端口"可靠 —— 后者在极小概率下真被别人占着。
func deadAddr(t *testing.T) string {
	t.Helper()
	ln := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := ln.Listener.Addr().String()
	ln.Close()
	return addr
}

// retryConfig 造一条「GET 路由 + 两个节点（坏的在前）」的配置。
// round_robin 从第 0 个开始轮转，所以第一次 Pick 必定选中坏节点 —— 重试与否的差别是确定的。
func retryConfig(dead, good string, rt *config.RetryPolicy) *config.Config {
	return &config.Config{
		ListenAddr: "127.0.0.1:0",
		Routes: []config.Route{{
			Name: "r", Host: "*", Path: "/r", PathType: config.PathExact,
			Upstream: "svc", Retry: rt,
		}},
		Services: map[string]*config.Service{
			"svc": {
				Name: "svc", Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: dead}, {Addr: good}},
				Timeout:   2 * time.Second,
			},
		},
	}
}

// TestRetryFailsOverToHealthyNode 是 C3 的核心：第一次尝试打到连不上的节点、
// 还没写出任何字节，重试换到健康节点后成功。
// 同一个配置去掉 Retry 就应当返回 502 —— 这两条对照才能证明"是重试起的作用"。
func TestRetryFailsOverToHealthyNode(t *testing.T) {
	good := echoServer("from-good", 200)
	defer good.Close()
	dead := deadAddr(t)

	t.Run("配了 retry 时换节点成功", func(t *testing.T) {
		gw, err := New(retryConfig(dead, good.Listener.Addr().String(), &config.RetryPolicy{Attempts: 1}))
		if err != nil {
			t.Fatal(err)
		}
		defer gw.Close()
		code, body := doGet(t, gw, "/r")
		if code != 200 || !strings.Contains(body, "from-good") {
			t.Fatalf("重试应当换到健康节点并成功，实际 code=%d body=%s", code, body)
		}
	})

	t.Run("没配 retry 时就是 502", func(t *testing.T) {
		gw, err := New(retryConfig(dead, good.Listener.Addr().String(), nil))
		if err != nil {
			t.Fatal(err)
		}
		defer gw.Close()
		code, body := doGet(t, gw, "/r")
		if code != http.StatusBadGateway {
			t.Fatalf("不配重试时应当直接 502，实际 code=%d body=%s", code, body)
		}
		if !strings.Contains(body, "upstream") {
			t.Errorf("错误响应里应说明是上游问题，实际 %s", body)
		}
	})
}

// TestRetryOnlyForIdempotent 验证"默认只重试幂等方法"：POST 即使配了 retry 也不换节点。
func TestRetryOnlyForIdempotent(t *testing.T) {
	good := echoServer("from-good", 200)
	defer good.Close()

	cfg := retryConfig(deadAddr(t), good.Listener.Addr().String(), &config.RetryPolicy{Attempts: 1})
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/r", nil) // 无请求体，但方法非幂等
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST 默认不应重试（可能重复下单），期望 502，实际 %d", rec.Code)
	}

	// 显式放开后就会重试
	cfg2 := retryConfig(deadAddr(t), good.Listener.Addr().String(),
		&config.RetryPolicy{Attempts: 1, AllowNonIdempotent: true})
	gw2, err := New(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	defer gw2.Close()
	rec2 := httptest.NewRecorder()
	gw2.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/r", nil))
	if rec2.Code != 200 {
		t.Fatalf("放开 allow_non_idempotent 后应当重试成功，实际 %d", rec2.Code)
	}
}

// TestRetrySkippedWhenBodyPresent：带请求体的请求不重试（重放需要缓冲请求体，本实现刻意不做）。
func TestRetrySkippedWhenBodyPresent(t *testing.T) {
	good := echoServer("from-good", 200)
	defer good.Close()

	cfg := retryConfig(deadAddr(t), good.Listener.Addr().String(), &config.RetryPolicy{Attempts: 1})
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	body := strings.NewReader(`{"k":"v"}`)
	req := httptest.NewRequest(http.MethodPost, "/r", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("带请求体时不重试，期望 502，实际 %d", rec.Code)
	}
}

// TestPerRouteTimeout：路由级 Timeout 覆盖服务级，且真的在到点后放弃（504）。
func TestPerRouteTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("slow-done"))
	}))
	defer slow.Close()

	cfg := retryConfig(deadAddr(t), slow.Listener.Addr().String(), nil)
	// round_robin 第一次会选中"坏节点"，这里把健康节点放到前面，专测超时
	cfg.Services["svc"].Upstreams = []config.Upstream{{Addr: slow.Listener.Addr().String()}}
	cfg.Routes[0].Timeout = config.Duration(80 * time.Millisecond)
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	start := time.Now()
	code, body := doGet(t, gw, "/r")
	elapsed := time.Since(start)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("路由超时应当回 504，实际 code=%d body=%s", code, body)
	}
	if elapsed >= 400*time.Millisecond {
		t.Errorf("路由级超时没生效（等了 %v，上游要睡 400ms）", elapsed)
	}
}

// TestProxyErrorLandsInAccessLog：盯住一个老缺口 ——
// 代理链路的上游错误以前进不了访问日志（ErrorHandler 改的是 Clone 出去的 Header）。
// 现在用 context 传递，日志里必须能看到 err=。
func TestProxyErrorLandsInAccessLog(t *testing.T) {
	cfg := retryConfig(deadAddr(t), deadAddr(t), nil)
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	doGet(t, gw, "/r")

	lines := gw.alog.Tail(1)
	if len(lines) == 0 {
		t.Fatal("访问日志为空")
	}
	if !strings.Contains(lines[0], "err=") || strings.Contains(lines[0], "err=-") {
		t.Fatalf("代理链路的上游错误应当落进访问日志的 err=，实际: %s", lines[0])
	}
}
