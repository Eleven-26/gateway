package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gwlab/internal/config"
)

// newRealIPGateway 造一个「网关前面站着可信代理」的网关，上游把收到的 X-Real-IP 回显到响应头。
//
// trusted 就是「直连对端算不算我们自己部署的代理」的判据：填 127.0.0.1 表示
// 测试里手工构造的那个对端（模拟 LB）是可信的。
func newRealIPGateway(t *testing.T, trusted []string) (*Gateway, *string) {
	t.Helper()

	var gotRealIP string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRealIP = r.Header.Get("X-Real-IP")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		ListenAddr:     "127.0.0.1:0",
		JWTSecret:      "test-secret",
		APIKey:         "test-key",
		TrustedProxies: trusted,
		Routes: []config.Route{
			{Name: "proxy", Host: "*", Path: "/", PathType: config.PathPrefix, Upstream: "svc"},
		},
		Services: map[string]*config.Service{
			"svc": {
				Name: "svc", Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: up.Listener.Addr().String()}},
				Timeout:   5 * time.Second,
			},
		},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Close)
	return gw, &gotRealIP
}

// TestRealIPReachesUpstreamThroughContext 端到端盯住「入口解析 → ctx → 出口写头」这条链：
//
//	入站对端是可信代理（LB）+ XFF 里带真实客户端
//	  → 入口 clientip.Resolve 解出 9.9.9.9
//	  → 结果挂到 ctx（clientip.WithClient）
//	  → proxy.Director 写 X-Real-IP 时从 ctx 取
//	  → 上游看到 9.9.9.9，而**不是**直连对端 127.0.0.1
//
// 修复前这里会是 127.0.0.1：入口解出来的真实客户端在出口被写回成代理地址，
// 上游按 IP 做限流 / 审计时看到的还是 LB（问题只换了个位置复发）。
func TestRealIPReachesUpstreamThroughContext(t *testing.T) {
	gw, gotRealIP := newRealIPGateway(t, []string{"127.0.0.1"})

	req := httptest.NewRequest(http.MethodGet, "/api/order/1", nil)
	req.RemoteAddr = "127.0.0.1:5000"            // 直连对端 = LB（可信）
	req.Header.Set("X-Forwarded-For", "9.9.9.9") // LB 写下的真实客户端

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if *gotRealIP != "9.9.9.9" {
		t.Errorf("上游看到的 X-Real-IP = %q，期望 %q；若为 127.0.0.1 说明出口又从 RemoteAddr 推了一遍",
			*gotRealIP, "9.9.9.9")
	}
}

// TestRealIPUntrustedPeerIgnoresXFF 是对照组：对端不可信时，XFF 一个字节都不该采信 ——
// X-Real-IP 只能是那个不可信的对端自己。这条同时证明「走 ctx」没有绕过可信代理判定。
func TestRealIPUntrustedPeerIgnoresXFF(t *testing.T) {
	gw, gotRealIP := newRealIPGateway(t, []string{"10.0.0.0/8"})

	req := httptest.NewRequest(http.MethodGet, "/api/order/1", nil)
	req.RemoteAddr = "203.0.113.9:5000"          // 攻击者直连，不在可信网段
	req.Header.Set("X-Forwarded-For", "9.9.9.9") // 伪造

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if *gotRealIP != "203.0.113.9" {
		t.Errorf("上游看到的 X-Real-IP = %q，期望对端 %q（伪造的 XFF 不该被采信）",
			*gotRealIP, "203.0.113.9")
	}
}
