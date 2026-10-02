package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// selfService 是「网关自答」的伪服务名：它不需要上游节点，也不参与选节点与熔断。
const selfService = "self"

// Validate 在启动时做配置自洽性检查 —— 让错误配置**启动即失败**，而不是等某个请求来了才 500。
//
// 为什么值得做：以前 `Route.Upstream` 写错服务名，要等真有请求打进来才返回 500 `bad_config`
// （`internal/gateway/gateway.go`）；`BreakerConfig.MinRequests > WindowSize` 会让熔断器永不打开
// 且毫无提示；缺少 `GRPC: true` 节点却在路由上声明了 `Transcode`，会一直连到 HTTP 端口上去。
// 这类错误的代价是「上线才发现」，而检查成本只有一次遍历。
//
// 容易做错的地方：
//   - `self` 是伪服务（网关自答），允许没有上游节点；
//   - Host 为空按「任意」处理（与 router 的语义一致），这里只做**归一化**、不报错；
//   - 只查能确定的矛盾（重名、匹配条件完全相同、上游缺失），不做「谁覆盖谁」这类语义推断；
//   - 这里**不复用** `internal/clientip` 的解析：config 是依赖树叶子，不能 import 任何内部包，
//     所以可信代理 CIDR 的校验在下面自己写一遍。
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config 为 nil")
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("ListenAddr 为空")
	}
	if len(c.Routes) == 0 {
		return errors.New("没有任何路由")
	}
	if len(c.Services) == 0 {
		return errors.New("没有任何上游服务")
	}
	if _, err := parseTrusted(c.TrustedProxies); err != nil {
		return fmt.Errorf("TrustedProxies 配置有误: %w", err)
	}
	// 限流后端：URL 写错就启动失败，别等第一个被限流的请求才暴露
	if u := strings.TrimSpace(c.SharedState.URL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("SharedState.URL=%q 不合法（需要 http(s)://host[:port]/path 形式）", c.SharedState.URL)
		}
		if c.SharedState.Timeout < 0 {
			return fmt.Errorf("SharedState.Timeout 不能为负")
		}
		if c.SharedState.SyncInterval < 0 {
			return fmt.Errorf("SharedState.SyncInterval 不能为负")
		}
	}
	// 负载保护
	if c.Overload.MaxInflight < 0 || c.Overload.MaxQueue < 0 {
		return fmt.Errorf("Overload 的 MaxInflight/MaxQueue 不能为负")
	}
	if c.Overload.MaxQueue > 0 && c.Overload.MaxInflight <= 0 {
		return fmt.Errorf("Overload.MaxQueue=%d 但没设 MaxInflight：不限制并发就谈不上排队", c.Overload.MaxQueue)
	}
	if c.Overload.QueueTimeout < 0 || c.Overload.RetryAfter < 0 {
		return fmt.Errorf("Overload 的 QueueTimeout/RetryAfter 不能为负")
	}
	if err := c.validateServices(); err != nil {
		return err
	}
	return c.validateRoutes()
}

func (c *Config) validateServices() error {
	for name, s := range c.Services {
		if s == nil {
			return fmt.Errorf("服务 %q 是 nil（检查 defaultServices 的字面量）", name)
		}
		if s.Name != name {
			return fmt.Errorf("服务 %q 的 Name 字段是 %q，两者必须一致（缓存以 Name 为 key）", name, s.Name)
		}
		if s.Timeout <= 0 {
			return fmt.Errorf("服务 %q 的 Timeout 必须 > 0（它是上游往返的唯一超时来源）", name)
		}
		if len(s.Upstreams) == 0 && name != selfService {
			return fmt.Errorf("服务 %q 没有任何上游节点", name)
		}
		seen := map[string]bool{}
		for i, u := range s.Upstreams {
			if strings.TrimSpace(u.Addr) == "" {
				return fmt.Errorf("服务 %q 的第 %d 个节点地址为空", name, i+1)
			}
			if seen[u.Addr] {
				return fmt.Errorf("服务 %q 的节点 %s 重复", name, u.Addr)
			}
			seen[u.Addr] = true
			if _, _, err := net.SplitHostPort(u.Addr); err != nil {
				return fmt.Errorf("服务 %q 的节点 %q 不是 host:port 形式: %w", name, u.Addr, err)
			}
			if u.Weight < 0 {
				return fmt.Errorf("服务 %q 的节点 %s 权重为负", name, u.Addr)
			}
			// scheme / TLS：https 时在**启动期**就把证书文件读出来解析 ——
			// 路径写错、证书与私钥不匹配都让进程起不来，而不是等第一个请求报握手错。
			switch u.SchemeOrDefault() {
			case "http":
				if u.TLS != nil {
					return fmt.Errorf("服务 %q 的节点 %s 配了 tls，但 scheme 不是 https", name, u.Addr)
				}
			case "https":
				if _, err := u.TLSClientConfig(); err != nil {
					return fmt.Errorf("服务 %q 的节点 %s 的 TLS 配置有误: %w", name, u.Addr, err)
				}
			default:
				return fmt.Errorf("服务 %q 的节点 %s 的 scheme=%q 不合法（只能 http / https）",
					name, u.Addr, u.Scheme)
			}
		}

		b := s.Breaker
		if b.FailRatio < 0 || b.FailRatio > 1 {
			return fmt.Errorf("服务 %q 的 Breaker.FailRatio=%.3f 必须落在 [0,1]", name, b.FailRatio)
		}
		if b.WindowSize < 0 || b.MinRequests < 0 || b.HalfOpenMax < 0 || b.OpenFor < 0 {
			return fmt.Errorf("服务 %q 的 Breaker 参数不能为负", name)
		}
		if b.WindowSize > 0 && b.MinRequests > b.WindowSize {
			return fmt.Errorf("服务 %q 的 Breaker.MinRequests(%d) 大于 WindowSize(%d)，熔断器永远不会打开",
				name, b.MinRequests, b.WindowSize)
		}

		e := s.Ejection
		if e.FailThreshold < 0 || e.Cooldown < 0 {
			return fmt.Errorf("服务 %q 的 Ejection 参数不能为负", name)
		}
		if e.FailThreshold > 0 && e.Cooldown <= 0 {
			return fmt.Errorf("服务 %q 配了 Ejection.FailThreshold 但 Cooldown<=0：节点被摘除后就再也回不来", name)
		}
	}
	return nil
}

