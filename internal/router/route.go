// Package router 实现 ① 路由匹配：Host + Path（exact / prefix / regex）+
// Method + Header。
//
// 匹配优先级刻意做成和 Nginx location 同构：
//
//	精确(exact) > 正则(regex) > 最长前缀(prefix)
//
// 这样从 Nginx 迁过来时行为不会突变 —— ⚠️ 注意「正则优先于普通前缀」这一条，
// 顺序写反会导致兜底的 location / 抢走本该由正则处理的请求（本机实测踩过）。
//
// 路径正则和头值正则都在 New 里预编译（头值部分对应审计 P1-5）：原先
// headersMatch 每收到一个含 * 的头值就调用一次
// regexp.MatchString("^" + strings.ReplaceAll(regexp.QuoteMeta(v), `\*`, ".*") + "$", got)，
// 等于每个请求编译一次正则并分配内存；现在编译结果挂在 compiledRoute.headerRes 上，
// 匹配阶段只做一次 Regexp.MatchString。
//
// 容易做错的地方：
//   - 「正则优先于普通前缀」写反了，兜底的前缀 location 会抢走本该走正则的请求；
//   - 头值通配是大小写敏感的（QuoteMeta 之后只把 \* 换成 .*，再首尾加锚点），
//     而不含 * 的头值走 strings.EqualFold（大小写不敏感）—— 顺手给通配正则加 (?i)
//     就把语义改掉了，P1-5 是纯性能重构，行为必须与改动前逐字一致；
//   - 规则里的每个头都必须存在，req.Header.Get(k) == "" 一律视为不匹配
//     （所以头值不能配成空串）；
//   - 预编译只做一次，别在 headersMatch 里回退成「没查到就现编」——
//     那等于把审计要修的问题又搬了回来。
package router

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"gwlab/internal/config"
)

// 匹配得分：数值越大优先级越高。
const (
	scoreExact  = 3
	scoreRegex  = 2
	scorePrefix = 1
)

type compiledRoute struct {
	config.Route
	re *regexp.Regexp

	// headerRes 是「带通配符的头值」预编译好的正则，key 为头名（审计 P1-5）。
	// 只有值里含 * 且不等于 "*" 的头才会出现在这里；nil 表示该路由没有这类头。
	headerRes map[string]*regexp.Regexp
}

// Router 持有编译后的路由表。
type Router struct {
	routes []compiledRoute
}

// New 编译路由表 —— 正则在这里一次性编译，匹配阶段不再解析。
//
// 两条都要编译：PathType == regex 的路径正则，以及含通配符的头值正则（审计 P1-5）。
// 任何一条编译失败都让 New 返回错误 —— 路由表是启动期加载的，编译不过属于配置错误，
// 不该等到第一个请求打进来才暴露（也是原来每请求 regexp.MatchString 静默吞错的地方）。
func New(rs []config.Route) (*Router, error) {
	r := &Router{}
	for _, rt := range rs {
		c := compiledRoute{Route: rt}
		if rt.PathType == config.PathRegex {
			re, err := regexp.Compile(rt.Path)
			if err != nil {
				return nil, fmt.Errorf("路由 %s 的正则 %q 编译失败: %w", rt.Name, rt.Path, err)
			}
			c.re = re
		}
		for k, v := range rt.Headers {
			// "*" 走「有这个头就算过」，纯字面值走 EqualFold，两者都不需要正则。
			if v == "*" || !strings.Contains(v, "*") {
				continue
			}
			re, err := regexp.Compile(headerValuePattern(v))
			if err != nil {
				return nil, fmt.Errorf("路由 %s 的头 %s 的通配值 %q 编译失败: %w", rt.Name, k, v, err)
			}
			if c.headerRes == nil {
				c.headerRes = make(map[string]*regexp.Regexp, len(rt.Headers))
			}
			c.headerRes[k] = re
		}
		r.routes = append(r.routes, c)
	}
	return r, nil
}

