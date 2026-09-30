package config

import "time"

// Config 是网关运行所需的全部配置。
type Config struct {
	ListenAddr string              // 网关监听地址
	JWTSecret  string              // HS256 签名密钥
	APIKey     string              // 静态 API Key
	Routes     []Route             // 路由表（顺序无关，按匹配优先级打分）
	Services   map[string]*Service // 上游服务表，key 为服务名
}

// Service 按名取上游服务，不存在时返回 nil。
func (c *Config) Service(name string) *Service { return c.Services[name] }

// Default 返回演示用配置：9 条路由 + 6 个上游服务，覆盖全部七级流水线。
//
// 密钥是硬编码的演示值，切勿用于生产。
func Default() *Config {
	return &Config{
		ListenAddr: "127.0.0.1:18080",
		JWTSecret:  "demo-secret-do-not-use-in-prod",
		APIKey:     "ak_live_9f2c41d7",
		Routes:     defaultRoutes(),
		Services:   defaultServices(),
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
			Timeout: 2 * time.Second,
			Breaker: BreakerConfig{WindowSize: 10, FailRatio: 0.5, MinRequests: 4,
				OpenFor: 3 * time.Second, HalfOpenMax: 2},
		},
		"flaky-svc": {
			// 指向能通过 /__control?fail=1 切换失败的真实后端，这样才能演示「熔断→恢复」完整闭环
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
			Timeout: 10 * time.Second, // 要容得下后端的 ?ms= 人为延时
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
