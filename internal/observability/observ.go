// Package observability 实现 ⑦ 可观测性：TraceID 透传 + 结构化访问日志 +
// Prometheus 文本指标。
//
// 三件事的关系是「一条请求的三个切面」：
//
//	· TraceID 解决「这一次请求经过了哪些环节」（单请求维度）；
//	· 日志解决「这一次请求发生了什么」（单请求维度、可读）；
//	· 指标解决「一万次请求整体怎么样」（聚合维度、便宜）。
//
// 不引任何 SDK：TraceID 用 16 字节随机数，指标直接输出 Prometheus 文本格式。
// 交换条件是这里的指标只有计数器和简单分位，生产应换成 client_golang + 直方图桶。
package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// TraceHeader 全链路透传的 TraceID 头名。
const TraceHeader = "X-Trace-Id"

// EnsureTraceID 复用上游传进来的（网关可能是链路中间一环），没有才生成。
//
// 从批次 C5 起它只是 StartTrace 的薄封装（保留旧调用点与测试）：真正干活的是
// StartTrace —— 它会先看 W3C traceparent，再退回 X-Trace-Id，并为本跳生成 span-id。
func EnsureTraceID(h http.Header) string { return StartTrace(h).TraceID }

// ---- 访问日志 ----

// Entry 是一条访问日志的结构化字段。
type Entry struct {
	TraceID    string
	Method     string
	Path       string
	Host       string
	ClientIP   string // 真实客户端 IP（走可信代理链解析，审计 P1-3）
	SpanID     string // 本跳 span-id（W3C traceparent 的 16 位，审计 C5）
	Route      string // 命中的路由名，空 = 未匹配
	MatchWhy   string // 为什么命中（exact/prefix/regex）
	Identity   string
	Upstream   string // 实际转发的节点
	Status     int
	Latency    time.Duration // 网关侧总耗时
	UpstreamMs int64         // 上游往返耗时（毫秒）；与 Latency 的差就是网关自身开销（审计 B8）
	Bytes      int64
	LimitHit   bool
	BreakerSt  string
	Err        string
}

// AccessLog 进程内保留最近 N 条访问日志，供 /debug/logs 查看。
type AccessLog struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewAccessLog 创建定长环形访问日志。
func NewAccessLog(max int) *AccessLog { return &AccessLog{max: max} }

// Write 格式化一条日志：既打 stdout，也留一份在内存里。
func (l *AccessLog) Write(e Entry) {
	// 结构化 key=value：既能人读，也能被 Filebeat / Vector 直接切成字段
	parts := []string{
		"ts=" + time.Now().Format("15:04:05.000"),
		// trace 记**全量**：响应体里给客户端的就是它，只有全长才能直接对齐排查（审计 B8）。
		// trace8 是给人眼扫日志用的短前缀，不再是唯一线索。
		"trace=" + e.TraceID,
		"trace8=" + Short(e.TraceID),
		"span=" + OrDash(e.SpanID),
		"method=" + e.Method,
		"host=" + e.Host,
		"client=" + OrDash(e.ClientIP),
		"path=" + e.Path,
		"route=" + OrDash(e.Route),
		"via=" + OrDash(e.MatchWhy),
		"who=" + OrDash(e.Identity),
		"upstream=" + OrDash(e.Upstream),
		"status=" + fmt.Sprint(e.Status),
		"latency=" + fmt.Sprintf("%.1fms", float64(e.Latency.Microseconds())/1000),
		"upstream_ms=" + fmt.Sprint(e.UpstreamMs),
		"bytes=" + fmt.Sprint(e.Bytes),
	}
	if e.LimitHit {
		parts = append(parts, "limit=hit")
	}
	if e.BreakerSt != "" {
		parts = append(parts, "breaker="+e.BreakerSt)
	}
	if e.Err != "" {
		parts = append(parts, "err="+e.Err)
	}
	line := strings.Join(parts, " ")

	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.mu.Unlock()

	fmt.Println(line) // 演示：直接打到 stdout
}

