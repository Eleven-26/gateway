package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gwlab/internal/config"
)

// mustRouter 编译一张路由表，出错直接 Fatal（路由表是启动期资产，测试里也不例外）。
func mustRouter(t *testing.T, routes []config.Route) *Router {
	t.Helper()
	r, err := New(routes)
	if err != nil {
		t.Fatalf("New(%d 条路由): %v", len(routes), err)
	}
	return r
}

// hdrRoute 造一条「/h 精确 + 单个头规则」的路由，用来逐条验证头值语义。
func hdrRoute(key, value string) []config.Route {
	return []config.Route{{
		Name: "hdr-rule", Host: "*", Path: "/h", PathType: config.PathExact,
		Headers: map[string]string{key: value}, Upstream: "svc",
	}}
}

// hostRoute 造一条只约束 Host 的路由。
func hostRoute(host string) []config.Route {
	return []config.Route{{
		Name: "host-rule", Host: host, Path: "/h", PathType: config.PathExact, Upstream: "svc",
	}}
}

// methodRoute 造一条只约束 Method 的路由。
func methodRoute() []config.Route {
	return []config.Route{{
		Name: "post-only", Host: "*", Path: "/m", PathType: config.PathExact,
		Methods: []string{"POST"}, Upstream: "svc",
	}}
}

// priorityRoutes 覆盖 exact / regex / 前缀三档优先级，且故意把兜底前缀排在第一个：
// Match 是全表扫描后按得分取最优，与路由声明顺序无关。
func priorityRoutes() []config.Route {
	return []config.Route{
		{Name: "fallback", Host: "*", Path: "/", PathType: config.PathPrefix, Upstream: "svc"},
		{Name: "short-prefix", Host: "*", Path: "/api/", PathType: config.PathPrefix, Upstream: "svc"},
		{Name: "long-prefix", Host: "*", Path: "/api/users/", PathType: config.PathPrefix, Upstream: "svc"},
		{Name: "num-regex", Host: "*", Path: `^/api/users/\d+$`, PathType: config.PathRegex, Upstream: "svc"},
		{Name: "id-exact", Host: "*", Path: "/api/users/42", PathType: config.PathExact, Upstream: "svc"},
	}
}