// headerValuePattern 把一个含 * 的头值翻译成锚定正则 —— 语义与 P1-5 之前逐字一致：
// QuoteMeta 之后只把 `\*` 换回 `.*`，再首尾加 ^ / $。大小写敏感，不要加 (?i)。
//
// 只在这里出现一次，编译由 New 调用；headerRes 存的正是它的结果。
func headerValuePattern(v string) string {
	return "^" + strings.ReplaceAll(regexp.QuoteMeta(v), `\*`, ".*") + "$"
}

// MatchResult 带上「为什么命中」——写进访问日志后，排查路由问题基本不用猜。
type MatchResult struct {
	Route  *config.Route
	Reason string
	Groups []string // 正则捕获组
}

// Match 返回优先级最高的一条路由。
func (r *Router) Match(req *http.Request) (*MatchResult, bool) {
	var best *MatchResult
	bestScore := -1
	bestLen := -1

	for i := range r.routes {
		c := &r.routes[i]
		if c.Host != "" && c.Host != "*" && !hostMatch(c.Host, req.Host) {
			continue
		}
		if len(c.Methods) > 0 && !contains(c.Methods, req.Method) {
			continue
		}
		if !headersMatch(c, req) {
			continue
		}

		path := req.URL.Path
		switch c.PathType {
		case config.PathExact:
			if path == c.Path {
				if scoreExact > bestScore {
					best = &MatchResult{Route: &c.Route, Reason: "exact " + c.Path}
					bestScore, bestLen = scoreExact, len(c.Path)
				}
			}
		case config.PathRegex:
			// ⚠️ 正则必须排在「普通前缀」前面 —— 与 Nginx 一致（`~` 优先于无修饰前缀），
			// 否则 ^/api/users/\d+$ 会被兜底的 location / 抢走。
			if m := c.re.FindStringSubmatch(path); m != nil {
				if scoreRegex > bestScore {
					best = &MatchResult{Route: &c.Route, Reason: "regex " + c.Path, Groups: m[1:]}
					bestScore, bestLen = scoreRegex, len(c.Path)
				}
			}
		case config.PathPrefix:
			if strings.HasPrefix(path, c.Path) {
				// 同级前缀取最长的那个 —— 与 Nginx 一致
				if scorePrefix > bestScore || (scorePrefix == bestScore && len(c.Path) > bestLen) {
					best = &MatchResult{Route: &c.Route, Reason: "prefix " + c.Path}
					bestScore, bestLen = scorePrefix, len(c.Path)
				}
			}
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// hostMatch 支持 *.example.com 通配，并忽略端口。
func hostMatch(rule, host string) bool {
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	if strings.HasPrefix(rule, "*.") {
		return strings.HasSuffix(host, rule[1:])
	}
	return strings.EqualFold(rule, host)
}

// headersMatch 要求规则里的每个头都存在；值支持 * 通配。
//
// 三种取值，语义与改动前完全一致：
//   - "*"：只要头存在（非空）就算过；
//   - 含 *（不是单个 "*"）：走 c.headerRes[k] 里预编译好的锚定正则，大小写敏感 ——
//     审计 P1-5 只把「每请求现编」换成「New 里预编」，语义不动；
//   - 其它：strings.EqualFold，大小写不敏感。
func headersMatch(c *compiledRoute, req *http.Request) bool {
	for k, v := range c.Headers {
		got := req.Header.Get(k)
		if got == "" {
			return false
		}
		if v == "*" {
			continue
		}
		if strings.Contains(v, "*") {
			// New 已为每个含 * 的值备好正则；取不到说明路由不是 New 编译出来的
			// （例如测试里手搓的 compiledRoute），按「不匹配」处理，绝不回退到现编。
			re := c.headerRes[k]
			if re == nil {
				return false
			}
			if !re.MatchString(got) {
				return false
			}
			continue
		}
		if !strings.EqualFold(got, v) {
			return false
		}
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
