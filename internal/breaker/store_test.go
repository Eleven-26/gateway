package breaker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gwlab/internal/config"
)

// TestForceOpenUntilAdoptsAndExpires：采纳远端状态后应当立即快速失败，
// 到期后自动进入半开（复用本地那套恢复流程，不需要第二套状态机）。
func TestForceOpenUntilAdoptsAndExpires(t *testing.T) {
	b := New(config.BreakerConfig{WindowSize: 4, FailRatio: 0.5, MinRequests: 2, OpenFor: 50 * time.Millisecond})

	b.ForceOpenUntil(time.Now().Add(80 * time.Millisecond))
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("采纳远端状态后应当快速失败，实际 %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("远端 OpenFor 到期后应当放行一个探测（半开），实际 %v", err)
	}
}

// TestForceOpenUntilNeverShortens：本地已经打开到更晚时，采纳远端不得把冷却缩短
// （否则"某个副本冷却短"会变成所有副本的冷却都被拉短，熔断形同虚设）。
func TestForceOpenUntilNeverShortens(t *testing.T) {
	b := New(config.BreakerConfig{WindowSize: 4, FailRatio: 0.5, MinRequests: 2, OpenFor: time.Second})
	_ = b.Allow()
	b.Report(false)
	_ = b.Allow()
	b.Report(false) // 本地打开到 now+1s

	local := b.OpenUntil()
	b.ForceOpenUntil(time.Now().Add(10 * time.Millisecond)) // 远端说只到 10ms 后

	if got := b.OpenUntil(); got.Before(local) {
		t.Fatalf("采纳远端不得缩短本地冷却：原 %v，变成 %v", local, got)
	}
}

// TestOnTripFiresOnLocalTripOnly 是一个**防风暴**用例：
// 本地跳闸要发布（OnTrip），而采纳远端状态**不能**再回调 ——
// 否则两个副本会互相转发同一件事（A 发布 → B 采纳 → B 又发布 → A 采纳 → …）。
func TestOnTripFiresOnLocalTripOnly(t *testing.T) {
	var trips int64
	var lastUntil atomic.Value
	b := New(config.BreakerConfig{WindowSize: 4, FailRatio: 0.5, MinRequests: 2, OpenFor: time.Second})
	b.OnTrip = func(until time.Time) {
		atomic.AddInt64(&trips, 1)
		lastUntil.Store(until)
	}

	_ = b.Allow()
	b.Report(false)
	_ = b.Allow()
	b.Report(false) // 第 2 次失败 → 打开 → 应当回调一次

	if got := atomic.LoadInt64(&trips); got != 1 {
		t.Fatalf("本地跳闸应当回调 1 次，实际 %d", got)
	}
	if until, _ := lastUntil.Load().(time.Time); !until.After(time.Now()) {
		t.Fatalf("回调参数应当是未来的时刻，实际 %v", until)
	}

	// 采纳远端状态：不能再回调
	b.ForceOpenUntil(time.Now().Add(2 * time.Second))
	if got := atomic.LoadInt64(&trips); got != 1 {
		t.Fatalf("采纳远端状态不应触发 OnTrip（否则会形成发布风暴），实际回调 %d 次", got)
	}
}

// TestOpenUntilZeroWhenClosed：未打开时 OpenUntil 返回零值（gateway 靠它决定要不要发布）。
func TestOpenUntilZeroWhenClosed(t *testing.T) {
	b := New(config.BreakerConfig{})
	if !b.OpenUntil().IsZero() {
		t.Fatal("closed 状态应当返回零值")
	}
	_ = b.Allow()
	b.Report(false) // 零值配置：一次失败即打开
	if b.OpenUntil().IsZero() {
		t.Fatal("打开后应当返回未来的时刻")
	}
}

// storeServer 起一个最小的共享熔断状态服务（协议与 cmd/statestore 的 /breaker 一致）。
func storeServer(t *testing.T) (*httptest.Server, map[string]int64) {
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
	return srv, openUntil
}

// TestHTTPStoreRoundTrip：发布与拉取能对上（含毫秒时间戳的往返）。
func TestHTTPStoreRoundTrip(t *testing.T) {
	srv, _ := storeServer(t)
	st := NewHTTPStore(srv.URL, time.Second)

	until := time.Now().Add(3 * time.Second).Truncate(time.Millisecond)
	if err := st.Publish("order-svc", until); err != nil {
		t.Fatalf("Publish 失败: %v", err)
	}
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	got, ok := snap["order-svc"]
	if !ok {
		t.Fatal("快照里应当有 order-svc")
	}
	if !got.Equal(until) {
		t.Fatalf("时间戳往返不一致：%v != %v", got, until)
	}
}

// TestHTTPStoreErrorsAreReported：状态服务出错时要报错并回调 OnError（用于打点），
// 但**不能**影响本地熔断 —— 调用方会忽略这个错误、继续用本地状态机（降级为单副本语义）。
func TestHTTPStoreErrorsAreReported(t *testing.T) {
	srv, _ := storeServer(t)
	url := srv.URL
	srv.Close()

	var errs int64
	st := NewHTTPStore(url, 200*time.Millisecond)
	st.OnError(func(error) { atomic.AddInt64(&errs, 1) })

	if err := st.Publish("x", time.Now().Add(time.Second)); err == nil {
		t.Fatal("Publish 到已下线的服务应当报错")
	}
	if _, err := st.Snapshot(); err == nil {
		t.Fatal("Snapshot 到已下线的服务应当报错")
	}
	if atomic.LoadInt64(&errs) == 0 {
		t.Fatal("失败必须回调 OnError（否则指标上看不出共享状态已经不可用）")
	}
}
