package config

import (
	"strings"
	"time"
)

// Config 是网关运行所需的全部配置。
type Config struct {
	ListenAddr      string              // 网关监听地址
	AdminListenAddr string              // 管理端点（/metrics、/debug/logs、/readyz）的独立监听地址；空 = 与业务同端口
	JWTSecret       string              // HS256 签名密钥
	APIKey          string              // 静态 API Key
	TrustedProxies  []string            // 可信代理的 CIDR（如 "10.0.0.0/8"）；只有来自这些地址的请求才采信 XFF/X-Real-IP
	SharedState     SharedStateConfig   // 限流与熔断的共享状态服务（审计 C4）；URL 为空 = 都在进程内
	Overload        OverloadConfig      // 负载保护（审计 C6）；MaxInflight=0 = 不启用
	Routes          []Route             // 路由表（顺序无关，按匹配优先级打分）
	Services        map[string]*Service // 上游服务表，key 为服务名
}

// SharedStateConfig 共享状态服务（限流 + 熔断共用一个服务，审计 C4）。
//
// 为什么值得做：网关的限流桶、熔断器都是**进程内**状态 —— 多副本时每个副本各一份，
// 后果是①限流实际放行量 = 副本数 × 阈值；②A 副本已熔断的服务，B 副本还在打流量。
// 单副本压测完全看不出来这两个问题（这正是"压测通过、扩容出事"的典型来源）。
//
// 一个 URL，两个端点（`cmd/statestore` 是参考实现）：
//
//	POST /ratelimit  限流判定（每次判定一次往返）
//	POST /breaker    发布本地跳闸；GET /breaker 周期性拉取别人的跳闸
//
// ⚠️ 代价与取舍：
//   - 限流判定多一次网络往返（本地是纯内存操作）——只在确实需要跨副本一致时打开；
//   - 熔断状态是**周期性拉取**（SyncInterval），所以副本之间的熔断生效有一个时间窗
//     （默认 1s）——这是刻意的：熔断在热路径上，不能每次都远程问一遍；
//   - 状态服务成了**新的单点**：它必须能快速重启或水平扩展；
//   - 读不到共享状态时**降级**而不是失败：限流按 FailOpen 决策，熔断退回本地状态机
//     （功能不丢，只是退回单副本语义），但两者都会计入 gw_shared_state_errors_total。
type SharedStateConfig struct {
	URL          string   `json:"url,omitempty"`           // 服务基址（如 http://127.0.0.1:18090）；空 = 全在进程内
	Timeout      Duration `json:"timeout,omitempty"`       // 单次访问超时；默认 200ms
	FailOpen     *bool    `json:"fail_open,omitempty"`     // 限流判定失败时是否放行；默认 true
	SyncInterval Duration `json:"sync_interval,omitempty"` // 熔断状态拉取间隔；默认 1s
}

// OverloadConfig 负载保护（审计 C6）。
//
// 与限流的区别（最容易混的一对概念）：
//
//	限流管**速率**（每秒多少），维度是"谁在用"（用户/IP/路由）；
//	负载保护管**并发**（此刻多少在飞），维度是"网关与上游此刻扛不扛得住"。
//
// 上游慢了 10 倍时请求速率可能一点没变，限流完全不触发，而网关里的请求会越堆越多 ——
// 只有并发保护能拦住这种雪崩。阈值怎么定：用压测（scripts/loadtest.py）找出
// 网关在不劣化前提下的并发拐点，取其 70% 左右；MaxQueue 只用来吸收**突发**，
// 不要指望它扛持续过载（那只会把失败推迟到超时那一刻）。
type OverloadConfig struct {
	MaxInflight  int      `json:"max_inflight,omitempty"`  // 全局并发上限；0 = 不启用负载保护
	MaxQueue     int      `json:"max_queue,omitempty"`     // 允许同时排队的请求数；0 = 不排队（并发满立刻 503）
	QueueTimeout Duration `json:"queue_timeout,omitempty"` // 排队等待上限；默认 1s
	RetryAfter   Duration `json:"retry_after,omitempty"`   // 过载 503 里的 Retry-After；默认 1s
}

// RateLimitEndpoint / BreakerEndpoint 由基址推出两个端点（统一在这里拼，避免各处各写一遍）。
func (c *Config) RateLimitEndpoint() string {
	return strings.TrimRight(c.SharedState.URL, "/") + "/ratelimit"
}

// BreakerEndpoint 见 RateLimitEndpoint。
func (c *Config) BreakerEndpoint() string {
	return strings.TrimRight(c.SharedState.URL, "/") + "/breaker"
}

// Service 按名取上游服务，不存在时返回 nil。
func (c *Config) Service(name string) *Service { return c.Services[name] }

