package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gwlab/internal/config"
	"gwlab/internal/ratelimit"
)

// limitStore 起一个共享限流状态服务（协议与 cmd/statestore 一致）。
func limitStore(t *testing.T) *httptest.Server {
	t.Helper()
	backend := ratelimit.NewLocalBackend()
	mux := http.NewServeMux()
	mux.HandleFunc("/ratelimit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key        string  `json:"key"`
			RatePerSec float64 `json:"rate_per_sec"`
			Burst      int     `json:"burst"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		allow, retry := backend.Allow(req.Key, config.LimitPolicy{RatePerSec: req.RatePerSec, Burst: req.Burst})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": allow, "retry_after_ms": int(retry.Milliseconds())})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sharedLimitConfig 造一条带限流的路由：GET /limited，每 key 每秒 0.01 个令牌、桶容量 2
// （几乎不补充，所以测试期间判定是确定的）。
func sharedLimitConfig(upstream, rateLimitURL string) *config.Config {
	cfg := &config.Config{
		ListenAddr: "127.0.0.1:0",
		Services: map[string]*config.Service{
			"svc": {Name: "svc", Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: upstream}}, Timeout: 2 * time.Second},
		},
	}
	cfg.Routes = []config.Route{{
		Name: "limited", Host: "*", Path: "/limited", PathType: config.PathExact,
		Upstream: "svc", Limit: config.LimitPolicy{RatePerSec: 0.01, Burst: 2},
	}}
	if rateLimitURL != "" {
		cfg.SharedState = config.SharedStateConfig{URL: rateLimitURL, Timeout: config.Duration(time.Second)}
	}
	return cfg
}

// TestSharedRateLimitAcrossReplicas 是 C4 的核心验收：
// 两个网关进程共用同一个状态服务时，桶也是共用的 —— 第 3 个请求必须被拒，
// 无论它落在哪个副本上。对照组（不配共享）则两个副本各放行 2 个。
func TestSharedRateLimitAcrossReplicas(t *testing.T) {
	up := echoServer("ok", 200)
	defer up.Close()
	srv := limitStore(t)

	replicaA, err := New(sharedLimitConfig(up.Listener.Addr().String(), srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaA.Close()
	replicaB, err := New(sharedLimitConfig(up.Listener.Addr().String(), srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaB.Close()

	codes := []int{}
	for _, gw := range []*Gateway{replicaA, replicaB, replicaA, replicaB} {
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/limited", nil))
		codes = append(codes, rec.Code)
	}
	want := []int{200, 200, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("共享限流的放行序列应为 %v，实际 %v", want, codes)
		}
	}
}

// TestLocalRateLimitPerReplica 是对照组：不配共享时，两个副本各有一份桶 ——
// 4 个请求全部放行（这就是"扩容后阈值变成 副本数×阈值"的实证）。
func TestLocalRateLimitPerReplica(t *testing.T) {
	up := echoServer("ok", 200)
	defer up.Close()

	replicaA, err := New(sharedLimitConfig(up.Listener.Addr().String(), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaA.Close()
	replicaB, err := New(sharedLimitConfig(up.Listener.Addr().String(), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaB.Close()

	allowed := 0
	for _, gw := range []*Gateway{replicaA, replicaB, replicaA, replicaB} {
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/limited", nil))
		if rec.Code == 200 {
			allowed++
		}
	}
	if allowed != 4 {
		t.Fatalf("本地限流两个副本应当各放行 2 个（共 4），实际 %d", allowed)
	}
}

// TestSharedStateFailureIsCounted：状态服务挂掉时按 fail-open 放行，
// 但必须打点（gw_shared_state_errors_total）—— 静默放行等于把限流失效藏起来。
func TestSharedStateFailureIsCounted(t *testing.T) {
	up := echoServer("ok", 200)
	defer up.Close()
	srv := limitStore(t)
	cfg := sharedLimitConfig(up.Listener.Addr().String(), srv.URL)
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	srv.Close() // 状态服务下线

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/limited", nil))
	if rec.Code != 200 {
		t.Fatalf("fail-open 应当放行，实际 %d", rec.Code)
	}
	metrics := gw.metrics.Render()
	if !strings.Contains(metrics, "gw_shared_state_errors_total") {
		t.Fatalf("共享状态判定失败应当出现在指标里，实际 metrics:\n%s", metrics)
	}
}
