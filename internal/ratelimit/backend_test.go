package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gwlab/internal/config"
)

// storeServer 起一个最小的共享状态服务（协议与 cmd/statestore 一致），
// 直接复用 LocalBackend —— 这样测试校验的是"网关侧客户端"，不是又写一遍桶逻辑。
func storeServer(t *testing.T) (*httptest.Server, *LocalBackend) {
	t.Helper()
	backend := NewLocalBackend()
	mux := http.NewServeMux()
	mux.HandleFunc("/ratelimit", func(w http.ResponseWriter, r *http.Request) {
		var req limitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		allow, retry := backend.Allow(req.Key, config.LimitPolicy{RatePerSec: req.RatePerSec, Burst: req.Burst})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(limitResponse{Allow: allow, RetryAfterMs: int(retry.Milliseconds())})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, backend
}

// TestHTTPBackendSharesOneBucket：共享后端的关键性质 ——
// **两个"副本"（两个 Backend 实例）看到同一个桶**，而不是各算各的。
// 这正是不共享时会失效的地方（见 TestLocalBackendsDoNotShare）。
func TestHTTPBackendSharesOneBucket(t *testing.T) {
	srv, _ := storeServer(t)
	policy := config.LimitPolicy{RatePerSec: 0.01, Burst: 2} // 几乎不补充，便于判定

	replicaA := NewHTTPBackend(srv.URL+"/ratelimit", time.Second, true)
	replicaB := NewHTTPBackend(srv.URL+"/ratelimit", time.Second, true)

	results := []bool{}
	for _, b := range []*HTTPBackend{replicaA, replicaB, replicaA, replicaB} {
		allow, _ := b.Allow("route:x|user:u", policy)
		results = append(results, allow)
	}
	// 桶容量 2：前两个放行（一个来自 A、一个来自 B），后两个必须被拒
	want := []bool{true, true, false, false}
	for i := range want {
		if results[i] != want[i] {
			t.Fatalf("共享桶的放行序列应为 %v，实际 %v（第 %d 次）", want, results, i+1)
		}
	}
}

// TestLocalBackendsDoNotShare 是对照组：本地后端在多副本下**不共享**，
// 每个副本各自放行 burst 个 —— 这就是"扩容后限流阈值变成 副本数×阈值"的实证。
func TestLocalBackendsDoNotShare(t *testing.T) {
	policy := config.LimitPolicy{RatePerSec: 0.01, Burst: 2}
	replicaA := NewLocalBackend()
	replicaB := NewLocalBackend()

	allowed := 0
	for _, b := range []*LocalBackend{replicaA, replicaB, replicaA, replicaB} {
		if ok, _ := b.Allow("route:x|user:u", policy); ok {
			allowed++
		}
	}
	if allowed != 4 {
		t.Fatalf("本地后端两个副本应当各放行 2 个（共 4），实际 %d —— "+
			"说明测试没体现出'副本各算各的'这一事实", allowed)
	}
}

// TestHTTPBackendReturnsRetryAfter：被拒时要带回 Retry-After 需要的等待时长。
func TestHTTPBackendReturnsRetryAfter(t *testing.T) {
	srv, _ := storeServer(t)
	b := NewHTTPBackend(srv.URL+"/ratelimit", time.Second, true)
	policy := config.LimitPolicy{RatePerSec: 1, Burst: 1} // 每秒补 1 个

	if ok, _ := b.Allow("k", policy); !ok {
		t.Fatal("第一个应当放行")
	}
	ok, retry := b.Allow("k", policy)
	if ok {
		t.Fatal("第二个应当被拒")
	}
	if retry <= 0 || retry > 2*time.Second {
		t.Fatalf("retry-after 应当在 (0,2s] 内，实际 %v", retry)
	}
}

// TestHTTPBackendFailOpenAndCounted：状态服务挂掉时按 fail-open 放行，
// **并且必须回调 OnError**（静默 fail-open 等于把限流失效藏起来）。
func TestHTTPBackendFailOpenAndCounted(t *testing.T) {
	srv, _ := storeServer(t)
	url := srv.URL + "/ratelimit"
	srv.Close() // 状态服务直接下线

	var errs int64
	b := NewHTTPBackend(url, 200*time.Millisecond, true)
	b.OnError = func(error) { atomic.AddInt64(&errs, 1) }

	allow, _ := b.Allow("k", config.LimitPolicy{RatePerSec: 1, Burst: 1})
	if !allow {
		t.Fatal("fail-open 时应当放行")
	}
	if atomic.LoadInt64(&errs) == 0 {
		t.Fatal("判定失败必须回调 OnError（否则指标上看不出限流已经失效）")
	}
}

// TestHTTPBackendFailClosed：显式选择 fail-closed 时，状态服务不可用必须拒绝。
func TestHTTPBackendFailClosed(t *testing.T) {
	srv, _ := storeServer(t)
	url := srv.URL + "/ratelimit"
	srv.Close()

	b := NewHTTPBackend(url, 200*time.Millisecond, false)
	if allow, _ := b.Allow("k", config.LimitPolicy{RatePerSec: 1, Burst: 1}); allow {
		t.Fatal("fail-closed 时应当拒绝")
	}
}

// TestHTTPBackendHandlesGarbage：状态服务返回垃圾/错误码时不能 panic，按失败策略处理。
func TestHTTPBackendHandlesGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	b := NewHTTPBackend(srv.URL, 200*time.Millisecond, true)
	if allow, _ := b.Allow("k", config.LimitPolicy{RatePerSec: 1, Burst: 1}); !allow {
		t.Fatal("非 2xx 应当按 fail-open 放行")
	}
	if b.Name() == "" {
		t.Fatal("Name 不能为空（要进日志与指标）")
	}
}
