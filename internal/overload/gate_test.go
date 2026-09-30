package overload

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGateNilWhenDisabled：MaxInflight<=0 表示不启用，Acquire 必须直接放行。
func TestGateNilWhenDisabled(t *testing.T) {
	g := New(0, 10)
	if g != nil {
		t.Fatal("MaxInflight=0 应当返回 nil（不启用）")
	}
	release, _, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("不启用时不该拒绝：%v", err)
	}
	release() // 不 panic 即可
	if g.Limit() != 0 {
		t.Fatalf("未启用时 Limit 应当是 0，实际 %d", g.Limit())
	}
}

// TestGateQueueFull：并发满且队列满时必须**立刻**拒绝（reason=queue_full），不能挂住。
func TestGateQueueFull(t *testing.T) {
	g := New(1, 1) // 1 个并发 + 1 个排队位
	ctx := context.Background()

	r1, _, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("第一个应当放行：%v", err)
	}
	defer r1()

	// 第二个占住唯一的排队位（它会一直等到 ctx 结束）
	holdCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, _, err := g.Acquire(holdCtx)
		if err == nil {
			release()
		}
	}()
	time.Sleep(20 * time.Millisecond) // 让它进入排队

	start := time.Now()
	_, reason, err := g.Acquire(ctx)
	elapsed := time.Since(start)
	if err != ErrQueueFull || reason != ReasonQueueFull {
		t.Fatalf("第三个应当被立刻拒绝（queue_full），实际 err=%v reason=%v", err, reason)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("拒绝必须立刻返回，实际等了 %v", elapsed)
	}
	cancel()
	wg.Wait()
}

// TestGateQueueTimeout：排队等到超时必须返回 queue_timeout，而不是无限等。
func TestGateQueueTimeout(t *testing.T) {
	g := New(1, 4)
	r1, _, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, reason, err := g.Acquire(ctx)
	if err != ErrQueueTimeout || reason != ReasonQueueTimeout {
		t.Fatalf("应当排队超时，实际 err=%v reason=%v", err, reason)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("应当在超时之后才返回，实际 %v", d)
	}
}

// TestGateReleaseIsIdempotent：重复 release 不能把额度还两次（那会让闸门逐渐失效）。
func TestGateReleaseIsIdempotent(t *testing.T) {
	g := New(1, 0)
	r1, _, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r1()
	r1() // 再调一次

	// 额度应当刚好回到 1（只有一次归还）
	r2, _, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("归还后应当能再拿到：%v", err)
	}
	defer r2()
	// 此时并发是 1，非阻塞再取应当排队/失败（用 0 队列 + 背景 ctx）
	if _, reason, err := g.Acquire(context.Background()); err != ErrQueueFull {
		t.Fatalf("重复 release 不该多还额度（并发上限被突破），实际 err=%v reason=%v", err, reason)
	}
}

// TestGateNeverExceedsLimit：并发压测 —— 无论怎么抢，在处理的请求数不得超过上限。
func TestGateNeverExceedsLimit(t *testing.T) {
	const limit = 4
	g := New(limit, 32)

	var maxSeen int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			release, _, err := g.Acquire(ctx)
			if err != nil {
				return // 过载被拒是预期内的
			}
			defer release()
			if n := g.inflight.Load(); n > limit {
				atomic.StoreInt64(&maxSeen, n)
			}
			time.Sleep(time.Millisecond)
		}()
	}
	wg.Wait()
	if maxSeen > limit {
		t.Fatalf("在处理的请求数突破了上限：观察到 %d > %d", maxSeen, limit)
	}
	if inflight, waiting := g.Stats(); inflight != 0 || waiting != 0 {
		t.Fatalf("压测结束后应当归零，实际 inflight=%d waiting=%d", inflight, waiting)
	}
}