func (c *Config) validateRoutes() error {
	seenName := map[string]bool{}
	seenMatch := map[string]string{}

	for i := range c.Routes {
		r := &c.Routes[i]
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("第 %d 条路由缺少 Name（它会进日志与指标）", i+1)
		}
		where := fmt.Sprintf("路由 %s", r.Name)
		if seenName[r.Name] {
			return fmt.Errorf("路由名重复: %s", r.Name)
		}
		seenName[r.Name] = true

		if r.Host == "" {
			r.Host = "*" // 归一化：与 router 的「空 = 任意」等价，写显式值便于启动横幅与排查
		}
		switch r.PathType {
		case PathExact, PathPrefix, PathRegex:
		default:
			return fmt.Errorf("%s 的 PathType=%q 不合法（只能是 exact / prefix / regex）", where, r.PathType)
		}
		if r.Path == "" {
			return fmt.Errorf("%s 的 Path 为空", where)
		}
		if r.PathType == PathRegex {
			if _, err := regexp.Compile(r.Path); err != nil {
				return fmt.Errorf("%s 的正则 %q 编译失败: %w", where, r.Path, err)
			}
		}
		if r.PathType == PathPrefix && !strings.HasPrefix(r.Path, "/") {
			return fmt.Errorf("%s 的前缀必须以 / 开头", where)
		}
		if r.StripPrefix != "" && !strings.HasPrefix(r.StripPrefix, "/") {
			return fmt.Errorf("%s 的 StripPrefix 必须以 / 开头", where)
		}
		if r.Limit.RatePerSec < 0 || r.Limit.Burst < 0 {
			return fmt.Errorf("%s 的 Limit 不能为负", where)
		}
		if r.Limit.RatePerSec > 0 && r.Limit.Burst == 0 {
			return fmt.Errorf("%s 配了 RatePerSec 但 Burst=0：桶容量为 0 会拒绝所有请求", where)
		}
		// 每路由超时与重试
		if r.Timeout < 0 {
			return fmt.Errorf("%s 的 Timeout 不能为负（0 = 用 Service.Timeout）", where)
		}
		if rp := r.Retry; rp != nil {
			const maxAttempts = 5
			if rp.Attempts < 0 {
				return fmt.Errorf("%s 的 Retry.Attempts 不能为负", where)
			}
			if rp.Attempts > maxAttempts {
				return fmt.Errorf("%s 的 Retry.Attempts=%d 过大（上限 %d）：重试是流量放大器，"+
					"上游过载时会把压力再翻几倍", where, rp.Attempts, maxAttempts)
			}
			if rp.PerTryTimeout < 0 {
				return fmt.Errorf("%s 的 Retry.PerTryTimeout 不能为负", where)
			}
			if rp.Attempts > 0 && rp.PerTryTimeout > 0 && time.Duration(rp.PerTryTimeout) < 10*time.Millisecond {
				return fmt.Errorf("%s 的 Retry.PerTryTimeout=%s 太短（<10ms），几乎必然全部超时",
					where, time.Duration(rp.PerTryTimeout))
			}
		}

		if r.Upstream == selfService {
			continue // 伪服务：不查上游
		}
		svc, ok := c.Services[r.Upstream]
		if !ok || svc == nil {
			return fmt.Errorf("%s 引用了不存在的上游服务 %q", where, r.Upstream)
		}
		if r.Transcode != nil {
			if r.Transcode.Service == "" || r.Transcode.Method == "" {
				return fmt.Errorf("%s 的 Transcode 必须同时给出 Service 与 Method", where)
			}
			hasGRPC := false
			for _, u := range svc.Upstreams {
				if u.GRPC {
					hasGRPC = true
					break
				}
			}
			if !hasGRPC {
				return fmt.Errorf("%s 声明了 Transcode，但上游服务 %q 没有 GRPC:true 的节点（会连到 HTTP 端口）",
					where, svc.Name)
			}
		}

		key := r.Host + "|" + string(r.PathType) + "|" + r.Path
		if prev, dup := seenMatch[key]; dup {
			return fmt.Errorf("%s 与路由 %s 的匹配条件完全相同（%s %s）：命中顺序会不确定",
				where, prev, r.PathType, r.Path)
		}
		seenMatch[key] = r.Name
	}
	return nil
}

// parseTrusted 解析可信代理列表：既接受 CIDR（"10.0.0.0/8"），也接受裸 IP（按 /32 或 /128 处理）。
func parseTrusted(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, raw := range entries {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("无法解析 CIDR %q: %w", raw, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("无法解析 IP %q（需要 CIDR 或裸 IP）: %w", raw, err)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