// Tail 取最近 n 条。
func (l *AccessLog) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.lines) {
		n = len(l.lines)
	}
	out := make([]string, n)
	copy(out, l.lines[len(l.lines)-n:])
	return out
}

// Short 截断 TraceID，给「人眼扫日志」用的短前缀（全量 ID 在 trace= 字段里）。
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// OrDash 空字符串显示成 "-"，让日志字段对齐。
func OrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- 指标 ----

const maxLatencySamples = 512

// Metrics 计数器 + 延迟样本（够看趋势，生产换直方图）。
type Metrics struct {
	mu        sync.Mutex
	requests  map[string]int64      // route|status → 次数
	latency   map[string][]int64    // route → 最近 N 个延迟(ms)【旧：不可跨实例聚合，保留仅为肉眼对比】
	durations map[string]*Histogram // route → 耗时直方图（审计 C5，可聚合）
	limitHits map[string]int64
	cbTrips   map[string]int64
	stateErrs map[string]int64 // 共享状态后端判定失败次数（审计 C4，fail-open 兜底）
	overloads map[string]int64 // 被负载保护拒绝的请求数（审计 C6）

	// 负载保护的可观测性：上限（配置）与队列长度（运行时），由 gateway 注入
	overloadLimit int
	overloadStats func() (inflight, waiting int64)
	inflight      int64
	panics        int64
}

// NewMetrics 创建指标集合。
func NewMetrics() *Metrics {
	return &Metrics{
		requests:  map[string]int64{},
		latency:   map[string][]int64{},
		durations: map[string]*Histogram{},
		limitHits: map[string]int64{},
		cbTrips:   map[string]int64{},
		stateErrs: map[string]int64{},
		overloads: map[string]int64{},
	}
}

// IncInflight 增减在处理中的请求数。
func (m *Metrics) IncInflight(d int64) {
	m.mu.Lock()
	m.inflight += d
	m.mu.Unlock()
}

// Observe 记录一次请求的状态码与耗时。
func (m *Metrics) Observe(route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[route+"|"+fmt.Sprint(status)]++
	s := m.latency[route]
	s = append(s, d.Milliseconds())
	if len(s) > maxLatencySamples {
		s = s[len(s)-maxLatencySamples:]
	}
	m.latency[route] = s
	m.observeDuration(route, d.Seconds())
}

// IncLimit 记一次限流拒绝。
func (m *Metrics) IncLimit(route string) {
	m.mu.Lock()
	m.limitHits[route]++
	m.mu.Unlock()
}

// IncTrip 记一次熔断器打开。
func (m *Metrics) IncTrip(service string) {
	m.mu.Lock()
	m.cbTrips[service]++
	m.mu.Unlock()
}

// IncStateError 记一次「共享状态后端判定失败」（审计 C4）。
//
// 为什么必须打点：fail-open 的设计是"状态服务挂了就放行"，但**静默放行等于把限流失效藏起来** ——
// 只有这个计数能让人发现"限流早就没在生效了"。
func (m *Metrics) IncStateError(backend string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stateErrs[backend]++
}

// IncOverload 记一次被负载保护拒绝的请求（审计 C6）。
func (m *Metrics) IncOverload(route string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overloads[route]++
}

// AttachOverload 注入负载保护的配置上限与实时队列长度（供 /metrics 展示）。
func (m *Metrics) AttachOverload(limit int, stats func() (int64, int64)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overloadLimit, m.overloadStats = limit, stats
}

