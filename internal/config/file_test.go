package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOverlayJSON 覆盖语义（审计 C1）：
//   - 只改"写了"的字段，其余保持默认；
//   - routes / services 是**整体替换**（避免"删不掉的旧服务"）；
//   - 时长写成 "1500ms" 这种人类可读形式；
//   - 省略 name 时用 map 的 key 兜底。
func TestOverlayJSON(t *testing.T) {
	cfg := Default()
	doc := `{
	  "listen_addr": "0.0.0.0:8080",
	  "services": {
	    "demo-svc": {
	      "balance": "least_conn",
	      "upstreams": [{"addr": "127.0.0.1:19001"}, {"addr": "127.0.0.1:19002"}],
	      "timeout": "1500ms",
	      "breaker": {"window_size": 8, "fail_ratio": 0.3, "min_requests": 3, "open_for": "2s", "half_open_max": 1},
	      "ejection": {"fail_threshold": 4, "cooldown": "7s"}
	    }
	  },
	  "routes": [
	    {"name": "demo", "host": "*", "path": "/demo", "path_type": "exact", "upstream": "demo-svc"}
	  ]
	}`
	if err := cfg.OverlayJSON([]byte(doc)); err != nil {
		t.Fatalf("OverlayJSON 失败: %v", err)
	}

	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.JWTSecret != Default().JWTSecret {
		t.Errorf("没写的字段应保持默认，JWTSecret 被改成了 %q", cfg.JWTSecret)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Name != "demo" {
		t.Errorf("routes 应整体替换，实际 %d 条: %+v", len(cfg.Routes), cfg.Routes)
	}
	if len(cfg.Services) != 1 {
		t.Errorf("services 应整体替换，实际 %d 个", len(cfg.Services))
	}
	svc := cfg.Services["demo-svc"]
	if svc == nil {
		t.Fatal("demo-svc 不见了")
	}
	if svc.Name != "demo-svc" {
		t.Errorf("省略 name 时应回落到 map 的 key，实际 %q", svc.Name)
	}
	if svc.Timeout != 1500*time.Millisecond {
		t.Errorf("timeout 应解析成 1.5s，实际 %v", svc.Timeout)
	}
	if svc.Breaker.OpenFor != 2*time.Second || svc.Breaker.MinRequests != 3 {
		t.Errorf("breaker 解析有误: %+v", svc.Breaker)
	}
	if svc.Ejection.FailThreshold != 4 || svc.Ejection.Cooldown != 7*time.Second {
		t.Errorf("ejection 解析有误: %+v", svc.Ejection)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("覆盖后的配置应能通过启动校验，实际: %v", err)
	}
}

// TestOverlayJSONRejectsUnknownField：字段名拼错必须报错，不能静默忽略。
func TestOverlayJSONRejectsUnknownField(t *testing.T) {
	cfg := Default()
	err := cfg.OverlayJSON([]byte(`{"listen_addrxxx": "127.0.0.1:1"}`))
	if err == nil {
		t.Fatal("拼错的字段名应当报错")
	}
	if !strings.Contains(err.Error(), "listen_addrxxx") {
		t.Errorf("错误信息里应当点出字段名，实际: %v", err)
	}
}

// TestOverlayJSONDurationErrors：时长写错要给可读提示（这是配置外置最常见的坑）。
func TestOverlayJSONDurationErrors(t *testing.T) {
	cfg := Default()
	err := cfg.OverlayJSON([]byte(`{"services":{"a":{"timeout":"2 seconds"}}}`))
	if err == nil || !strings.Contains(err.Error(), "解析时长") {
		t.Fatalf("非法时长应当报错并提到「解析时长」，实际: %v", err)
	}
}

// TestLoadFromFile：GW_CONFIG 指向的文件生效，且来源描述就是文件路径。
func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.json")
	if err := os.WriteFile(path, []byte(`{"listen_addr":"127.0.0.1:19123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GW_CONFIG", path)
	t.Setenv("GW_LISTEN_ADDR", "")

	cfg, source, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:19123" {
		t.Errorf("文件里的 listen_addr 未生效: %q", cfg.ListenAddr)
	}
	if source != path {
		t.Errorf("来源应指向文件路径，实际 %q", source)
	}
}

// TestLoadEnvOverrides：环境变量优先级最高（部署时用它改监听地址/密钥，不用碰文件）。
func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("GW_CONFIG", "")
	t.Setenv("GW_LISTEN_ADDR", "0.0.0.0:19999")
	t.Setenv("GW_JWT_SECRET", "from-env")
	t.Setenv("GW_TRUSTED_PROXIES", "10.0.0.0/8, 127.0.0.1/32")
	t.Setenv("GW_API_KEY", "")

	cfg, source, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:19999" || cfg.JWTSecret != "from-env" {
		t.Errorf("环境变量未生效: %q / %q", cfg.ListenAddr, cfg.JWTSecret)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Errorf("可信代理应解析成 2 条，实际 %v", cfg.TrustedProxies)
	}
	if !strings.Contains(source, "环境变量") {
		t.Errorf("来源描述应提到环境变量覆盖，实际 %q", source)
	}
}

// TestOverlayJSONAcceptsBOM：Windows 编辑器/PowerShell 写出的 UTF-8 BOM 必须被容忍。
// 不剥 BOM 的话，报错是 `invalid character 'ï' looking for beginning of value`，
// 而用户从文件内容里完全看不出问题（本机实测踩过）。
func TestOverlayJSONAcceptsBOM(t *testing.T) {
	cfg := Default()
	doc := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"listen_addr":"127.0.0.1:18099"}`)...)
	if err := cfg.OverlayJSON(doc); err != nil {
		t.Fatalf("带 BOM 的配置应当能被解析，实际: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:18099" {
		t.Errorf("BOM 之后的内容应当生效，实际 %q", cfg.ListenAddr)
	}
}

// TestOverlayJSONUnderscoreComments：下划线开头的 key 当注释忽略（递归生效），
// 这样示例配置里可以直接写说明，不必外挂文档。
func TestOverlayJSONUnderscoreComments(t *testing.T) {
	cfg := Default()
	doc := `{
	  "_说明": ["这份配置是覆盖语义", "时长写 2s 这种形式"],
	  "listen_addr": "127.0.0.1:18081",
	  "services": {
	    "_comment": "只留一个服务",
	    "demo": {"timeout": "1s", "upstreams": [{"addr": "127.0.0.1:19001"}],
	             "breaker": {"_why": "没有样本量保护会一次失败就熔断", "fail_ratio": 0.5, "min_requests": 4}}
	  },
	  "routes": [{"name": "demo", "path": "/demo", "path_type": "exact", "upstream": "demo"}]
	}`
	if err := cfg.OverlayJSON([]byte(doc)); err != nil {
		t.Fatalf("下划线注释键应当被忽略，实际报错: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:18081" {
		t.Errorf("正常字段应生效，实际 %q", cfg.ListenAddr)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("覆盖后应通过校验: %v", err)
	}
}

// TestLoadFileErrorMentionsSource：文件写坏时，错误里要能看出是哪个来源的问题。
func TestLoadFileErrorMentionsSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte(`{"routes":[{"name":"dup","path":"/a","path_type":"exact","upstream":"nope"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GW_CONFIG", path)

	_, source, err := Load()
	if err == nil {
		t.Fatal("上游服务不存在，应当校验失败")
	}
	if source != path || !strings.Contains(err.Error(), "不存在的上游服务") {
		t.Errorf("错误应指出来源与原因，实际 source=%q err=%v", source, err)
	}
}
