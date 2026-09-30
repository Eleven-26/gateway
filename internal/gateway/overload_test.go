package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gwlab/internal/config"
)

// gatedUpstream 返回一个"可控放行"的上游：请求会阻塞到 release 被关闭，
// 用来精确制造"并发被占满"的场景（比 sleep 更确定）。
func gatedUpstream(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(up.Close)
	return up, func() { once.Do(func() { close(release) }) }
}

func overloadConfig(addr string, maxInflight, maxQueue int, queueTO, retryAfter time.Duration) *config.Config {
	cfg := singleUpstreamConfig(addr)
	cfg.Overload = config.OverloadConfig{
		MaxInflight:  maxInflight,
		MaxQueue:     maxQueue,
		QueueTimeout: config.Duration(queueTO),
		RetryAfter:   config.Duration(retryAfter),
	}
	return cfg
}

// TestOverloadRejectsWithRetryAfter：并发打满且不允许排队时，多余的请求必须
// **立刻**拿到 503 + Retry-After + X-Load-Shed，而不是挂在那里等上游。
func TestOverloadRejectsWithRetryAfter(t *testing.T) {
	up, release := gatedUpstream(t)
	gw, err := New(overloadConfig(up.Listener.Addr().String(), 1, 0, time.Second, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 第一个请求占住唯一的并发额度（它会阻塞在上游）
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r", nil))
	}()

	// 等它确实进到"在途"状态（轮询指标，避免 sleep 猜测）
	deadline := time.Now().Add(5 * time.Second) // 全量 go test 时机器很忙，预算给足（正常几毫秒就满足）
	for time.Now().Before(deadline) {
		if inflight, _ := gw.gate.Stats(); inflight == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if inflight, _ := gw.gate.Stats(); inflight != 1 {
		t.Fatalf("第一个请求应当占住并发额度，实际 inflight=%d", inflight)
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("过载应当返回 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "overloaded") {
		t.Fatalf("响应体应当说明过载，实际 %s", rec.Body.String())
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("拒绝必须立刻返回（不能挂着等），实际 %v", elapsed)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("过载响应必须带 Retry-After（否则客户端只能盲目重试）")
	}
	if got := rec.Header().Get("X-Load-Shed"); got != "queue_full" {
		t.Errorf("X-Load-Shed 应当是 queue_full，实际 %q", got)
	}
	if !strings.Contains(gw.metrics.Render(), `gw_overload_rejected_total{route="r"} 1`) {
		t.Errorf("过载拒绝要计数：\n%s", gw.metrics.Render())
	}

	release()
	wg.Wait()

	// 额度归还后应当恢复正常
	rec2 := httptest.NewRecorder()
	gw.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/r", nil))
	if rec2.Code != 200 {
		t.Fatalf("过载过去后应当恢复 200，实际 %d", rec2.Code)
	}
}

// TestOverloadQueueWaitsThenSucceeds：允许排队时，第二个请求应当**等到**额度释放后成功，
// 而不是被拒。
func TestOverloadQueueWaitsThenSucceeds(t *testing.T) {
	up, release := gatedUpstream(t)
	gw, err := New(overloadConfig(up.Listener.Addr().String(), 1, 4, 2*time.Second, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		gw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	}()
	for i := 0; i < 1000 && func() bool { in, _ := gw.gate.Stats(); return in != 1 }(); i++ {
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r", nil))
		done <- rec.Code
	}()

	// 让第二个请求进入排队，然后放行第一个
	for i := 0; i < 1000; i++ {
		if _, waiting := gw.gate.Stats(); waiting == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, waiting := gw.gate.Stats(); waiting != 1 {
		t.Fatalf("第二个请求应当进入排队，实际 waiting=%d", waiting)
	}
	release()
	wg.Wait()

	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("排队后应当成功，实际 %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("排队请求没有完成")
	}
}

// TestOverloadDoesNotShedHealthz：self 路由（/healthz）不参与负载保护 ——
// 否则负载高时健康检查被拒，LB 会把实例摘掉，剩余实例压力更大（雪崩加速）。
func TestOverloadDoesNotShedHealthz(t *testing.T) {
	up, release := gatedUpstream(t)
	defer release()
	cfg := overloadConfig(up.Listener.Addr().String(), 1, 0, time.Second, time.Second)
	cfg.Routes = append(cfg.Routes, config.Route{
		Name: "health", Host: "*", Path: "/healthz", PathType: config.PathExact, Upstream: "self",
	})
	gw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	// 占满并发
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		gw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	}()
	for i := 0; i < 1000 && func() bool { in, _ := gw.gate.Stats(); return in != 1 }(); i++ {
		time.Sleep(5 * time.Millisecond)
	}

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("健康检查不该被负载保护拒掉，实际 %d", rec.Code)
	}
	release()
	wg.Wait()
}
