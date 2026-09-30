package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMaxServiceTimeout：cmd/gateway 用它推导 http.Server.WriteTimeout，必须容得下最慢的上游。
func TestMaxServiceTimeout(t *testing.T) {
	c := &Config{Services: map[string]*Service{
		"a":      {Timeout: 2 * time.Second},
		"b":      {Timeout: 10 * time.Second},
		"broken": nil, // 配置写错时不能 panic
	}}
	if got := c.MaxServiceTimeout(); got != 10*time.Second {
		t.Errorf("MaxServiceTimeout = %v，期望 10s", got)
	}
	if got := (&Config{}).MaxServiceTimeout(); got != 0 {
		t.Errorf("空配置应返回 0，实际 %v", got)
	}
}

func routeByName(c *Config, name string) *Route {
	for i := range c.Routes {
		if c.Routes[i].Name == name {
			return &c.Routes[i]
		}
	}
	return nil
}

// TestValidate：启动期校验必须把各类错误配置都挡住（审计 P1-1）。
// 每一条都对应一种"以前要等到运行期、甚至永远不报错"的配置错误。
func TestValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("默认配置必须通过校验，实际报错: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string // 期望错误信息里出现的关键字
	}{
		{"ListenAddr 为空", func(c *Config) { c.ListenAddr = " " }, "ListenAddr"},
		{"没有任何路由", func(c *Config) { c.Routes = nil }, "没有任何路由"},
		{"没有任何服务", func(c *Config) { c.Services = nil }, "没有任何上游服务"},
		{"路由缺少 Name", func(c *Config) { routeByName(c, "flaky").Name = "" }, "缺少 Name"},
		{"路由名重复", func(c *Config) { routeByName(c, "flaky").Name = "slow" }, "路由名重复"},
		{"匹配条件完全相同", func(c *Config) {
			dup := *routeByName(c, "order-exact")
			dup.Name = "order-exact-copy"
			c.Routes = append(c.Routes, dup)
		}, "完全相同"},
		{"PathType 非法", func(c *Config) { routeByName(c, "order-exact").PathType = "glob" }, "不合法"},
		{"正则编译失败", func(c *Config) { routeByName(c, "user-regex").Path = "^(unclosed" }, "编译失败"},
		{"前缀不以 / 开头", func(c *Config) { routeByName(c, "open-api").Path = "open/" }, "必须以 / 开头"},
		{"StripPrefix 不以 / 开头", func(c *Config) { routeByName(c, "order-prefix").StripPrefix = "api" }, "StripPrefix"},
		{"Limit.Burst 为 0 但有速率", func(c *Config) {
			routeByName(c, "order-exact").Limit = LimitPolicy{RatePerSec: 10}
		}, "桶容量为 0"},
		{"上游服务不存在", func(c *Config) { routeByName(c, "flaky").Upstream = "nope-svc" }, "不存在的上游服务"},
		{"Transcode 指向非 gRPC 节点", func(c *Config) {
			routeByName(c, "order-create").Upstream = "order-svc"
		}, "没有 GRPC:true"},
		{"Transcode 缺 Method", func(c *Config) {
			routeByName(c, "order-create").Transcode = &TranscodePolicy{Service: "order.OrderService"}
		}, "必须同时给出"},
		{"服务是 nil", func(c *Config) { c.Services["bogus"] = nil }, "是 nil"},
		{"服务 Name 与 key 不一致", func(c *Config) { c.Services["order-svc"].Name = "other" }, "必须一致"},
		{"服务 Timeout <= 0", func(c *Config) { c.Services["order-svc"].Timeout = 0 }, "Timeout 必须 > 0"},
		{"服务没有节点", func(c *Config) { c.Services["order-svc"].Upstreams = nil }, "没有任何上游节点"},
		{"节点地址不是 host:port", func(c *Config) { c.Services["order-svc"].Upstreams[0].Addr = "19001" }, "host:port"},
		{"节点重复", func(c *Config) {
			u := c.Services["order-svc"].Upstreams
			u[1].Addr = u[0].Addr
		}, "重复"},
		{"Breaker.MinRequests > WindowSize", func(c *Config) {
			c.Services["order-svc"].Breaker.MinRequests = 99
		}, "永远不会打开"},
		{"Breaker.FailRatio > 1", func(c *Config) { c.Services["order-svc"].Breaker.FailRatio = 1.5 }, "必须落在 [0,1]"},
		{"Ejection 有阈值但无冷却", func(c *Config) {
			c.Services["order-svc"].Ejection = EjectionConfig{FailThreshold: 3}
		}, "再也回不来"},
		{"TrustedProxies 非法", func(c *Config) { c.TrustedProxies = []string{"not-a-cidr"} }, "TrustedProxies"},
		{"节点 scheme 非法", func(c *Config) { c.Services["order-svc"].Upstreams[0].Scheme = "ftp" }, "不合法"},
		{"配了 tls 但 scheme 不是 https", func(c *Config) {
			c.Services["order-svc"].Upstreams[0].TLS = &TLSConfig{}
		}, "配了 tls"},
		{"https 但 CA 文件读不到", func(c *Config) {
			u := &c.Services["order-svc"].Upstreams[0]
			u.Scheme = "https"
			u.TLS = &TLSConfig{CAFile: filepath.Join(t.TempDir(), "not-exist.pem")}
		}, "TLS 配置有误"},
		{"mTLS 只给证书没给私钥", func(c *Config) {
			u := &c.Services["order-svc"].Upstreams[0]
			u.Scheme = "https"
			u.TLS = &TLSConfig{ClientCertFile: "client.pem"}
		}, "必须同时给"},
		{"路由 Timeout 为负", func(c *Config) { c.Routes[0].Timeout = Duration(-1) }, "不能为负"},
		{"重试次数过大", func(c *Config) { c.Routes[0].Retry = &RetryPolicy{Attempts: 99} }, "过大"},
		{"重试次数为负", func(c *Config) { c.Routes[0].Retry = &RetryPolicy{Attempts: -1} }, "不能为负"},
		{"单次尝试超时过短", func(c *Config) {
			c.Routes[0].Retry = &RetryPolicy{Attempts: 1, PerTryTimeout: Duration(2 * time.Millisecond)}
		}, "太短"},
		{"Overload 有队列但没并发上限", func(c *Config) { c.Overload = OverloadConfig{MaxQueue: 5} }, "没设 MaxInflight"},
		{"Overload 为负", func(c *Config) { c.Overload = OverloadConfig{MaxInflight: -1} }, "不能为负"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("期望校验失败，实际通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息里应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// TestValidateNormalizesHost：Host 为空按「任意」处理，校验时顺手归一化成 "*"。
func TestValidateNormalizesHost(t *testing.T) {
	cfg := Default()
	routeByName(cfg, "public-health").Host = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("空 Host 应当被接受，实际 %v", err)
	}
	if got := routeByName(cfg, "public-health").Host; got != "*" {
		t.Errorf("空 Host 应被归一化为 \"*\"，实际 %q", got)
	}
}

// TestValidateAllowsSelfWithoutUpstreams：self 是伪服务，允许没有节点。
func TestValidateAllowsSelfWithoutUpstreams(t *testing.T) {
	cfg := Default()
	if len(cfg.Services["self"].Upstreams) != 0 {
		t.Fatalf("这个用例假设 self 没有节点")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("self 无节点是合法的，实际 %v", err)
	}
}
