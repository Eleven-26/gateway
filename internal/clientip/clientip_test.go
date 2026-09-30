package clientip

import (
	"net/http"
	"net/netip"
	"testing"
)

// mustTrusted 把测试里的 CIDR/IP 字面量转成前缀集合；解析失败直接 Fatal（测试自身写错了）。
func mustTrusted(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	ps, err := ParseTrusted(cidrs)
	if err != nil {
		t.Fatalf("ParseTrusted(%v) 失败: %v", cidrs, err)
	}
	return ps
}

// TestResolve 覆盖 Resolve 的全部取值分支。
// 每条用例的 why 写清「这条钉住的是哪个具体错误」，避免后人删测试时不知道丢了什么保证。
func TestResolve(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		trusted    []string
		want       string
		why        string
	}{
		{
			// 【要求 1】伪造无效：对端不可信时 XFF / X-Real-IP 一个都不看。
			name:       "对端不可信_伪造XFF与XRealIP被忽略",
			remoteAddr: "203.0.113.9:5678",
			headers:    map[string]string{"X-Forwarded-For": "1.2.3.4", "X-Real-IP": "5.6.7.8"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.9",
			why:        "不看对端就采信 XFF = 匿名客户端可伪造 IP 绕过限流（审计 P1-3 错误修法的典型形态）",
		},
		{
			// 同上，但 trusted 为空切片：等价于「没有任何可信代理」。
			name:       "可信列表为空_伪造XFF同样被忽略",
			remoteAddr: "203.0.113.9:5678",
			headers:    map[string]string{"X-Forwarded-For": "1.2.3.4, 10.0.0.1"},
			trusted:    nil,
			want:       "203.0.113.9",
			why:        "空 trusted 是最容易写错的「默认信任」情形，必须退化成只用 RemoteAddr",
		},
		{
			// 【要求 2】可信链：从右往左跳过可信跳，取第一个不可信地址。
			name:       "对端可信_取XFF最右的不可信跳",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "XFF 最左是客户端自称值；从左侧取会被伪造前缀带偏",
		},
		{
			name:       "对端可信_单跳XFF即真实客户端",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "单跳是最常见的 LB 形态（客户端 → SLB），不能因为只有一个值就退回对端",
		},
		{
			name:       "对端可信_多级代理链逐跳跳过",
			remoteAddr: "10.0.0.9:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.1, 192.168.0.2"},
			trusted:    []string{"10.0.0.0/8", "192.168.0.0/16"},
			want:       "203.0.113.7",
			why:        "两级可信代理（CDN + SLB）时可信段不止一条，必须全部跳过",
		},
		{
			// 【要求 3】整条 XFF 都是可信跳 → 取最左那个。
			name:       "对端可信_XFF全是可信跳_取最左",
			remoteAddr: "10.0.0.5:1234",
			headers:    map[string]string{"X-Forwarded-For": "10.1.1.1, 10.0.0.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.1.1.1",
			why:        "内网客户端直连 SLB 时客户端本身也在可信段内；退回对端会让它们坍缩成一个限流桶（见包注释③）",
		},
		{
			name:       "对端可信_单条可信跳也是取它自己",
			remoteAddr: "10.0.0.5:1234",
			headers:    map[string]string{"X-Forwarded-For": "10.7.7.7"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.7.7.7",
			why:        "「全可信」在只有一条时也必须成立，否则该分支只在大链上被测到",
		},
		{
			// 【要求 4】无 XFF → 回落 X-Real-IP（仅对端可信时）。
			name:       "对端可信_无XFF_回落XRealIP",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Real-IP": "198.51.100.9"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "198.51.100.9",
			why:        "只有对端可信才允许回落；这条同时证明了 X-Real-IP 通道可用",
		},
		{
			name:       "对端可信_XFF为空串_回落XRealIP",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "   ", "X-Real-IP": "198.51.100.9"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "198.51.100.9",
			why:        "空 XFF 头（有些代理会写成空值）不能当成「有效证据」而返回空字符串",
		},
		{
			// 【要求 5】XFF 夹非法值：跳过该条目继续往左，而不是整条放弃。
			name:       "对端可信_XFF夹非法值_跳过继续往左",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "garbage, 203.0.113.7, 10.0.0.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "解析失败就整体放弃 = 攻击者塞一个 garbage 即可顶掉真实 IP（常见实现 bug）",
		},
		{
			name:       "对端可信_非法值在链中间也要跳过",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7, not-an-ip, 10.0.0.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "从右往左走时非法值出现在中间（可信跳左侧），跳过逻辑不能只在最右端生效",
		},
		{
			name:       "对端可信_XFF最右为非法值_跳过它继续往左",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.1, ???"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "最右端非法时不能直接回落 X-Real-IP/对端，否则真实 IP 被一个坏条目顶掉",
		},
		{
			name:       "对端可信_全非法XFF_回落XRealIP",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "garbage, ???", "X-Real-IP": "198.51.100.9"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "198.51.100.9",
			why:        "全非法等价于「没有可用证据」，此时才允许回落 X-Real-IP",
		},
		{
			// 【要求 6】IPv6：方括号 + 端口形式。
			name:       "IPv6对端_无trusted_返回去括号地址",
			remoteAddr: "[2001:db8::1]:443",
			headers:    map[string]string{"X-Forwarded-For": "1.2.3.4"},
			trusted:    nil,
			want:       "2001:db8::1",
			why:        "IPv6 的 [addr]:port 必须用 net.SplitHostPort 剥（含方括号），自己按 : 切会切坏",
		},
		{
			name:       "IPv6回环_去括号与端口",
			remoteAddr: "[::1]:5678",
			headers:    nil,
			trusted:    nil,
			want:       "::1",
			why:        "本机/容器内自测最常出现的形态，剥错会得到 \"[::1]\" 这种非法 IP",
		},
		{
			name:       "IPv6可信链_按2001db8::/32过滤",
			remoteAddr: "[2001:db8::2]:443",
			headers:    map[string]string{"X-Forwarded-For": "2001:db8:1::9, 2001:db8::2"},
			trusted:    []string{"2001:db8::/32"},
			want:       "2001:db8:1::9",
			why:        "IPv6 前缀 Contains 同样要生效，不能只测 IPv4 可信段",
		},
		{
			name:       "IPv6_XFF全在可信前缀内_取最左",
			remoteAddr: "[2001:db8::2]:443",
			headers:    map[string]string{"X-Forwarded-For": "2001:db8:aaaa::1, 2001:db8::2"},
			trusted:    []string{"2001:db8::/32"},
			want:       "2001:db8:aaaa::1",
			why:        "「全可信跳」规则在 IPv6 下与 IPv4 一致",
		},
		{
			name:       "IPv6_对端不可信_伪造XFF失效",
			remoteAddr: "[2001:db8::2]:443",
			headers:    map[string]string{"X-Forwarded-For": "2001:db8:1::9"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "2001:db8::2",
			why:        "可信段与对端不同族时不能被误判为可信",
		},
		{
			name:       "IPv6_XRealIP通道",
			remoteAddr: "[2001:db8::2]:443",
			headers:    map[string]string{"X-Real-IP": "198.51.100.9"},
			trusted:    []string{"2001:db8::/32"},
			want:       "198.51.100.9",
			why:        "X-Real-IP 可以是 IPv4 而对端是 IPv6（双栈 LB），不该被族不同挡住",
		},
		{
			// 边界：remoteAddr 本身没有端口（httptest / 内部直调的场景）。
			name:       "无端口对端_按整串即主机处理",
			remoteAddr: "10.0.0.1",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "SplitHostPort 失败时必须退化为「整串就是主机」，否则可信判定整个失效",
		},
		{
			name:       "可信且无任何转发头_返回对端主机",
			remoteAddr: "10.0.0.1:1234",
			headers:    nil,
			trusted:    []string{"10.0.0.0/8"},
			want:       "10.0.0.1",
			why:        "兜底分支：不能让返回值变成空串（限流 key 空串会再次坍缩成单一桶）",
		},
		{
			name:       "IPv4映射地址_归一为IPv4",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "::ffff:203.0.113.7, ::ffff:10.0.0.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "不 Unmap 时 ::ffff:10.0.0.1 不会被 10.0.0.0/8 命中，可信跳会被当成客户端返回",
		},
		{
			name:       "XFF条目带端口_仍能解析",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7:5555, 10.0.0.1"},
			trusted:    []string{"10.0.0.0/8"},
			want:       "203.0.113.7",
			why:        "现实里存在带端口的脏 XFF，按主机解析比丢弃更接近真实客户端",
		},
		{
			name:       "裸IP形式的trusted_同样命中",
			remoteAddr: "10.0.0.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7, 192.168.1.1"},
			trusted:    []string{"10.0.0.1", "192.168.1.1"},
			want:       "203.0.113.7",
			why:        "ParseTrusted 支持裸 IP（/32），接线方直接写单机地址也要能用",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			trusted := mustTrusted(t, tc.trusted...)
			got := Resolve(tc.remoteAddr, h, trusted)
			if got != tc.want {
				t.Fatalf("Resolve(%q, %v, %v) = %q，期望 %q\n这条用例钉住的是：%s",
					tc.remoteAddr, tc.headers, tc.trusted, got, tc.want, tc.why)
			}
		})
	}
}

