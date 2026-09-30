package observability

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHistogramCumulativeBuckets：直方图的桶是**累积**的（le 语义：<= 上界的次数），
// 这一点写错了 Prometheus 算出来的分位会完全不对。
func TestHistogramCumulativeBuckets(t *testing.T) {
	m := NewMetrics()
	m.Observe("r", 200, 5*time.Millisecond)   // 落在 0.005 及以上的所有桶
	m.Observe("r", 200, 50*time.Millisecond)  // 落在 0.05 及以上
	m.Observe("r", 200, 7*time.Second)        // 只落在 +Inf（超过最大桶 10s 之前：7 <= 10，落在 10）
	m.Observe("r", 500, 300*time.Millisecond) // 落在 0.5 及以上

	out := m.Render()
	// 4 次观测、均 <= 10s，所以 le="10" 的累积值应当是 4；+Inf 也是 4
	if !strings.Contains(out, `gw_request_duration_seconds_bucket{route="r",le="0.005"} 1`) {
		t.Errorf("le=0.005 应当是 1（只有 5ms 那次）\n%s", pick(out, "gw_request_duration_seconds_bucket"))
	}
	if !strings.Contains(out, `gw_request_duration_seconds_bucket{route="r",le="0.05"} 2`) {
		t.Errorf("le=0.05 应当是 2（5ms 与 50ms）\n%s", pick(out, "gw_request_duration_seconds_bucket"))
	}
	if !strings.Contains(out, `gw_request_duration_seconds_bucket{route="r",le="+Inf"} 4`) {
		t.Errorf("+Inf 应当等于总次数 4\n%s", pick(out, "gw_request_duration_seconds_bucket"))
	}
	if !strings.Contains(out, `gw_request_duration_seconds_count{route="r"} 4`) {
		t.Errorf("count 应当是 4")
	}
	if got := metricValue(out, `gw_request_duration_seconds_sum{route="r"}`); math.Abs(got-7.355) > 1e-9 {
		t.Errorf("sum 应当是 0.005+0.05+7+0.3=7.355（浮点容差 1e-9），实际 %v", got)
	}
}

// TestRenderIsStable：同一份指标连渲染两次必须**逐字节相同**。
// 这是 P2-5（Render 里三处 map 未排序）的回归用例：map 迭代顺序随机会让看板与 diff 抖动。
// 运行时指标（goroutine 数等）会自己变，所以比对前先剔掉。
func TestRenderIsStable(t *testing.T) {
	m := NewMetrics()
	for _, r := range []string{"b", "a", "c", "a"} {
		m.Observe(r, 200, 3*time.Millisecond)
	}
	m.IncLimit("z")
	m.IncLimit("a")
	m.IncTrip("svc-b")
	m.IncTrip("svc-a")
	m.IncStateError("ratelimit")

	first := stripRuntime(m.Render())
	for i := 0; i < 5; i++ {
		if got := stripRuntime(m.Render()); got != first {
			t.Fatalf("第 %d 次渲染与首次不同（说明输出里有未排序的 map）:\n--- 首次 ---\n%s\n--- 本次 ---\n%s", i+1, first, got)
		}
	}
}

// TestRenderIncludesHistogramAndRuntime：直方图与运行时指标都得在 /metrics 里出现
// （审计 C5 的两项：可聚合的分位 + 最小的 go_* 替代品）。
func TestRenderIncludesHistogramAndRuntime(t *testing.T) {
	m := NewMetrics()
	m.Observe("order-list", 200, 12*time.Millisecond)
	m.Observe("order-list", 500, 2*time.Second)
	out := m.Render()

	for _, want := range []string{
		"# TYPE gw_request_duration_seconds histogram",
		`gw_request_duration_seconds_bucket{route="order-list",le="0.025"} 1`,
		`gw_request_duration_seconds_count{route="order-list"} 2`,
		"gw_runtime_goroutines",
		"gw_runtime_mem_alloc_bytes",
		"gw_runtime_gc_cycles",
		// 旧的分位指标保留（肉眼对比用），但要能看出它不该被聚合
		`gw_request_duration_ms{route="order-list",quantile="p95"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里应当包含 %q\n%s", want, pick(out, "gw_"))
		}
	}
}

// pick 把输出里匹配前缀的行挑出来，便于失败时定位。
func pick(out, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// stripRuntime 去掉会自己变化的运行时指标行。
func stripRuntime(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "gw_runtime_") || strings.Contains(line, "# HELP gw_runtime") ||
			strings.Contains(line, "# TYPE gw_runtime") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// metricValue 从渲染结果里取出某个指标的值（找不到返回 NaN）。
func metricValue(out, name string) float64 {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 64)
			if err == nil {
				return v
			}
		}
	}
	return math.NaN()
}
