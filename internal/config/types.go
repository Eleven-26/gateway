// Package config 定义网关的声明式配置模型。
//
// 一条 Route 描述「什么样的请求」映射到「哪个上游」，并声明这条路由上的
// 横切策略（鉴权 / 限流 / 协议转换）；一个 Service 描述「一组节点 + 负载
// 均衡算法 + 熔断参数」。
//
// 生产中这份配置应从配置文件 / 配置中心（etcd、Nacos）加载并支持热更新；
// 本项目用 Go 字面量硬编码（见 config.go 的 Default），为的是让
// 「网关由哪些声明式规则组成」一眼可见。
package config

import "time"

// PathType 路由路径的匹配方式。
type PathType string

const (
	PathExact  PathType = "exact"  // 完全相等，优先级最高
	PathPrefix PathType = "prefix" // 前缀匹配，最长前缀优先
	PathRegex  PathType = "regex"  // 正则匹配，优先级最低（但排在普通前缀之前）
)

// Route 一条路由规则。
type Route struct {
	Name        string            // 规则名，出现在日志与指标里
	Host        string            // 空 = 任意；支持 *.example.com 通配
	Path        string            // 路径规则
	PathType    PathType          // 匹配方式
	Methods     []string          // 空 = 任意
	Headers     map[string]string // 必须全部匹配（值支持 * 通配）
	Upstream    string            // 上游服务名，见 Config.Services
	StripPrefix string            // 转发前剥掉的前缀
	Auth        AuthPolicy
	Limit       LimitPolicy
	Transcode   *TranscodePolicy // 非空表示走「HTTP→gRPC」协议转换
}

// AuthPolicy 这条路由的鉴权要求。
type AuthPolicy struct {
	Required bool
	Scheme   string // "jwt" | "apikey"
	Roles    []string
}

// LimitPolicy 这条路由的限流要求（令牌桶）。
type LimitPolicy struct {
	RatePerSec float64 // 每秒补充的令牌数；0 = 不限流
	Burst      int     // 桶容量（允许的突发）
}

// TranscodePolicy 协议转换：把 REST/JSON 映射成 gRPC 方法调用。
type TranscodePolicy struct {
	Service string // 例 "order.OrderService"
	Method  string // 例 "CreateOrder"
}

// Upstream 一个后端节点。
type Upstream struct {
	Addr    string // host:port
	Weight  int    // 加权轮询 / 一致性哈希的虚拟节点倍数
	GRPC    bool   // 该节点是否说 gRPC
	Backend int    // 仅演示用：后端自报名（用于观测负载均衡分布）
}

// Service 一个上游服务（一组节点 + 负载均衡算法 + 熔断参数）。
type Service struct {
	Name        string
	Balance     string // "round_robin" | "weighted" | "least_conn" | "consistent_hash"
	Upstreams   []Upstream
	Timeout     time.Duration
	Breaker     BreakerConfig
	HashKeyFrom string // consistent_hash 的 key 来源：header 名，空则用 client IP
}

// BreakerConfig 熔断器参数。
//
// 注意零值语义：FailRatio 为 0 时「任何失败率都达阈值」，即记录到 1 次失败
// 就会熔断；MinRequests 为 0 时不做样本量保护。New 只兜底 WindowSize /
// OpenFor / HalfOpenMax，因此不希望立刻熔断的服务必须显式给出这两个值。
type BreakerConfig struct {
	WindowSize  int           // 统计窗口内的请求数
	FailRatio   float64       // 失败率超过它就打开
	MinRequests int           // 样本不足时不打开
	OpenFor     time.Duration // 打开后多久进入半开
	HalfOpenMax int           // 半开状态放行的探测请求数
}