// MaxServiceTimeout 返回**单次请求可能花掉的最长时间上界** —— cmd/gateway 用它推导
// http.Server 的 WriteTimeout（必须容得下最慢的一次上游往返）。没有服务时返回 0。
//
// 从批次 C3（每路由超时 + 重试）起，它同时考虑三件事，否则 WriteTimeout 会把还在重试的
// 请求拦腰掐断（客户端看到连接被重置，而不是网关的错误响应）：
//   - 每路由超时 Route.Timeout（覆盖 Service.Timeout）；
//   - 重试：最坏耗时 ≈ 单次尝试超时 × (Attempts+1)；
//   - 重试时若给了 PerTryTimeout，就按它算单次尝试。
func (c *Config) MaxServiceTimeout() time.Duration {
	var max time.Duration
	for _, s := range c.Services {
		if s != nil && s.Timeout > max {
			max = s.Timeout
		}
	}
	for _, rt := range c.Routes {
		svc := c.Services[rt.Upstream]
		if svc == nil {
			continue
		}
		perTry := svc.Timeout
		if rt.Timeout > 0 {
			perTry = time.Duration(rt.Timeout)
		}
		attempts := 1
		if rt.Retry != nil && rt.Retry.Attempts > 0 {
			attempts += rt.Retry.Attempts
			if rt.Retry.PerTryTimeout > 0 {
				perTry = time.Duration(rt.Retry.PerTryTimeout)
			}
		}
		if total := perTry * time.Duration(attempts); total > max {
			max = total
		}
	}
	return max
}

// Default 返回演示用配置：9 条路由 + 6 个上游服务，覆盖全部七级流水线。
//
// 密钥是硬编码的演示值，切勿用于生产。
func Default() *Config {
	return &Config{
		ListenAddr: "127.0.0.1:18080",
		// 管理端点单独监听（审计 P1-6）：业务端口不再暴露 /metrics 与 /debug/logs，
		// 生产可以只让管理端口绑定 127.0.0.1 或内网。留空则退回「与业务同端口」的老行为。
		AdminListenAddr: "127.0.0.1:18081",
		JWTSecret:       "demo-secret-do-not-use-in-prod",
		APIKey:          "ak_live_9f2c41d7",
		// 演示环境是单机直连，没有任何可信代理（因此完全忽略 XFF/X-Real-IP）；
		// 接入 LB 时填 LB 的网段，例如 []string{"10.0.0.0/8", "127.0.0.1/32"}。
		TrustedProxies: nil,
		Routes:         defaultRoutes(),
		Services:       defaultServices(),
	}
}

func defaultRoutes() []Route {
	return []Route{
		{
			Name: "public-health", Host: "*", Path: "/healthz", PathType: PathExact,
			Upstream: "self",
		},
		{
			// 演示 ① 路由匹配：精确匹配优先于前缀
			Name: "order-exact", Host: "api.example.com", Path: "/api/order/list",
			PathType: PathExact, Methods: []string{"GET"},
			Upstream: "order-svc",
			Limit:    LimitPolicy{RatePerSec: 50, Burst: 10},
		},
		{
			// 演示 ① 最长的前缀优先
			Name: "order-prefix", Host: "api.example.com", Path: "/api/order/",
			PathType: PathPrefix, Upstream: "order-svc", StripPrefix: "/api",
			Auth:  AuthPolicy{Required: true, Scheme: "jwt"},
			Limit: LimitPolicy{RatePerSec: 20, Burst: 5},
		},
		{
			// 演示 ① 正则匹配
			Name: "user-regex", Host: "*", Path: `^/api/users/\d+$`, PathType: PathRegex,
			Methods: []string{"GET"}, Upstream: "user-svc", Auth: AuthPolicy{Required: true, Scheme: "jwt"},
			Limit: LimitPolicy{RatePerSec: 10, Burst: 3},
		},
		{
			// 演示 ① Header 参与匹配：只有带上 X-Client: mobile 才命中这条
			Name: "user-mobile", Host: "*", Path: "/api/users/me", PathType: PathExact,
			Headers:  map[string]string{"X-Client": "mobile"},
			Upstream: "user-svc", Auth: AuthPolicy{Required: true, Scheme: "jwt"},
		},
		{
			// 演示 ② API Key 鉴权
			Name: "open-api", Host: "*", Path: "/open/", PathType: PathPrefix,
			Upstream: "order-svc", Auth: AuthPolicy{Required: true, Scheme: "apikey"},
			Limit: LimitPolicy{RatePerSec: 5, Burst: 2},
		},
		{
			// 演示 ③ 需要一个「熔断演示」的上游：它只有一条很窄的路由
			Name: "flaky", Host: "*", Path: "/flaky", PathType: PathExact,
			Upstream: "flaky-svc",
		},
		{
			// 演示 ⑤ 协议转换：外部 REST，内部 gRPC
			Name: "order-create", Host: "*", Path: "/api/orders", PathType: PathExact,
			Methods: []string{"POST"}, Upstream: "grpc-order-svc",
			Auth:      AuthPolicy{Required: true, Scheme: "jwt", Roles: []string{"admin", "user"}},
			Limit:     LimitPolicy{RatePerSec: 10, Burst: 3},
			Transcode: &TranscodePolicy{Service: "order.OrderService", Method: "CreateOrder"},
		},
		{
			// 演示 ④ 最少连接：配合后端的 `?ms=800` 让请求真的占住连接，
			// 才看得出「新请求落到在途最少的节点」——计数由主流水线的 Pick/Done 成对维护。
			Name: "slow", Host: "*", Path: "/slow", PathType: PathExact,
			Upstream: "slow-svc",
		},
	}
}