// renderOverload 输出负载保护相关的指标（调用方已持 m.mu）。
func (m *Metrics) renderOverload() string {
	var b strings.Builder
	if len(m.overloads) > 0 {
		b.WriteString("# HELP gw_overload_rejected_total 被负载保护（并发上限/排队）拒绝的请求数\n")
		b.WriteString("# TYPE gw_overload_rejected_total counter\n")
		for _, route := range sortedKeys(m.overloads) {
			fmt.Fprintf(&b, "gw_overload_rejected_total{route=%q} %d\n", route, m.overloads[route])
		}
	}
	if m.overloadLimit > 0 {
		b.WriteString("# HELP gw_inflight_limit 配置的并发上限（0 = 未启用负载保护）\n")
		b.WriteString("# TYPE gw_inflight_limit gauge\n")
		fmt.Fprintf(&b, "gw_inflight_limit %d\n", m.overloadLimit)
		b.WriteString("# HELP gw_queue_waiting 当前排队等待并发槽位的请求数\n")
		b.WriteString("# TYPE gw_queue_waiting gauge\n")
		if m.overloadStats != nil {
			_, waiting := m.overloadStats()
			fmt.Fprintf(&b, "gw_queue_waiting %d\n", waiting)
		} else {
			b.WriteString("gw_queue_waiting 0\n")
		}
	}
	return b.String()
}

// IncPanic 记一次被兜底捕获的 panic。这类错误以前只走 stderr，
// /metrics 与 /debug/logs 里完全看不到（审计 P0-2）。
func (m *Metrics) IncPanic() {
	m.mu.Lock()
	m.panics++
	m.mu.Unlock()
}

// Render 输出 Prometheus 文本格式（http://…/metrics 直接可抓）。
func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP gw_requests_total 按路由与状态码统计的请求数\n")
	b.WriteString("# TYPE gw_requests_total counter\n")
	keys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		route, status, _ := strings.Cut(k, "|")
		fmt.Fprintf(&b, "gw_requests_total{route=%q,status=%q} %d\n", route, status, m.requests[k])
	}

	b.WriteString("# HELP gw_request_duration_ms 请求耗时（当前样本的 p50/p95/p99）\n")
	b.WriteString("# TYPE gw_request_duration_ms gauge\n")
	for _, route := range sortedKeys(m.latency) {
		s := m.latency[route]
		if len(s) == 0 {
			continue
		}
		cp := append([]int64(nil), s...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		for _, q := range []struct {
			name string
			p    float64
		}{{"p50", 0.5}, {"p95", 0.95}, {"p99", 0.99}} {
			v := cp[int(float64(len(cp)-1)*q.p)]
			fmt.Fprintf(&b, "gw_request_duration_ms{route=%q,quantile=%q} %d\n", route, q.name, v)
		}
	}

	b.WriteString("# HELP gw_limit_rejected_total 被限流拒绝的请求数\n")
	b.WriteString("# TYPE gw_limit_rejected_total counter\n")
	for _, route := range sortedKeys(m.limitHits) {
		v := m.limitHits[route]
		fmt.Fprintf(&b, "gw_limit_rejected_total{route=%q} %d\n", route, v)
	}

	b.WriteString("# HELP gw_breaker_trips_total 熔断器打开次数\n")
	b.WriteString("# TYPE gw_breaker_trips_total counter\n")
	for _, svc := range sortedKeys(m.cbTrips) {
		v := m.cbTrips[svc]
		fmt.Fprintf(&b, "gw_breaker_trips_total{service=%q} %d\n", svc, v)
	}

	fmt.Fprintf(&b, "# HELP gw_inflight_requests 在处理中的请求数\n# TYPE gw_inflight_requests gauge\ngw_inflight_requests %d\n", m.inflight)
	fmt.Fprintf(&b, "# HELP gw_panics_total 被网关兜底捕获的 panic 次数\n# TYPE gw_panics_total counter\ngw_panics_total %d\n", m.panics)
	{
		keys := make([]string, 0, len(m.stateErrs))
		for k := range m.stateErrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			fmt.Fprint(&b, "# HELP gw_shared_state_errors_total 共享状态后端判定失败次数（fail-open 兜底）\n# TYPE gw_shared_state_errors_total counter\n")
			for _, k := range keys {
				fmt.Fprintf(&b, "gw_shared_state_errors_total{backend=%q} %d\n", k, m.stateErrs[k])
			}
		}
	}
	b.WriteString(m.renderOverload())
	b.WriteString(m.renderDurations())
	b.WriteString(m.renderRuntime())
	return b.String()
}