// TestMatch 表驱动覆盖 Host / Method / Header / 路径优先级。
//
// 重点是 Header 与优先级这两块 —— 头值正则预编译是纯性能重构，
// 这里每一条断言都是「改动前后必须逐字一致」的行为契约。
func TestMatch(t *testing.T) {
	cases := []struct {
		name       string
		routes     []config.Route
		method     string // 空 = GET
		host       string // 空 = httptest 默认 example.com
		path       string
		headers    map[string]string
		wantRoute  string // 空 = 期望不命中
		wantReason string // 非空则精确比对
		wantGroups []string
	}{
		// —— 头值：精确值 / 大小写 / 缺失 / 不匹配 ——
		{
			name: "头值精确相等", routes: hdrRoute("X-Client", "mobile"), path: "/h",
			headers:   map[string]string{"X-Client": "mobile"},
			wantRoute: "hdr-rule", wantReason: "exact /h",
		},
		{
			name: "头值大小写不同也算命中(EqualFold)", routes: hdrRoute("X-Client", "MOBILE"), path: "/h",
			headers:   map[string]string{"X-Client": "mobile"},
			wantRoute: "hdr-rule",
		},
		{
			name: "规则头名与请求头名大小写不同也能命中", routes: hdrRoute("x-client", "mobile"), path: "/h",
			headers:   map[string]string{"X-CLIENT": "mobile"},
			wantRoute: "hdr-rule",
		},
		{
			name: "缺失头不命中", routes: hdrRoute("X-Client", "mobile"), path: "/h",
			wantRoute: "",
		},
		{
			name: "头存在但值为空串视为缺失(连 * 也不放行)", routes: hdrRoute("X-Client", "*"), path: "/h",
			headers:   map[string]string{"X-Client": ""},
			wantRoute: "",
		},
		{
			name: "头值不匹配", routes: hdrRoute("X-Client", "mobile"), path: "/h",
			headers:   map[string]string{"X-Client": "desktop"},
			wantRoute: "",
		},

		// —— 头值 "*" ——
		{
			name: "头值 * 只要头存在就命中", routes: hdrRoute("X-Client", "*"), path: "/h",
			headers:   map[string]string{"X-Client": "whatever-1.0"},
			wantRoute: "hdr-rule",
		},
		{
			name: "头值 * 但头缺失不命中", routes: hdrRoute("X-Client", "*"), path: "/h",
			wantRoute: "",
		},

		// —— 前缀通配 mobile* ——
		{
			name: "前缀通配 mobile* 命中 mobile9", routes: hdrRoute("X-Client", "mobile*"), path: "/h",
			headers:   map[string]string{"X-Client": "mobile9"},
			wantRoute: "hdr-rule",
		},
		{
			name: "前缀通配 mobile* 命中 mobile 本身(.* 可空)", routes: hdrRoute("X-Client", "mobile*"), path: "/h",
			headers:   map[string]string{"X-Client": "mobile"},
			wantRoute: "hdr-rule",
		},
		{
			name: "前缀通配是大小写敏感的: Mobile9 不命中", routes: hdrRoute("X-Client", "mobile*"), path: "/h",
			headers:   map[string]string{"X-Client": "Mobile9"},
			wantRoute: "",
		},
		{
			name: "前缀通配不命中 desktop9", routes: hdrRoute("X-Client", "mobile*"), path: "/h",
			headers:   map[string]string{"X-Client": "desktop9"},
			wantRoute: "",
		},

		// —— 后缀通配 *mobile ——
		{
			name: "后缀通配 *mobile 命中 Xmobile", routes: hdrRoute("X-Client", "*mobile"), path: "/h",
			headers:   map[string]string{"X-Client": "Xmobile"},
			wantRoute: "hdr-rule",
		},
		{
			name: "后缀通配 *mobile 不命中 mobileX", routes: hdrRoute("X-Client", "*mobile"), path: "/h",
			headers:   map[string]string{"X-Client": "mobileX"},
			wantRoute: "",
		},

		// —— 中间通配 a*b ——
		{
			name: "中间通配 a*b 命中 a--b", routes: hdrRoute("X-Client", "a*b"), path: "/h",
			headers:   map[string]string{"X-Client": "a--b"},
			wantRoute: "hdr-rule",
		},
		{
			name: "中间通配 a*b 命中 ab", routes: hdrRoute("X-Client", "a*b"), path: "/h",
			headers:   map[string]string{"X-Client": "ab"},
			wantRoute: "hdr-rule",
		},
		{
			name: "中间通配 a*b 不命中 bba(锚定首尾)", routes: hdrRoute("X-Client", "a*b"), path: "/h",
			headers:   map[string]string{"X-Client": "bba"},
			wantRoute: "",
		},

		// —— 通配值里的正则元字符按字面量处理（QuoteMeta 的意义）——
		{
			name: "通配值 v1.* 里的点号是字面量", routes: hdrRoute("X-Client", "v1.*"), path: "/h",
			headers:   map[string]string{"X-Client": "v1.2"},
			wantRoute: "hdr-rule",
		},
		{
			name: "通配值 v1.* 不命中 v1x2", routes: hdrRoute("X-Client", "v1.*"), path: "/h",
			headers:   map[string]string{"X-Client": "v1x2"},
			wantRoute: "",
		},

		// —— Host ——
		{
			name: "Host 通配 *.example.com 命中 api.example.com", routes: hostRoute("*.example.com"),
			host: "api.example.com", path: "/h", wantRoute: "host-rule", wantReason: "exact /h",
		},
		{
			name: "Host 通配带端口时剥离端口", routes: hostRoute("*.example.com"),
			host: "api.example.com:18080", path: "/h", wantRoute: "host-rule",
		},
		{
			name: "Host 通配不命中裸域名 example.com", routes: hostRoute("*.example.com"),
			host: "example.com", path: "/h", wantRoute: "",
		},
		{
			name: "Host 通配不命中 notexample.com", routes: hostRoute("*.example.com"),
			host: "notexample.com", path: "/h", wantRoute: "",
		},
		{
			name: "Host 精确大小写不敏感", routes: hostRoute("API.example.com"),
			host: "api.example.com", path: "/h", wantRoute: "host-rule",
		},
		{
			name: "Host 精确比较也会剥离端口", routes: hostRoute("api.example.com"),
			host: "api.example.com:8443", path: "/h", wantRoute: "host-rule",
		},

		// —— Method ——
		{
			name: "Method 大小写不敏感(post vs POST)", routes: methodRoute(),
			method: "post", path: "/m", wantRoute: "post-only",
		},
		{
			name: "Method 不匹配则不命中", routes: methodRoute(),
			method: http.MethodGet, path: "/m", wantRoute: "",
		},

		// —— 路径优先级 ——
		{
			name: "exact 胜过 regex(同一条路径两条都能匹配)", routes: priorityRoutes(),
			path: "/api/users/42", wantRoute: "id-exact", wantReason: "exact /api/users/42",
		},
		{
			name: "regex 胜过普通前缀(哪怕前缀更长)", routes: priorityRoutes(),
			path: "/api/users/7", wantRoute: "num-regex", wantReason: "regex " + `^/api/users/\d+$`,
			wantGroups: []string{},
		},
		{
			name: "同级前缀取最长", routes: priorityRoutes(),
			path: "/api/users/me", wantRoute: "long-prefix", wantReason: "prefix /api/users/",
		},
		{
			name: "较短前缀兜住同族其它路径", routes: priorityRoutes(),
			path: "/api/orders", wantRoute: "short-prefix", wantReason: "prefix /api/",
		},
		{
			name: "兜底前缀 / 只在没有更优规则时命中", routes: priorityRoutes(),
			path: "/other", wantRoute: "fallback", wantReason: "prefix /",
		},
		{
			name: "路径不匹配任何规则", routes: []config.Route{
				{Name: "only", Host: "*", Path: "/exact", PathType: config.PathExact, Upstream: "svc"},
			},
			path: "/exact/deeper", wantRoute: "",
		},

		// —— 头 + 优先级组合：头不满足时该路由整条出局，不会「降级」命中 ——
		{
			name: "头不满足的路由不参与打分", routes: []config.Route{
				{Name: "fallback", Host: "*", Path: "/api/", PathType: config.PathPrefix, Upstream: "svc"},
				{
					Name: "hdr-prefix", Host: "*", Path: "/api/", PathType: config.PathPrefix,
					Headers: map[string]string{"X-Client": "mobile*"}, Upstream: "svc",
				},
			},
			path: "/api/users", headers: map[string]string{"X-Client": "desktop"},
			wantRoute: "fallback", wantReason: "prefix /api/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustRouter(t, tc.routes)

			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tc.path, nil)
			if tc.host != "" {
				req.Host = tc.host
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			got, ok := r.Match(req)
			if tc.wantRoute == "" {
				if ok {
					t.Fatalf("期望不命中，实际命中 %s (%s)", got.Route.Name, got.Reason)
				}
				return
			}
			if !ok {
				t.Fatalf("期望命中 %s，实际未命中", tc.wantRoute)
			}
			if got.Route.Name != tc.wantRoute {
				t.Fatalf("命中路由 = %s，期望 %s", got.Route.Name, tc.wantRoute)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Fatalf("命中理由 = %q，期望 %q", got.Reason, tc.wantReason)
			}
			if tc.wantGroups != nil && !reflect.DeepEqual(got.Groups, tc.wantGroups) {
				t.Fatalf("正则捕获组 = %#v，期望 %#v", got.Groups, tc.wantGroups)
			}
		})
	}
}