// TestResolveMultipleXFFHeaders 现实里的代理链常常是多条同名 XFF 头（每跳 Append 一行），
// 语义等价于按序逗号拼接；只看第一条会把真实客户端丢掉。
func TestResolveMultipleXFFHeaders(t *testing.T) {
	h := http.Header{}
	h.Add("X-Forwarded-For", "203.0.113.7")
	h.Add("X-Forwarded-For", "10.0.0.1")

	got := Resolve("10.0.0.9:1234", h, mustTrusted(t, "10.0.0.0/8"))
	if want := "203.0.113.7"; got != want {
		t.Fatalf("多行 XFF 应拼接后从右往左解析，得到 %q，期望 %q", got, want)
	}
}

// TestResolveNilHeaderForwardedForIgnoredWhenUntrusted 显式钉住安全底线：
// 即使 h 为 nil（不会 panic），不可信对端也绝不从转发头取值。
func TestResolveNilHeaderForwardedForIgnoredWhenUntrusted(t *testing.T) {
	got := Resolve("203.0.113.9:5678", nil, nil)
	if want := "203.0.113.9"; got != want {
		t.Fatalf("nil header + 空 trusted 应返回对端，得到 %q，期望 %q", got, want)
	}
}

// TestParseTrusted 覆盖合法 CIDR、裸 IP、规范化与非法输入。
func TestParseTrusted(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string // 期望的 Prefix.String() 序列；nil 表示期望空结果
		wantErr bool
		why     string
	}{
		{
			name: "合法CIDR",
			in:   []string{"10.0.0.0/8", "192.168.0.0/16", "2001:db8::/32"},
			want: []string{"10.0.0.0/8", "192.168.0.0/16", "2001:db8::/32"},
			why:  "最基本形态",
		},
		{
			name: "裸IPv4当作32位前缀",
			in:   []string{"10.0.0.1"},
			want: []string{"10.0.0.1/32"},
			why:  "接线方常直接写单机地址，不支持的裸 IP 会被当成错误配置",
		},
		{
			name: "裸IPv6当作128位前缀",
			in:   []string{"2001:db8::1"},
			want: []string{"2001:db8::1/128"},
			why:  "BitLen() 对 IPv6 是 128，不能写死 32",
		},
		{
			name: "主机位非零的CIDR被规范化",
			in:   []string{"10.0.0.1/8"},
			want: []string{"10.0.0.0/8"},
			why:  "Masked() 避免 Contains 语义被误读（也就避免了「看起来匹配却匹配不上」）",
		},
		{
			name: "IPv4映射裸IP归一为IPv4",
			in:   []string{"::ffff:10.0.0.1"},
			want: []string{"10.0.0.1/32"},
			why:  "Unmap 保证与 Resolve 的客户端地址同族，比较才成立",
		},
		{
			name: "空白项被跳过",
			in:   []string{"10.0.0.0/8", "  ", ""},
			want: []string{"10.0.0.0/8"},
			why:  "配置里尾随逗号不该让网关起不来；空白项不构成任何信任，跳过是安全的",
		},
		{
			name: "两侧空白被裁剪",
			in:   []string{"  10.0.0.0/8  "},
			want: []string{"10.0.0.0/8"},
			why:  "从配置文件/环境变量读来的值常带空格",
		},
		{
			name: "空输入返回空集合",
			in:   nil,
			want: nil,
			why:  "nil 是最常见的默认值，必须等价于「没有可信代理」而不是报错",
		},
		{
			name:    "非法字符串返回错误",
			in:      []string{"not-a-cidr"},
			wantErr: true,
			why:     "可信代理列表写错属于安全配置错误，必须显式失败而不是静默忽略",
		},
		{
			name:    "前缀长度越界返回错误",
			in:      []string{"10.0.0.0/33"},
			wantErr: true,
			why:     "越界前缀不能被静默接受成 10.0.0.0/32 或 /0",
		},
		{
			name:    "非法项在中间也要报错",
			in:      []string{"10.0.0.0/8", "10.0.0.0/8/8"},
			wantErr: true,
			why:     "不能在第一个合法项之后就停止校验",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTrusted(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTrusted(%v) 期望报错，实际返回 %v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTrusted(%v) 意外报错: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseTrusted(%v) 返回 %d 项 %v，期望 %d 项 %v\n这条用例钉住的是：%s",
					tc.in, len(got), got, len(tc.want), tc.want, tc.why)
			}
			for i := range got {
				if got[i].String() != tc.want[i] {
					t.Fatalf("ParseTrusted(%v)[%d] = %q，期望 %q\n这条用例钉住的是：%s",
						tc.in, i, got[i].String(), tc.want[i], tc.why)
				}
			}
		})
	}
}

// TestParseTrustedResultFeedsResolve 把两个 API 串起来跑一遍，防止出现
// 「ParseTrusted 解析得出来、Resolve 却匹配不上」的族/规范化不一致。
func TestParseTrustedResultFeedsResolve(t *testing.T) {
	trusted, err := ParseTrusted([]string{"10.0.0.1", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("ParseTrusted 失败: %v", err)
	}

	if got, want := Resolve("10.0.0.1:1", http.Header{"X-Forwarded-For": {"203.0.113.7"}}, trusted), "203.0.113.7"; got != want {
		t.Errorf("裸 IP 可信对端: 得到 %q，期望 %q", got, want)
	}
	if got, want := Resolve("[2001:db8::2]:1", http.Header{"X-Forwarded-For": {"2001:db8:1::9"}}, trusted), "2001:db8:1::9"; got != want {
		t.Errorf("IPv6 可信对端: 得到 %q，期望 %q", got, want)
	}
}
