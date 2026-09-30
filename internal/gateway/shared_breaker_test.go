package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gwlab/internal/config"
)

// breakerStore 起一个共享熔断状态服务（协议与 cmd/statestore 的 /breaker 一致）。
func breakerStore(t *testing.T) *httptest.Server {
	t.Helper()
	var (
		mu        sync.Mutex
		openUntil = map[string]int64{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/breaker", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				Service string `json:"service"`
				UntilMs int64  `json:"until_ms"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Service == "" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			mu.Lock()
			openUntil[req.Service] = req.UntilMs
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		mu.Lock()
		out := make(map[string]int64, len(openUntil))
		for k, v := range openUntil {
			out[k] = v
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"services": out})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sharedBreakerConfig 造一条打向"恒失败上游"的路由，熔断阈值很低、冷却很长，
// 并把共享状态同步间隔调到 50ms（测试里不想等默认的 1s）。
func sharedBreakerConfig(upstream, storeURL string) *config.Config {
	return &config.Config{
		ListenAddr: "127.0.0.1:0",
		SharedState: config.SharedStateConfig{
			URL:          storeURL,
			Timeout:      config.Duration(time.Second),
			SyncInterval: config.Duration(50 * time.Millisecond),
		},
		Routes: []config.Route{{
			Name: "r", Host: "*", Path: "/r", PathType: config.PathExact, Upstream: "svc",
		}},
		Services: map[string]*config.Service{
			"svc": {
				Name: "svc", Balance: "round_robin",
				Upstreams: []config.Upstream{{Addr: upstream}},
				Timeout:   2 * time.Second,
				Breaker: config.BreakerConfig{
					WindowSize: 10, FailRatio: 0.5, MinRequests: 2,
					OpenFor: 10 * time.Second, HalfOpenMax: 1,
				},
			},
		},
	}
}

// TestSharedBreakerStateAcrossReplicas 是 C4b 的核心验收：
// 副本 A 把熔断器打到打开，副本 B **自己一次失败都没见过**，但它应当快速失败（503 circuit_open），
// 而不是继续把流量打到那个坏服务上（单副本语义下 B 会返回 500）。
func TestSharedBreakerStateAcrossReplicas(t *testing.T) {
	bad := echoServer("boom", http.StatusInternalServerError)
	defer bad.Close()
	store := breakerStore(t)
	addr := bad.Listener.Addr().String()

	replicaA, err := New(sharedBreakerConfig(addr, store.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaA.Close()
	replicaB, err := New(sharedBreakerConfig(addr, store.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaB.Close()

	// 副本 B 先证明自己是"健康的旁观者"：它没打过任何请求之前，B 的熔断器是 closed
	//（用同一个坏上游打一次会得到 500 —— 说明 B 确实没被本地经验影响）
	if code, _ := doGet(t, replicaB, "/r"); code != http.StatusInternalServerError {
		t.Fatalf("副本 B 首次请求应当是上游的 500，实际 %d", code)
	}

	// 副本 A 打到熔断打开（MinRequests=2）
	for i := 0; i < 6; i++ {
		if code, body := doGet(t, replicaA, "/r"); code == 503 && strings.Contains(body, "circuit_open") {
			break
		}
	}
	if code, body := doGet(t, replicaA, "/r"); code != 503 || !strings.Contains(body, "circuit_open") {
		t.Fatalf("副本 A 应当已经熔断，实际 %d %s", code, body)
	}

	// 等 A 发布 + B 拉取（同步间隔 50ms，留足余量）
	time.Sleep(400 * time.Millisecond)

	code, body := doGet(t, replicaB, "/r")
	if code != 503 || !strings.Contains(body, "circuit_open") {
		t.Fatalf("副本 B 应当采纳到共享的熔断状态并快速失败（503 circuit_open），实际 %d %s", code, body)
	}
}

// TestSharedBreakerDisabledKeepsLocalSemantics：不配 SharedState 时行为与单副本一致
// （B 永远不会因为 A 熔断而快速失败 —— 这正是共享要解决的问题）。
func TestSharedBreakerDisabledKeepsLocalSemantics(t *testing.T) {
	bad := echoServer("boom", http.StatusInternalServerError)
	defer bad.Close()
	addr := bad.Listener.Addr().String()

	replicaA, err := New(sharedBreakerConfig(addr, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaA.Close()
	replicaB, err := New(sharedBreakerConfig(addr, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaB.Close()

	for i := 0; i < 6; i++ {
		doGet(t, replicaA, "/r")
	}
	if code, _ := doGet(t, replicaB, "/r"); code != http.StatusInternalServerError {
		t.Fatalf("没有共享时 B 不受 A 影响，应当仍是上游的 500，实际 %d", code)
	}
}
