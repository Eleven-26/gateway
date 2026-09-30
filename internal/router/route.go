// Package router 实现 ① 路由匹配：Host + Path（exact / prefix / regex）+
// Method + Header。
//
// 匹配优先级刻意做成和 Nginx location 同构：
//
//	精确(exact) > 正则(regex) > 最长前缀(prefix)
//
// 这样从 Nginx 迁过来时行为不会突变 —— ⚠️ 注意「正则优先于普通前缀」这一条，
// 顺序写反会导致兜底的 location / 抢走本该由正则处理的请求（本机实测踩过）。
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
}

// Router 持有编译后的路由表。
type Router struct {
	routes []compiledRoute
}

// New 编译路由表 —— 正则在这里一次性编译，匹配阶段不再解析。
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
		r.routes = append(r.routes, c)
	}
	return r, nil
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
		if !headersMatch(c.Headers, req) {
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

// headersMatch 要求规则里的每个头都存在；值支持 * 通配，大小写不敏感。
func headersMatch(want map[string]string, req *http.Request) bool {
	for k, v := range want {
		got := req.Header.Get(k)
		if got == "" {
			return false
		}
		if v == "*" {
			continue
		}
		if strings.Contains(v, "*") {
			if ok, _ := regexp.MatchString("^"+strings.ReplaceAll(regexp.QuoteMeta(v), `\*`, ".*")+"$", got); !ok {
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