// TestHeaderValuePattern 锁住「通配值 → 锚定正则」的翻译结果。
//
// 这就是改动前 headersMatch 里内联拼的那串 "^"+...+ "$"，
// 所以逐字比对字符串即可证明语义没变；顺便确认 QuoteMeta 让 . + 等元字符按字面量走。
func TestHeaderValuePattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mobile*", `^mobile.*$`},
		{"*mobile", `^.*mobile$`},
		{"a*b", `^a.*b$`},
		{"a*b*c", `^a.*b.*c$`},
		{"v1.*", `^v1\..*$`},
		{"a+b*", `^a\+b.*$`},
		{"**", `^.*.*$`},
		{"mobile", `^mobile$`},
	}
	for _, tc := range cases {
		if got := headerValuePattern(tc.in); got != tc.want {
			t.Errorf("headerValuePattern(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
		// 翻译结果必须是能编译的正则 —— New 里 err 分支就是为它准备的。
		if _, err := regexp.Compile(headerValuePattern(tc.in)); err != nil {
			t.Errorf("headerValuePattern(%q) 编译失败: %v", tc.in, err)
		}
	}
}

// TestNewPrecompilesHeaderWildcards 验证预编译的落点：New 里一次性编译、匹配期零编译。
func TestNewPrecompilesHeaderWildcards(t *testing.T) {
	r := mustRouter(t, []config.Route{{
		Name: "wild", Host: "*", Path: "/h", PathType: config.PathExact,
		Headers:  map[string]string{"X-Client": "mobile*", "X-Env": "*", "X-Trace": "abc"},
		Upstream: "svc",
	}})

	c := &r.routes[0]
	if c.headerRes == nil {
		t.Fatal("headerRes 为空：含 * 的头值没有被预编译")
	}
	re, ok := c.headerRes["X-Client"]
	if !ok || re == nil {
		t.Fatalf("X-Client 的通配值没有预编译: %#v", c.headerRes)
	}
	if re.String() != `^mobile.*$` {
		t.Fatalf("预编译正则 = %q，期望 %q", re.String(), `^mobile.*$`)
	}
	// "*" 和纯字面量值都不该占用正则（前者是「存在即过」，后者走 EqualFold）。
	if _, ok := c.headerRes["X-Env"]; ok {
		t.Error(`头值 "*" 不该编译正则`)
	}
	if _, ok := c.headerRes["X-Trace"]; ok {
		t.Error("纯字面量头值不该编译正则")
	}
	if len(c.headerRes) != 1 {
		t.Fatalf("headerRes 条目数 = %d，期望 1", len(c.headerRes))
	}

	// 没有通配头值的路由应当保持 nil —— 不给每条路由白付一个 map。
	plain := mustRouter(t, hdrRoute("X-Client", "mobile"))
	if plain.routes[0].headerRes != nil {
		t.Errorf("无通配头值的路由不该分配 headerRes: %#v", plain.routes[0].headerRes)
	}
}

// TestNewBadRegexError 编译失败必须在 New 阶段报错，且错误里带路由名（路径正则同理）。
//
// 头值那条分支实际不可达 —— QuoteMeta 的输出加上 .* 永远是合法正则；保留它是为了
// 与路径正则对称，也防未来改写 headerValuePattern 时静默退化。
func TestNewBadRegexError(t *testing.T) {
	_, err := New([]config.Route{{
		Name: "bad-regex", Host: "*", Path: "([", PathType: config.PathRegex, Upstream: "svc",
	}})
	if err == nil {
		t.Fatal("非法路径正则应当让 New 返回错误")
	}
	if !strings.Contains(err.Error(), "bad-regex") {
		t.Fatalf("错误信息里应带路由名，实际: %v", err)
	}
}

// BenchmarkMatch 度量一次 Match 的耗时与分配。
//
// 请求刻意打中「带通配头值」的那条路由：改动前 headersMatch 每次请求都对
// mobile* 现编一次正则（regexp.MatchString → regexp.Compile），
// allocs/op 里有编译与机器分配；预编译后这部分归零，只剩 MatchResult 自身的分配。
func BenchmarkMatch(b *testing.B) {
	routes := []config.Route{
		{Name: "fallback", Host: "*", Path: "/", PathType: config.PathPrefix, Upstream: "svc"},
		{Name: "health", Host: "*", Path: "/healthz", PathType: config.PathExact, Upstream: "self"},
		{
			Name: "wildcard-header", Host: "*.example.com", Path: "/api/", PathType: config.PathPrefix,
			Headers:  map[string]string{"X-Client": "mobile*", "X-Env": "*"},
			Upstream: "svc",
		},
		{
			Name: "literal-header", Host: "*", Path: "/api/order", PathType: config.PathExact,
			Headers:  map[string]string{"X-Client": "mobile"},
			Upstream: "svc",
		},
	}
	r, err := New(routes)
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/users/42", nil)
	req.Host = "api.example.com:18080"
	req.Header.Set("X-Client", "mobile-ios")
	req.Header.Set("X-Env", "prod")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, ok := r.Match(req)
		if !ok || res.Route.Name != "wildcard-header" {
			b.Fatalf("基准前提被打破: ok=%v res=%+v", ok, res)
		}
	}
}