func defaultServices() map[string]*Service {
	return map[string]*Service{
		"self": {
			Name: "self", Balance: "round_robin", Timeout: time.Second,
			// ⚠️ 零值 BreakerConfig：FailRatio=0 意味着「一次失败即熔断」。
			// 目前 /healthz 在熔断检查之前就返回，所以未暴露；不打算熔断的服务应显式配置。
			Breaker: BreakerConfig{},
		},
		"order-svc": {
			Name: "order-svc", Balance: "round_robin",
			Upstreams: []Upstream{
				{Addr: "127.0.0.1:19001", Weight: 3, Backend: 1},
				{Addr: "127.0.0.1:19002", Weight: 1, Backend: 2},
				{Addr: "127.0.0.1:19003", Weight: 1, Backend: 3},
			},
			Timeout: 2 * time.Second,
			// 节点级被动摘除（审计 P1-2）：连续 3 次 5xx 就摘掉那个节点 5 秒，到期放一个探测
			Ejection: EjectionConfig{FailThreshold: 3, Cooldown: 5 * time.Second},
			Breaker: BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4,
				OpenFor: 3 * time.Second, HalfOpenMax: 2},
		},
		"user-svc": {
			Name: "user-svc", Balance: "consistent_hash", HashKeyFrom: "X-User-Id",
			Upstreams: []Upstream{
				{Addr: "127.0.0.1:19001", Backend: 1},
				{Addr: "127.0.0.1:19002", Backend: 2},
				{Addr: "127.0.0.1:19003", Backend: 3},
			},
			Timeout:  2 * time.Second,
			Ejection: EjectionConfig{FailThreshold: 3, Cooldown: 5 * time.Second},
			Breaker: BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4,
				OpenFor: 3 * time.Second, HalfOpenMax: 2},
		},
		"flaky-svc": {
			// 指向能通过 /__control?fail=1 切换失败的真实后端，这样才能演示「熔断→恢复」完整闭环。
			// ⚠️ 故意**不配** Ejection：它只有一个节点，摘掉等于没有上游（此时会 fail-open 回退全量），
			// 这里要演示的是服务级熔断，两者分开看更清楚。
			Name: "flaky-svc", Balance: "round_robin",
			Upstreams: []Upstream{{Addr: "127.0.0.1:19003", Backend: 3}},
			Timeout:   1 * time.Second,
			Breaker: BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4,
				OpenFor: 3 * time.Second, HalfOpenMax: 2},
		},
		"slow-svc": {
			// 演示 ④ least_conn：三个节点权重相同，「谁在途请求最少就发给谁」。
			// 节点计数由 balancer.New 初始化、由主流水线的 Pick/Done 成对维护
			//（只 Pick 不 Done 会让计数只增不减 —— 这条链路以前断过，见 AGENTS.md §5）。
			Name: "slow-svc", Balance: "least_conn",
			Upstreams: []Upstream{
				{Addr: "127.0.0.1:19001", Backend: 1},
				{Addr: "127.0.0.1:19002", Backend: 2},
				{Addr: "127.0.0.1:19003", Backend: 3},
			},
			Timeout:  10 * time.Second, // 要容得下后端的 ?ms= 人为延时
			Ejection: EjectionConfig{FailThreshold: 3, Cooldown: 5 * time.Second},
			Breaker: BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4,
				OpenFor: 3 * time.Second, HalfOpenMax: 2},
		},
		"grpc-order-svc": {
			Name: "grpc-order-svc", Balance: "round_robin",
			Upstreams: []Upstream{{Addr: "127.0.0.1:19100", GRPC: true, Backend: 1}},
			Timeout:   3 * time.Second,
			Breaker: BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4,
				OpenFor: 3 * time.Second, HalfOpenMax: 2},
		},
	}
}
