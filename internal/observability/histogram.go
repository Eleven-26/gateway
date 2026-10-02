package observability

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// 手写 Prometheus 直方图。
//
// 为什么需要：原来的分位是"当场算出来的近似值"（`gw_request_duration_ms{quantile="p95"}`），
// 它**不能跨实例聚合** —— 多副本时没法把两个副本的 p95 合起来得到全局 p95，
// 看板上按副本聚合出来的"全局延迟"是错的。直方图没有这个问题：
// 各副本只上报"落在每个桶里的次数"，聚合交给 Prometheus 的 `histogram_quantile()`，
// 这也是 Prometheus 生态的标准做法。
//
// 取舍：固定桶换来"可聚合 + 常数内存 + 无锁竞争下的稳定输出"，代价是精度受桶宽限制
// （桶边界是按网关的延迟量级选的：1ms ~ 10s，覆盖本地回环到上游超时）。
//
// ⚠️ 单位用**秒**：`gw_request_duration_seconds_*` 是 Prometheus 的惯例（`histogram_quantile`
// 与各种看板模板都假定秒）。老的毫秒分位指标保留，方便直接肉眼对比，但不要再拿它做聚合。
var durationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Histogram 固定桶直方图。桶边界是只读的，计数与和都在锁内更新。
type Histogram struct {
	mu     sync.Mutex
	counts []int64 // 与 bounds 一一对应（不含 +Inf）
	sum    float64
	count  int64
}

// NewHistogram 创建直方图；bounds 必须升序（内部直接用，不做校验，调用方是常量）。
func NewHistogram(bounds []float64) *Histogram {
	return &Histogram{counts: make([]int64, len(bounds))}
}

// Observe 记录一次观测值（单位与桶一致，这里是秒）。
//
// ⚠️ 只把计数加到**第一个匹配的桶**上，累积在渲染时做（Prometheus 的 le 是"<="语义）。
// 两边都累积的话数值会被算两遍 —— 本机实测踩过：le 序列变成 0,0,1,2,3,5,7…（像累积其实错位）。
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += v
	for i, ub := range durationBuckets {
		if v <= ub {
			h.counts[i]++
			return // 超过最大桶（v > 10s）时不落任何桶，只进 +Inf（由 count 体现）
		}
	}
}

// histogramView 是渲染用的快照（值拷贝，避免持锁渲染）。
type histogramView struct {
	counts []int64
	sum    float64
	count  int64
}

func (h *Histogram) snapshot() histogramView {
	h.mu.Lock()
	defer h.mu.Unlock()
	return histogramView{counts: append([]int64(nil), h.counts...), sum: h.sum, count: h.count}
}

// ObserveDuration 由 Metrics.Observe 调用（在 m.mu 内，所以这里只碰自己的锁）。
func (m *Metrics) observeDuration(route string, seconds float64) {
	h := m.durations[route]
	if h == nil {
		h = NewHistogram(durationBuckets)
		m.durations[route] = h
	}
	h.Observe(seconds)
}

// renderDurations 输出直方图（Prometheus 文本格式）。
//
// ⚠️ 调用方（Render）已经持有 m.mu，这里**不能**再锁 m.mu（Histogram 自己的锁是另一把，可以拿）。
func (m *Metrics) renderDurations() string {
	if len(m.durations) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# HELP gw_request_duration_seconds 请求耗时直方图（可跨实例聚合，用 histogram_quantile 算分位）\n")
	b.WriteString("# TYPE gw_request_duration_seconds histogram\n")
	for _, route := range sortedKeys(m.durations) {
		v := m.durations[route].snapshot()
		cumulative := int64(0)
		for i, ub := range durationBuckets {
			cumulative += v.counts[i]
			fmt.Fprintf(&b, "gw_request_duration_seconds_bucket{route=%q,le=%q} %d\n",
				route, trimFloat(ub), cumulative)
		}
		// +Inf 桶 = 总次数（所有观测值都 <= +Inf）
		fmt.Fprintf(&b, "gw_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", route, v.count)
		fmt.Fprintf(&b, "gw_request_duration_seconds_sum{route=%q} %g\n", route, v.sum)
		fmt.Fprintf(&b, "gw_request_duration_seconds_count{route=%q} %d\n", route, v.count)
	}
	return b.String()
}

// renderRuntime 输出最小的运行时指标。
//
// 只挑排障最常用的四个：goroutine 数（泄漏第一眼）、堆内存、向 OS 申请的内存、GC 次数。
// 不引 client_golang 就无法复用官方的采集器，所以这里显式用 runtime 包自己取。
//
// ⚠️ runtime.ReadMemStats 会短暂 STW（几百微秒量级）——只在被抓取时调用，不要放进请求路径。
func (m *Metrics) renderRuntime() string {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	var b strings.Builder
	b.WriteString("# HELP gw_runtime_goroutines 当前 goroutine 数（泄漏时第一眼看它）\n")
	b.WriteString("# TYPE gw_runtime_goroutines gauge\n")
	fmt.Fprintf(&b, "gw_runtime_goroutines %d\n", runtime.NumGoroutine())

	b.WriteString("# HELP gw_runtime_mem_alloc_bytes 已分配（仍在用）的堆字节数\n")
	b.WriteString("# TYPE gw_runtime_mem_alloc_bytes gauge\n")
	fmt.Fprintf(&b, "gw_runtime_mem_alloc_bytes %d\n", ms.Alloc)

	b.WriteString("# HELP gw_runtime_mem_sys_bytes 向操作系统申请的内存字节数\n")
	b.WriteString("# TYPE gw_runtime_mem_sys_bytes gauge\n")
	fmt.Fprintf(&b, "gw_runtime_mem_sys_bytes %d\n", ms.Sys)

	b.WriteString("# HELP gw_runtime_gc_cycles 完成的 GC 次数\n")
	b.WriteString("# TYPE gw_runtime_gc_cycles counter\n")
	fmt.Fprintf(&b, "gw_runtime_gc_cycles %d\n", ms.NumGC)
	return b.String()
}

// sortedKeys 返回 map 的键（已排序）。
//
// /metrics 的输出必须**稳定**：map 迭代顺序随机会让每次抓取的行序都不同，
// 看板与 diff 都会抖。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// trimFloat 让桶边界输出成 0.001 / 0.01 / 1 / 10 这种干净形式（Prometheus 的 le 是字符串标签）。
func trimFloat(v float64) string {
	s := fmt.Sprintf("%g", v)
	return s
}
