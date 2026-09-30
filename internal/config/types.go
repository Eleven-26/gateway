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
//
// 字段带 `json` tag：这份结构同时也是**外部配置文件**（`GW_CONFIG` 指向的 JSON）的 schema，
// 命名要对外稳定（snake_case）—— 不能拿 Go 字段名当外部接口（审计 C1）。
type Route struct {
	Name        string            `json:"name"`
	Host        string            `json:"host"` // 空 = 任意；支持 *.example.com 通配
	Path        string            `json:"path"`
	PathType    PathType          `json:"path_type"`
	Methods     []string          `json:"methods,omitempty"` // 空 = 任意
	Headers     map[string]string `json:"headers,omitempty"` // 必须全部匹配（值支持 * 通配）
	Upstream    string            `json:"upstream"`
	StripPrefix string            `json:"strip_prefix,omitempty"`
	Timeout     Duration          `json:"timeout,omitempty"` // 每路由超时；0 = 用 Service.Timeout（审计 C3）
	Retry       *RetryPolicy      `json:"retry,omitempty"`   // 每路由重试；nil = 不重试
	Auth        AuthPolicy        `json:"auth,omitempty"`
	Limit       LimitPolicy       `json:"limit,omitempty"`
	Transcode   *TranscodePolicy  `json:"transcode,omitempty"` // 非空 = 走「HTTP→gRPC」协议转换
}

// RetryPolicy 每路由重试策略（审计 C3）。
//
// ⚠️ 重试是**流量放大器**：上游已经过载时，无脑重试会把压力放大 (Attempts+1) 倍。
// 所以这里刻意只做最安全的那一类重试，并把边界写清楚：
//
//  1. **只重试「还没写出任何字节」的失败**（连接被拒/重置、拨号超时、TLS 握手失败）。
//     一旦上游已经开始回响应，就绝不换节点重来 —— 那需要把响应缓冲起来（内存代价，
//     还会破坏流式/SSE/协议升级），而且可能把已经产生副作用的操作做两次。
//  2. **5xx 不重试**：上游明确答了 500，说明请求已经到达并被处理过，重试很可能重复执行。
//     "上游 5xx 就换节点"应当在上游侧解决，而不是让网关把写请求再发一遍。
//  3. **默认只重试幂等方法**（GET/HEAD/PUT/DELETE/OPTIONS/TRACE，RFC 7231），
//     且请求体必须为空（重放需要缓冲请求体）。要重试写操作必须显式打开
//     `AllowNonIdempotent` **并自行保证幂等键**。
//
// 时间上界：每次尝试都用完整的路由/服务超时，最坏耗时 ≈ (Attempts+1) × 超时 ——
// `config.MaxServiceTimeout()` 已把重试算进去（它决定 `http.Server.WriteTimeout`）。
type RetryPolicy struct {
	Attempts           int      `json:"attempts"`                       // 最多**再**试几次（1 = 最多两次上游请求）
	PerTryTimeout      Duration `json:"per_try_timeout,omitempty"`      // 单次尝试超时；0 = 用路由/服务超时
	AllowNonIdempotent bool     `json:"allow_non_idempotent,omitempty"` // ⚠️ 允许重试 POST 等非幂等方法
}

// AuthPolicy 这条路由的鉴权要求。
type AuthPolicy struct {
	Required bool     `json:"required"`
	Scheme   string   `json:"scheme,omitempty"` // "jwt" | "apikey"
	Roles    []string `json:"roles,omitempty"`
}

// LimitPolicy 这条路由的限流要求（令牌桶）。
type LimitPolicy struct {
	RatePerSec float64 `json:"rate_per_sec,omitempty"` // 每秒补充的令牌数；0 = 不限流
	Burst      int     `json:"burst,omitempty"`        // 桶容量（允许的突发）
}

// TranscodePolicy 协议转换：把 REST/JSON 映射成 gRPC 方法调用。
type TranscodePolicy struct {
	Service string `json:"service"` // 例 "order.OrderService"
	Method  string `json:"method"`  // 例 "CreateOrder"
}

// Upstream 一个后端节点。
type Upstream struct {
	Addr    string     `json:"addr"`
	Scheme  string     `json:"scheme,omitempty"`  // "" | "http"（默认）| "https"；https 时按 TLS 参数连接
	TLS     *TLSConfig `json:"tls,omitempty"`     // 仅 scheme=https 时有意义
	Weight  int        `json:"weight,omitempty"`  // 加权轮询 / 一致性哈希的虚拟节点倍数
	GRPC    bool       `json:"grpc,omitempty"`    // 该节点是否说 gRPC
	Backend int        `json:"backend,omitempty"` // 仅演示用：后端自报名（用于观测负载均衡分布）
}

// TLSConfig 上游 TLS 参数（审计 C2）。零值 = 用系统根证书 + 校验主机名，这是最安全的默认。
//
// 为什么值得做：原实现把目标 URL 硬编码成 `http://`，上游只能是明文 HTTP —— 内网服务间 TLS、
// 以及要求 mTLS 的第三方接口（支付/风控常见）都接不了。
//
// ⚠️ 三个容易做错的地方：
//   - `InsecureSkipVerify: true` 关掉的是**证书链与主机名校验**，等于给中间人开门，只应临时调试用；
//   - `ServerName`（SNI）不填时用地址里的主机名：用 IP 直连、而证书签的是域名时**必须**显式给，
//     否则握手会因主机名不匹配失败（「本地通、线上不通」最常见的原因）；
//   - 证书文件在**启动期**就会被读出来解析（`config.Validate`），路径写错/证书私钥不匹配
//     会在启动时失败，而不是等第一个请求打到这个节点才报握手错。
type TLSConfig struct {
	CAFile             string `json:"ca_file,omitempty"`              // 自定义根证书（PEM）；空 = 系统根证书
	ServerName         string `json:"server_name,omitempty"`          // SNI（证书上的域名）
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"` // ⚠️ 仅调试用
	ClientCertFile     string `json:"client_cert_file,omitempty"`     // mTLS 客户端证书
	ClientKeyFile      string `json:"client_key_file,omitempty"`      // mTLS 客户端私钥
}

// Service 一个上游服务（一组节点 + 负载均衡算法 + 熔断参数）。
type Service struct {
	Name        string
	Balance     string // "round_robin" | "weighted" | "least_conn" | "consistent_hash"
	Upstreams   []Upstream
	Timeout     time.Duration
	Breaker     BreakerConfig
	HashKeyFrom string         // consistent_hash 的 key 来源：header 名，空则用 client IP
	Ejection    EjectionConfig // 节点级被动摘除；FailThreshold=0 表示不启用
}

// EjectionConfig 节点级被动摘除（审计 P1-2）。FailThreshold 为 0 表示不启用。
//
// 与 Service.Breaker 的分工：熔断的粒度是**服务**（整个服务快速失败），摘除的粒度是**节点**
// （只摘掉连续失败的那个地址，其余节点继续服务）。两者互补 —— 节点摘除处理「单点挂了」，
// 熔断处理「整片都不行」。没有节点摘除时，一个坏节点会一直吃掉 1/N 流量，直到整个服务被熔断。
type EjectionConfig struct {
	FailThreshold int           // 连续失败达到该次数就摘除该节点（成功一次即清零）
	Cooldown      time.Duration // 摘除时长；到期后放行一个探测请求，成功才回列
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
