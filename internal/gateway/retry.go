package gateway

import (
	"net/http"

	"gwlab/internal/config"
)

// retryPlan 一次请求的重试计划。
//
// `deferErr` 决定是否请 proxy「先把上游错误记下来、别写响应」：
// 只有真的可能重试时才推迟错误响应，否则保持原行为（ErrorHandler 立刻写 502）——
// 这样没配重试的路由，行为与改造前**完全一致**（回归风险最小）。
type retryPlan struct {
	attempts int    // 总尝试次数（1 = 不重试）
	deferErr bool   // 是否让 proxy 把错误响应交给本层收尾（重试期间不写 502）
	reason   string // 没重试的原因（只用于日志/调试，不参与判定）
}

// idempotentMethod 判断方法是否幂等（RFC 7231 §4.2.2），决定默认能不能重试。
func idempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete,
		http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// planRetry 计算实际尝试次数。
//
// 三重闸门（任何一条不满足就退回"只试一次"）：
//  1. 路由配了 Retry 且 Attempts > 0；
//  2. 方法幂等，或显式打开 AllowNonIdempotent；
//  3. 请求体为空 —— 重放请求要靠缓冲 body，这里刻意**不做**：带体的请求通常是写操作，
//     重试它们的风险（重复下单/重复扣款）远大于收益。
//
// `ContentLength == 0` 同时排除了 chunked（值为 -1）的情况，这正是我们要的。
func planRetry(rt *config.Route, r *http.Request) retryPlan {
	plan := retryPlan{attempts: 1}
	if rt.Retry == nil || rt.Retry.Attempts <= 0 {
		return plan
	}
	if !idempotentMethod(r.Method) && !rt.Retry.AllowNonIdempotent {
		plan.reason = "非幂等方法，未开启 allow_non_idempotent"
		return plan
	}
	if r.ContentLength != 0 {
		plan.reason = "请求体非空（重放需要缓冲请求体，本实现刻意不做）"
		return plan
	}
	plan.attempts = 1 + rt.Retry.Attempts
	plan.deferErr = true
	return plan
}
