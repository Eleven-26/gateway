package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gwlab/internal/config"
)

// reloadConfig 造一份「一条路由 → 一个服务 → 一个上游」的配置，便于观察换上游/换路由。
func reloadConfig(routeName, serviceName, addr string, openFor time.Duration) *config.Config {
	return &config.Config{
		ListenAddr: "127.0.0.1:0",
		JWTSecret:  "test-secret",
		APIKey:     "test-key",
		Routes: []config.Route{
			{Name: routeName, Host: "*", Path: "/" + routeName, PathType: config.PathExact, Upstream: serviceName},
		},
		Services: map[string]*config.Service{
			serviceName: {
				Name: serviceName, Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: addr}},
				Timeout:   5 * time.Second,
				Breaker:   config.BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4, OpenFor: openFor, HalfOpenMax: 1},
			},
		},
	}
}

func echoServer(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func doGet(t *testing.T, gw *Gateway, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// TestReloadSwapsRoutesAndPrunes：新配置原子生效（新路由通、老路由 404），
// 且被删掉的服务与它的均衡器/代理缓存都要清掉。
func TestReloadSwapsRoutesAndPrunes(t *testing.T) {
	upA := echoServer("from-A", 200)
	defer upA.Close()
	upB := echoServer("from-B", 200)
	defer upB.Close()

	gw, err := New(reloadConfig("routeA", "svc-a", upA.Listener.Addr().String(), time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	if code, body := doGet(t, gw, "/routeA"); code != 200 || !strings.Contains(body, "from-A") {
		t.Fatalf("重载前 routeA 应当通：code=%d body=%s", code, body)
	}

	sum, err := gw.Reload(reloadConfig("routeB", "svc-b", upB.Listener.Addr().String(), time.Minute))
	if err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}

	if code, body := doGet(t, gw, "/routeB"); code != 200 || !strings.Contains(body, "from-B") {
		t.Errorf("重载后 routeB 应当通：code=%d body=%s", code, body)
	}
	if code, _ := doGet(t, gw, "/routeA"); code != 404 {
		t.Errorf("重载后 routeA 应当 404，实际 %d", code)
	}
	if len(sum.Removed) != 1 || sum.Removed[0] != "svc-a" {
		t.Errorf("应当报告删除了 svc-a，实际 %v", sum.Removed)
	}

	gw.mu.Lock()
	_, hasOldBreaker := gw.breakers["svc-a"]
	_, hasOldBalancer := gw.balancers["svc-a"]
	staleProxies := 0
	for k := range gw.proxies {
		if strings.HasPrefix(k, "svc-a|") {
			staleProxies++
		}
	}
	gw.mu.Unlock()
	if hasOldBreaker || hasOldBalancer {
		t.Errorf("已删除服务的熔断器/均衡器应当被清理（breaker=%v balancer=%v）", hasOldBreaker, hasOldBalancer)
	}
	if staleProxies != 0 {
		t.Errorf("旧服务的代理缓存应当被清理，实际还剩 %d 条", staleProxies)
	}
	if sum.ProxiesPruned == 0 {
		t.Errorf("summary 应报告代理清理条数，实际 %+v", sum)
	}
}

// TestReloadKeepsBreakerState 是热重载里最关键的一条（AGENTS.md §4 坑①）：
// 熔断器状态**不能重建**——重建会把刚打开的熔断器重置回 closed，流量立刻又打到没恢复的上游。
func TestReloadKeepsBreakerState(t *testing.T) {
	up := echoServer(`{"error":"boom"}`, 500) // 恒失败的上游
	defer up.Close()

	cfg := reloadConfig("flaky", "svc-flaky", up.Listener.Addr().String(), time.Minute)
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 打到熔断打开（MinRequests=4，FailRatio=0.5 → 4 次失败必然打开）
	opened := false
	for i := 0; i < 8; i++ {
		if code, body := doGet(t, gw, "/flaky"); code == 503 && strings.Contains(body, "circuit_open") {
			opened = true
			break
		}
	}
	if !opened {
		t.Fatal("连续失败后熔断器应当打开")
	}

	// 拓扑完全没变的重载：熔断器与均衡器都应复用
	sum, err := gw.Reload(cfg)
	if err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if len(sum.BreakerKept) != 1 || sum.BreakerKept[0] != "svc-flaky" {
		t.Errorf("熔断器应当被复用，实际 %+v", sum)
	}
	if len(sum.BalancerKept) != 1 || sum.BalancerKept[0] != "svc-flaky" {
		t.Errorf("拓扑未变时均衡器应当复用，实际 %+v", sum)
	}

	// 关键断言：重载之后仍然处于熔断快速失败，而不是又去打那个坏上游
	code, body := doGet(t, gw, "/flaky")
	if code != 503 || !strings.Contains(body, "circuit_open") {
		t.Fatalf("重载后熔断状态被重置了（这是 §4 坑①）：code=%d body=%s", code, body)
	}
}

// TestReloadFailureKeepsOldConfig：新配置非法时，当前配置必须原样可用（不能半新半旧）。
func TestReloadFailureKeepsOldConfig(t *testing.T) {
	up := echoServer("from-A", 200)
	defer up.Close()

	gw, err := New(reloadConfig("routeA", "svc-a", up.Listener.Addr().String(), time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	bad := reloadConfig("routeA", "svc-a", up.Listener.Addr().String(), time.Minute)
	bad.Routes[0].Upstream = "not-exist" // 校验必须拦住
	if _, err := gw.Reload(bad); err == nil {
		t.Fatal("非法配置应当被 Reload 拒绝")
	}
	if code, body := doGet(t, gw, "/routeA"); code != 200 || !strings.Contains(body, "from-A") {
		t.Fatalf("重载失败后旧配置必须继续可用：code=%d body=%s", code, body)
	}
}

// TestReloadRebuildsBalancerOnTopologyChange：节点列表变了要重建均衡器（并如实报告）。
func TestReloadRebuildsBalancerOnTopologyChange(t *testing.T) {
	upA := echoServer("from-A", 200)
	defer upA.Close()
	upB := echoServer("from-B", 200)
	defer upB.Close()

	cfg := reloadConfig("routeA", "svc-a", upA.Listener.Addr().String(), time.Minute)
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 同一个服务加一个节点 → 拓扑变了
	cfg2 := reloadConfig("routeA", "svc-a", upA.Listener.Addr().String(), time.Minute)
	cfg2.Services["svc-a"].Upstreams = append(cfg2.Services["svc-a"].Upstreams,
		config.Upstream{Addr: upB.Listener.Addr().String()})

	sum, err := gw.Reload(cfg2)
	if err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if len(sum.BalancerRebuilt) != 1 || sum.BalancerRebuilt[0] != "svc-a" {
		t.Errorf("节点列表变化时应当重建均衡器，实际 %+v", sum)
	}

	// 重建后两个节点都能被选到（round_robin 两个节点，多打几次必然见到 B）
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		_, body := doGet(t, gw, "/routeA")
		seen[body] = true
	}
	if !seen["from-A"] || !seen["from-B"] {
		t.Errorf("新节点应当参与轮询，实际只见到 %v", seen)
	}
}
