// Package clientip 实现「沿可信代理链解析真实客户端 IP」。
//
// 为什么值得做：
//
//	网关部署在 LB / SLB / CDN 后面时，`r.RemoteAddr` 恒为**直接对端（代理）**的地址。
//	流水线 ③ 限流用 IP 做桶 key（internal/gateway/gateway.go:270 附近的
//	`net.SplitHostPort(r.RemoteAddr)`），于是所有匿名客户端坍缩到同一个桶里：
//	一个客户端打满，全网匿名流量一起 429 —— 审计 P1-3。
//	修法不是「去读 X-Forwarded-For」，而是**先判定直接对端是否可信**：
//	只有转发头确实由我们自己部署的代理写下时，它才有证据价值。
//
// 容易做错的地方（每一条都有对应测试钉住）：
//
//	① 无条件信任 X-Forwarded-For —— 这是最危险的写法。XFF 是客户端能自己塞的普通头，
//	   不看对端就采信 = 任何匿名攻击者都能伪造 IP 绕过限流。本包的硬规则：
//	   **对端不可信时，XFF / X-Real-IP 一个字节都不看**（Resolve 直接短路返回对端）。
//	② 从 XFF 最左侧取值 —— XFF 的语义是「每经过一跳就在右端追加」，最左端是**客户端自称**的值；
//	   正确方向是**从右往左**走：跳过可信跳，遇到的第一个不可信地址才是与最近可信代理直连的那一方。
//	③ 遇到解析不了的条目就整条链放弃 —— 攻击者只要在 XFF 里塞一个 `garbage` 就能把真实 IP 顶掉。
//	   正确做法是**跳过该条目继续往左**，非法值既不能采信也不该破坏其余证据。
//	④ 用 `net.ParseIP` 再拼字符串 —— 会把 `::ffff:1.2.3.4`、`[::1]` 这类形式带进返回值；
//	   本包统一走 `net/netip` 的 `Addr.Unmap()` + `Addr.String()`，输出唯一的规范形态。
//	⑤ 忘了 IPv6 的 `[::1]:1234` 形式 —— 端口剥离必须用 `net.SplitHostPort`（它会剥方括号），
//	   失败时才退化为「整个字符串就是主机」，不能自己按 ':' 切。
//
// 本机实测：`go test ./internal/clientip/ -count=1 -v` 全绿（含伪造 XFF 无效、非法条目跳过、
// IPv6 括号形式等边界）。本机 CGO_ENABLED=0，`-race` 不可用（AGENTS.md §3），并发安全由
// 「纯函数 + 不共享可变状态」保证，不依赖锁。
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrusted 把 CIDR 字符串列表（也允许裸 IP，如 "10.0.0.1"）解析成前缀集合。
//
// 裸 IP 按单机前缀处理：IPv4 → /32，IPv6 → /128（`Addr.BitLen()` 正好是这两个值）。
// CIDR 会做 `Masked()` 规范化（`10.0.0.1/8` → `10.0.0.0/8`），避免 Contains 语义被误读。
// 空白项被跳过（配置里常见的尾随逗号不该让网关起不来）；**真正写错的条目返回错误**，
// 因为「可信代理列表写错」属于安全配置错误，静默吞掉等于把 XFF 变成不可信来源却不自知。
func ParseTrusted(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for i, raw := range cidrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("clientip: 第 %d 个可信代理 %q 既不是 CIDR 也不是 IP: %w", i+1, raw, err)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// Resolve 返回尽力而为的客户端 IP（不带端口）。
//
// 取值顺序（每一步都以「对端可信」为前提，可信与否由 trusted 判定）：
//
//	① remoteAddr 不是可信代理 → 直接返回它的主机部分，**完全忽略** XFF / X-Real-IP，防伪造；
//	② 对端可信 → 取 X-Forwarded-For，**从最右往左**逐个判定：
//	   解析不了的条目跳过并继续往左；遇到第一个不在 trusted 里的地址即返回；
//	③ 整条 XFF 都是可信跳（且至少有一个可解析条目）→ 返回**最左那个**（理由见下）；
//	④ XFF 缺失、为空或全部不可解析 → 回落到 X-Real-IP（同样只在②成立时才会走到这里）；
//	⑤ 什么都拿不到 → 返回 remoteAddr 的主机部分（此时它本身就是那个可信代理）。
//
// 关于③「XFF 全是可信跳」的规则选择：取**最左（最早）那个**条目，而不是退回对端地址。
// 理由：能走到③说明客户端自身就落在可信网段里（例如内网办公网客户端直连 SLB，
// 而 SLB 与内网同属 `10.0.0.0/8` 这一条 trusted）。此时最左条目正是真实客户端，
// 且它在可信段内仍能区分不同客户端；若退回对端（SLB）地址，这些客户端又会坍缩成一个限流桶，
// 等于在「可信内网直连」这一常见拓扑下把 P1-3 原样保留。这与 nginx realip 模块的行为一致：
// 它同样从右往左扫，扫完所有可信跳后使用最左那个地址。
//
// remoteAddr 形如 "1.2.3.4:5678" 或 "[::1]:5678"；trusted 为空切片等价于「没有任何可信代理」。
func Resolve(remoteAddr string, h http.Header, trusted []netip.Prefix) string {
	host := hostOnly(strings.TrimSpace(remoteAddr))
	peer, ok := parseIP(host)
	if !ok {
		return host // 连对端都解析不了，只能原样返回
	}

	// ① 对端不可信：转发头一律不采信。这是本包的安全底线。
	if !trustedHas(trusted, peer) {
		return peer.String()
	}

	// ② 从 XFF 最右侧往左走，跳过可信跳，第一个不可信地址即真实客户端。
	chain := parseChain(h.Values("X-Forwarded-For"))
	for i := len(chain) - 1; i >= 0; i-- {
		if !trustedHas(trusted, chain[i]) {
			return chain[i].String()
		}
	}

	// ③ 整条链都在可信网段内：取最左那个（见函数注释）。
	if len(chain) > 0 {
		return chain[0].String()
	}

	// ④ 没有可用的 XFF：回落 X-Real-IP。
	if ip, ok := parseIP(hostOnly(strings.TrimSpace(h.Get("X-Real-IP")))); ok {
		return ip.String()
	}

	// ⑤ 兜底：对端自己（它本身是可信代理）。
	return peer.String()
}

// parseChain 把 X-Forwarded-For 拆成有序地址列表（左 → 右）。
// 支持多个同名头（语义上等价于按序逗号拼接），空白与非法条目被丢弃而不是中断解析。
func parseChain(values []string) []netip.Addr {
	var out []netip.Addr
	for _, v := range values {
		for _, seg := range strings.Split(v, ",") {
			ip, ok := parseIP(hostOnly(strings.TrimSpace(seg)))
			if !ok {
				continue // 非法条目跳过并继续往左，不能因此放弃整条链
			}
			out = append(out, ip)
		}
	}
	return out
}

// hostOnly 剥离端口。优先 net.SplitHostPort（它同时处理 "[::1]:1234" 的方括号），
// 失败时按「整个字符串就是主机」处理，并顺手去掉可能残留的方括号（"[::1]" 这种无端口写法）。
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
}

// parseIP 解析单个 IP，并统一 Unmap：IPv4-mapped IPv6（::ffff:1.2.3.4）归一到 IPv4，
// 否则它既不会被 IPv4 前缀 Contains 命中，返回值形态也不规范。
func parseIP(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// trustedHas 判断 IP 是否落在任一可信前缀内。
// trusted 为空 → 恒为 false，即「没有任何可信代理」，等价于完全不看转发头。
func trustedHas(trusted []netip.Prefix, ip netip.Addr) bool {
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
