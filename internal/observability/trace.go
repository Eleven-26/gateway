package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// W3C Trace Context。
//
// 为什么值得做：原来只有一个自定义的 `X-Trace-Id` —— 它能在自己的系统里串起来，
// 但**接不进任何标准链路追踪**（Jaeger/Tempo/各云厂商 APM 都认 W3C `traceparent`）。
// 网关是链路的中间一跳，正确做法是：
//
//	沿用上游传进来的 trace-id → **为本跳生成新的 span-id** → 把新的 traceparent 传给下游。
//
// 而不是把客户端给的 span-id 原样透传 —— 那会让下游以为"自己就是客户端那一跳"，
// 调用关系被压平成一条直线，跨服务排查时看不出网关这一跳的存在。
//
// 格式（W3C Trace Context Level 1）：
//
//	traceparent: 00-<32 hex trace-id>-<16 hex span-id>-<2 hex flags>
//	tracestate:  vendored key=value 列表 —— 不解析、原样透传（它是别人的私有数据）
//
// ⚠️ 校验必须严格，不要把自己看不懂的东西往下传：版本只支持 `00`（`ff` 非法）、
// trace-id/span-id 不能全 0、十六进制必须**小写**（规范如此，大写算非法）。
// 非法就**当作没传**，自己新起一条链路，而不是把非法值继续传下去。

// TraceInfo 一条链路上下文（本跳视角）。
type TraceInfo struct {
	TraceID    string // 32 hex
	SpanID     string // 16 hex，本跳（网关）的 span
	Flags      byte   // 采样等标志位
	TraceState string // 原样透传
	Incoming   bool   // true = trace-id 沿用了上游
}

// StartTrace 解析入站 traceparent（非法则忽略），退回到 X-Trace-Id，再没有就新生成；
// 无论如何都会为本跳生成一个新的 span-id。
func StartTrace(h http.Header) TraceInfo {
	info := TraceInfo{Flags: 1, TraceState: h.Get("tracestate")} // 默认 sampled=1
	if tp := h.Get("traceparent"); tp != "" {
		if tid, _, flags, ok := ParseTraceparent(tp); ok {
			info.TraceID, info.Flags, info.Incoming = tid, flags, true
		}
	}
	if info.TraceID == "" {
		// 兼容老的 X-Trace-Id（本项目的演示后端与旧调用方还在用它）
		if v := strings.ToLower(strings.TrimSpace(h.Get(TraceHeader))); isHex(v, 32) && !isAllZero(v) {
			info.TraceID = v
			info.Incoming = true
		}
	}
	if info.TraceID == "" {
		info.TraceID = randomHex(16)
	}
	info.SpanID = randomHex(8)
	h.Set(TraceHeader, info.TraceID) // 回写：日志、响应头与下游都看到同一个 trace-id
	return info
}

// Header 返回本跳要传给下游的 traceparent。
func (t TraceInfo) Header() string {
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + hexByte(t.Flags)
}

// ParseTraceparent 严格解析 W3C traceparent。返回 (traceID, spanID, flags, ok)。
//
// 严格的地方（都是规范要求，也是常见实现踩坑处）：版本只认 00、长度固定、
// 十六进制必须小写、trace-id 与 span-id 都不能全 0。
func ParseTraceparent(v string) (traceID, spanID string, flags byte, ok bool) {
	parts := strings.Split(v, "-")
	if len(parts) != 4 {
		return "", "", 0, false
	}
	version, tid, sid, fl := parts[0], parts[1], parts[2], parts[3]
	if version != "00" || len(tid) != 32 || len(sid) != 16 || len(fl) != 2 {
		return "", "", 0, false
	}
	if !isHex(tid, 32) || !isHex(sid, 16) || !isHex(fl, 2) {
		return "", "", 0, false
	}
	if isAllZero(tid) || isAllZero(sid) {
		return "", "", 0, false // 规范明确：全 0 视为非法
	}
	b, err := hex.DecodeString(fl)
	if err != nil {
		return "", "", 0, false
	}
	return tid, sid, b[0], true
}

// isHex 判断是否恰好 n 个**小写**十六进制字符。
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func isAllZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hexByte(b byte) string {
	const hexdigits = "0123456789abcdef"
	return string([]byte{hexdigits[b>>4], hexdigits[b&0x0f]})
}

type traceKey struct{}

// WithTrace 把链路信息放进 context，供下游（proxy.Director）写 traceparent。
// 用 context 是因为 ReverseProxy 会把出站请求 Clone 一份，Header 改了传不回来
// （同样的道理见 proxy.ErrFrom 的说明）。
func WithTrace(ctx context.Context, t TraceInfo) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom 取回链路信息。
func TraceFrom(ctx context.Context) (TraceInfo, bool) {
	t, ok := ctx.Value(traceKey{}).(TraceInfo)
	return t, ok
}
