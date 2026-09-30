package observability

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const (
	validTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	validSpan  = "00f067aa0ba902b7"
)

// TestParseTraceparent 严格性用例：这些都是规范明确**非法**的形式，
// 实现宽松一点就会出现"把垃圾透传下去"或"链路对不上"的怪问题。
func TestParseTraceparent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"合法", "00-" + validTrace + "-" + validSpan + "-01", true},
		{"未采样标志", "00-" + validTrace + "-" + validSpan + "-00", true},
		{"版本非 00", "01-" + validTrace + "-" + validSpan + "-01", false},
		{"版本 ff", "ff-" + validTrace + "-" + validSpan + "-01", false},
		{"trace-id 太短", "00-" + validTrace[:30] + "-" + validSpan + "-01", false},
		{"span-id 太长", "00-" + validTrace + "-" + validSpan + "aa" + "-01", false},
		{"flags 只有 1 位", "00-" + validTrace + "-" + validSpan + "-1", false},
		{"大写十六进制", "00-" + strings.ToUpper(validTrace) + "-" + validSpan + "-01", false},
		{"trace-id 全 0", "00-" + strings.Repeat("0", 32) + "-" + validSpan + "-01", false},
		{"span-id 全 0", "00-" + validTrace + "-" + strings.Repeat("0", 16) + "-01", false},
		{"字段数不对", "00-" + validTrace + "-" + validSpan, false},
		{"非十六进制", "00-" + strings.Repeat("z", 32) + "-" + validSpan + "-01", false},
		{"空串", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tid, sid, _, ok := ParseTraceparent(c.in)
			if ok != c.ok {
				t.Fatalf("解析 %q 的 ok 应为 %v，实际 %v", c.in, c.ok, ok)
			}
			if !ok {
				return
			}
			if tid != validTrace || sid != validSpan {
				t.Fatalf("解析结果不对：%s / %s", tid, sid)
			}
		})
	}
}

// TestStartTraceKeepsTraceIDMakesNewSpan 是 C5 的核心语义：
// 网关是链路中间一跳 —— 沿用上游 trace-id，但为本跳生成**新的** span-id。
// 若把客户端的 span-id 原样透传，下游会以为"自己就是客户端那一跳"，调用关系被压平。
func TestStartTraceKeepsTraceIDMakesNewSpan(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "00-"+validTrace+"-"+validSpan+"-01")
	h.Set("tracestate", "vendor=abc")

	tr := StartTrace(h)
	if tr.TraceID != validTrace {
		t.Fatalf("应当沿用上游 trace-id，实际 %s", tr.TraceID)
	}
	if !tr.Incoming {
		t.Fatal("应当标记为沿用上游")
	}
	if tr.SpanID == validSpan {
		t.Fatal("必须为本跳生成新的 span-id（不能复用上游的）")
	}
	if !isHex(tr.SpanID, 16) {
		t.Fatalf("span-id 应当是 16 位小写十六进制，实际 %q", tr.SpanID)
	}
	if tr.TraceState != "vendor=abc" {
		t.Fatalf("tracestate 应当原样保留，实际 %q", tr.TraceState)
	}
	if h.Get(TraceHeader) != validTrace {
		t.Fatalf("X-Trace-Id 应当回写成 trace-id，实际 %q", h.Get(TraceHeader))
	}
	if got := tr.Header(); got != "00-"+validTrace+"-"+tr.SpanID+"-01" {
		t.Fatalf("出站 traceparent 格式不对：%s", got)
	}
}

// TestStartTraceFallbacks：非法 traceparent 要"当作没传"（自己新起一条链路），
// 而不是把非法值继续传下去；没有 traceparent 时兼容 X-Trace-Id；都没有就生成。
func TestStartTraceFallbacks(t *testing.T) {
	t.Run("非法 traceparent 被忽略", func(t *testing.T) {
		h := http.Header{}
		h.Set("traceparent", "00-"+strings.Repeat("0", 32)+"-"+validSpan+"-01")
		tr := StartTrace(h)
		if tr.Incoming {
			t.Fatal("非法值不该被当成有效链路")
		}
		if !isHex(tr.TraceID, 32) || isAllZero(tr.TraceID) {
			t.Fatalf("应当新生成 trace-id，实际 %q", tr.TraceID)
		}
	})

	t.Run("回退到 X-Trace-Id", func(t *testing.T) {
		h := http.Header{}
		h.Set(TraceHeader, validTrace)
		tr := StartTrace(h)
		if tr.TraceID != validTrace || !tr.Incoming {
			t.Fatalf("应当沿用 X-Trace-Id，实际 %q incoming=%v", tr.TraceID, tr.Incoming)
		}
	})

	t.Run("非法 X-Trace-Id 也忽略", func(t *testing.T) {
		h := http.Header{}
		h.Set(TraceHeader, "not-a-trace")
		tr := StartTrace(h)
		if tr.TraceID == "not-a-trace" || !isHex(tr.TraceID, 32) {
			t.Fatalf("非法 X-Trace-Id 应当被忽略并新生成，实际 %q", tr.TraceID)
		}
	})

	t.Run("都没有就生成", func(t *testing.T) {
		tr := StartTrace(http.Header{})
		if !isHex(tr.TraceID, 32) || !isHex(tr.SpanID, 16) {
			t.Fatalf("应当生成合法 id，实际 %q / %q", tr.TraceID, tr.SpanID)
		}
		if tr.Flags != 1 {
			t.Fatalf("默认应当标记为 sampled，实际 flags=%d", tr.Flags)
		}
	})
}

// TestTraceFromContext：链路信息要能通过 context 传到 proxy（ReverseProxy 会 Clone 请求，
// Header 改了传不回来，所以必须走 context）。
func TestTraceFromContext(t *testing.T) {
	tr := TraceInfo{TraceID: validTrace, SpanID: validSpan, Flags: 1}
	ctx := WithTrace(context.Background(), tr)
	got, ok := TraceFrom(ctx)
	if !ok || got.TraceID != validTrace || got.SpanID != validSpan {
		t.Fatalf("context 传递失败：%+v ok=%v", got, ok)
	}
	if _, ok := TraceFrom(context.Background()); ok {
		t.Fatal("没有链路信息的 context 不该返回 ok")
	}
}
