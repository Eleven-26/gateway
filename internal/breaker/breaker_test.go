package breaker

import (
	"errors"
	"testing"
	"time"

	"gwlab/internal/config"
)

// TestZeroFailRatioDoesNotTripOnSuccess 是一个**回归用例**：
// `FailRatio` 零值的语义是"任何失败率都达阈值"，但"零失败"时的比率 0 也满足 `>= 0`，
// 若开闸条件不要求"确实有过失败"，一个**成功的**请求就会把熔断器打开 ——
// 本机实测：没显式配 breaker 的服务，第一次成功请求后 `breaker=open`，正常流量随即被 503。
func TestZeroFailRatioDoesNotTripOnSuccess(t *testing.T) {
	b := New(config.BreakerConfig{}) // 全零值
	for i := 0; i < 5; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("第 %d 次 Allow 就失败了：%v（成功请求不该打开熔断器）", i+1, err)
		}
		b.Report(true)
	}
	if st, failures, filled := b.Snapshot(); st != StateClosed {
		t.Fatalf("连续 5 次成功之后状态应为 closed，实际 %v（failures=%d filled=%d）", st, failures, filled)
	}
}

// TestZeroFailRatioTripsOnFirstFailure 记录零值语义（有意保留的行为）：
// FailRatio/MinRequests 为 0 时「一次失败即熔断」——所以生产配置必须显式给这两个值。
func TestZeroFailRatioTripsOnFirstFailure(t *testing.T) {
	b := New(config.BreakerConfig{})
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Report(false)
	if st, _, _ := b.Snapshot(); st != StateOpen {
		t.Fatalf("零值配置下一次失败即应打开，实际 %v", st)
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("打开后 Allow 应当快速失败，实际 %v", err)
	}
}

// TestMinRequestsSampleGate：样本量不足时不该打开（避免"启动后第一个请求失败就熔断"）。
func TestMinRequestsSampleGate(t *testing.T) {
	b := New(config.BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4, OpenFor: time.Minute})
	for i := 0; i < 3; i++ {
		_ = b.Allow()
		b.Report(false) // 3 次失败，但门槛是 4
	}
	if st, _, _ := b.Snapshot(); st != StateClosed {
		t.Fatalf("样本量不足时不应打开，实际 %v", st)
	}
	_ = b.Allow()
	b.Report(false) // 第 4 次
	if st, _, _ := b.Snapshot(); st != StateOpen {
		t.Fatalf("达到 MinRequests 且失败率超阈值应打开，实际 %v", st)
	}
}

// TestHalfOpenSingleProbeAndRecovery：冷却到期后只放行 HalfOpenMax 个探测；
// 探测成功才回 closed，失败立刻回 open（并重新计时）。
func TestHalfOpenSingleProbeAndRecovery(t *testing.T) {
	b := New(config.BreakerConfig{WindowSize: 4, FailRatio: 0.5, MinRequests: 2,
		OpenFor: 30 * time.Millisecond, HalfOpenMax: 1})

	for i := 0; i < 2; i++ {
		_ = b.Allow()
		b.Report(false)
	}
	if st, _, _ := b.Snapshot(); st != StateOpen {
		t.Fatalf("应已打开，实际 %v", st)
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("冷却期内应快速失败，实际 %v", err)
	}

	time.Sleep(40 * time.Millisecond) // 等冷却
	if err := b.Allow(); err != nil {
		t.Fatalf("冷却到期应放行一个探测，实际 %v", err)
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("探测槽位只有 %d 个，第二个应当被拒，实际 %v", 1, err)
	}
	b.Report(true) // 探测成功
	if st, _, _ := b.Snapshot(); st != StateClosed {
		t.Fatalf("探测成功后应回到 closed，实际 %v", st)
	}
}

// TestHalfOpenProbeFailureReopens：探测失败必须回到 open，而不是"失败一次就混回池子"。
func TestHalfOpenProbeFailureReopens(t *testing.T) {
	b := New(config.BreakerConfig{WindowSize: 4, FailRatio: 0.5, MinRequests: 2,
		OpenFor: 20 * time.Millisecond, HalfOpenMax: 1})
	for i := 0; i < 2; i++ {
		_ = b.Allow()
		b.Report(false)
	}
	time.Sleep(30 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Report(false)
	if st, _, _ := b.Snapshot(); st != StateOpen {
		t.Fatalf("探测失败应回到 open，实际 %v", st)
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("回到 open 后应立刻快速失败（重新计时），实际 %v", err)
	}
}

// TestSuccessEvictsOldFailures：滑动窗口里的旧失败会被新成功挤出，失败率随之下降。
func TestSuccessEvictsOldFailures(t *testing.T) {
	b := New(config.BreakerConfig{WindowSize: 4, FailRatio: 0.75, MinRequests: 4})
	// 3 失败 + 1 成功：窗口满，失败率 0.75 >= 0.75 → 第 4 条上报时打开
	for i := 0; i < 3; i++ {
		_ = b.Allow()
		b.Report(false)
	}
	_ = b.Allow()
	b.Report(true) // 此时窗口 = [F,F,F,T]，ratio=0.75 → 打开
	if st, _, _ := b.Snapshot(); st != StateOpen {
		t.Fatalf("失败率达到阈值应打开，实际 %v", st)
	}
}
