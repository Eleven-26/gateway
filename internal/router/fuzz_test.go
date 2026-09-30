package router

import (
	"net/http"
	"net/url"
	"testing"

	"gwlab/internal/config"
)

// fuzzRoutes 覆盖三种 PathType、Host 通配与头通配（含通配头值），
// 让 fuzz 能同时踩到精确/正则/前缀与 headersMatch 的翻译逻辑。
func fuzzRoutes() []config.Route {
	return []config.Route{
		{Name: "exact", Host: "api.example.com", Path: "/api/order/list", PathType: config.PathExact, Methods: []string{"GET"}, Upstream: "self"},
		{Name: "regex", Host: "*", Path: `^/api/users/\d+$`, PathType: config.PathRegex, Upstream: "self"},
		{Name: "prefix", Host: "*", Path: "/api/", PathType: config.PathPrefix, Upstream: "self"},
		{Name: "wild-host", Host: "*.example.com", Path: "/x", PathType: config.PathExact, Upstream: "self"},
		{Name: "headers", Host: "*", Path: "/h", PathType: config.PathExact,
			Headers: map[string]string{"X-Client": "mobile*", "X-Env": "*", "X-Fixed": "v1"}, Upstream: "self"},
		{Name: "root", Host: "*", Path: "/", PathType: config.PathPrefix, Upstream: "self"},
	}
}

// FuzzMatch 要求：任意 host / path / 头值 / 方法输入都不能让匹配 panic，
// 并且命中的路由一定来自配置表（不会返回凭空造出来的规则）。
//
// 手搓 *http.Request 而不是用 httptest.NewRequest：后者会对非法输入 panic，
// 那样 fuzz 报的是"构造请求失败"而不是"匹配器有 bug"，噪音太大。
func FuzzMatch(f *testing.F) {
	r, err := New(fuzzRoutes())
	if err != nil {
		f.Fatalf("编译路由表失败: %v", err)
	}
	names := map[string]bool{}
	for _, rt := range fuzzRoutes() {
		names[rt.Name] = true
	}

	for _, c := range []struct{ host, path, client, env, method string }{
		{"api.example.com", "/api/order/list", "mobile-ios", "prod", "GET"},
		{"api.example.com:18080", "/api/users/42", "", "", "GET"},
		{"x.example.com", "/x", "mobile", "*", "POST"},
		{"example.com", "/h", "mobile", "dev", "GET"},
		{"", "/", "", "", ""},
		{"api.example.com", "/api//../etc/passwd", "*", "*", "GET"},
		{"[::1]:8080", "/api/%2e%2e/x", "", "", "GET"},
	} {
		f.Add(c.host, c.path, c.client, c.env, c.method)
	}

	f.Fuzz(func(t *testing.T, host, path, client, env, method string) {
		req := &http.Request{
			Host:   host,
			Method: method,
			URL:    &url.URL{Path: path},
			Header: http.Header{},
		}
		if client != "" {
			req.Header.Set("X-Client", client)
		}
		if env != "" {
			req.Header.Set("X-Env", env)
		}

		got, ok := r.Match(req)
		if !ok {
			return
		}
		if got.Route == nil {
			t.Fatal("Match 返回 ok=true 但 Route 为 nil")
		}
		if !names[got.Route.Name] {
			t.Fatalf("命中了配置里不存在的路由 %q", got.Route.Name)
		}
	})
}
