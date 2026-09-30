package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// 配置外置（审计 C1）：把「默认值 → JSON 文件 → 环境变量」三层合成一份配置。
//
// 为什么值得做：配置原本是编译期 Go 字面量（`Default()`），加一个上游要重新编译 + 重启，
// 这是审计里列的主要生产化缺口。外置之后，改配置只需要改文件（热重载见 `gateway.Reload`）。
//
// 三条刻意的设计取舍（也是容易做错的地方）：
//  1. **只支持 JSON**（标准库 `encoding/json`）：项目定位是「除 grpc 外只用标准库」，
//     不为 YAML 引第三方依赖；JSON 不能写注释这个缺点，用「下划线开头的 key 当注释」补（见 OverlayJSON）。
//  2. **文件是"覆盖"语义，不是"合并"**：`routes` / `services` 一写就**整体替换**，
//     避免出现"删不掉的旧服务"——配置系统最怕的语义模糊就是合并。
//  3. **字段名写错必须报错**：`DisallowUnknownFields()`。静默忽略拼错的字段
//     （`timeout` 写成 `time_out`）是配置外置最常见的线上事故来源。
//
// 时长一律写成字符串（`"2s"` / `"500ms"`）：`time.Duration` 的默认 JSON 形态是**整数纳秒**，
// 手写配置几乎必错，所以这里用 `Duration` 适配。

// Duration 让 JSON 里能写 "2s" / "500ms"（也接受整数纳秒，保持向后兼容）。
type Duration time.Duration

// UnmarshalJSON 解析时长字符串或整数纳秒。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("无法解析时长 %q（示例：\"2s\"、\"500ms\"）: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("时长必须是字符串（如 \"2s\"）或整数纳秒，收到 %s", strings.TrimSpace(string(b)))
	}
	*d = Duration(n)
	return nil
}

// MarshalJSON 输出人类可读的时长字符串。
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// fileConfig 是磁盘上的 JSON 形态。
//
// 标量与子结构用**指针**：只有这样才分得清「没写这个字段」与「显式写了零值」——
// 这是覆盖式加载最容易踩的坑（`burst: 0` 与"没写 burst"语义完全不同）。
type fileConfig struct {
	ListenAddr      *string            `json:"listen_addr"`
	AdminListenAddr *string            `json:"admin_listen_addr"`
	JWTSecret       *string            `json:"jwt_secret"`
	APIKey          *string            `json:"api_key"`
	TrustedProxies  []string           `json:"trusted_proxies"`
	SharedState     *fileSharedState   `json:"shared_state"`
	Overload        *OverloadConfig    `json:"overload"`
	Routes          []Route            `json:"routes"`
	Services        map[string]fileSvc `json:"services"`
}

// fileSharedState 与 SharedStateConfig 对应（时长换成 Duration）。
type fileSharedState struct {
	URL          string   `json:"url"`
	Timeout      Duration `json:"timeout"`
	FailOpen     *bool    `json:"fail_open"`
	SyncInterval Duration `json:"sync_interval"`
}

// fileSvc 与 Service 一一对应，只有时长字段换成 Duration。
type fileSvc struct {
	Name        string        `json:"name"`
	Balance     string        `json:"balance"`
	Upstreams   []Upstream    `json:"upstreams"`
	Timeout     Duration      `json:"timeout"`
	Breaker     *fileBreaker  `json:"breaker"`
	HashKeyFrom string        `json:"hash_key_from"`
	Ejection    *fileEjection `json:"ejection"`
}

type fileBreaker struct {
	WindowSize  int      `json:"window_size"`
	FailRatio   float64  `json:"fail_ratio"`
	MinRequests int      `json:"min_requests"`
	OpenFor     Duration `json:"open_for"`
	HalfOpenMax int      `json:"half_open_max"`
}

type fileEjection struct {
	FailThreshold int      `json:"fail_threshold"`
	Cooldown      Duration `json:"cooldown"`
}

// OverlayJSON 用一段 JSON 覆盖配置：只覆盖"写了的字段"，其余保持原值。
// 覆盖完成后**必须**由调用方跑 `Validate()`（`Load` / `gateway.New` 都会跑）。
//
// 约定：**以下划线开头的 key 一律当注释忽略**（递归生效，`_说明` / `_comment` / `_doc` 都行）——
// JSON 没有注释语法，而配置又是最需要写清"这一项为什么这么配"的地方；与其外挂一份说明文档，
// 不如给一个明确的逃生口（见 `deploy/config.example.json`）。
func (c *Config) OverlayJSON(data []byte) error {
	// Windows 的编辑器（以及 PowerShell 的 `Set-Content -Encoding UTF8`）会给文件加 UTF-8 BOM，
	// 而 json.Decoder 会把 BOM 当成第一个字符，直接报 `invalid character 'ï'` ——
	// 用户从文件内容上看不出任何问题。这属于纯环境噪音，在这里统一剥掉（本机实测踩过）。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	// 先用 UseNumber 保持数字字面量原样（避免 float64 往返丢精度），再剥掉注释键
	raw := map[string]any{}
	dec0 := json.NewDecoder(bytes.NewReader(data))
	dec0.UseNumber()
	if err := dec0.Decode(&raw); err != nil {
		return fmt.Errorf("解析 JSON 配置失败: %w", err)
	}
	stripCommentFields(raw)
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("重新序列化配置失败: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(cleaned))
	dec.DisallowUnknownFields()
	var fc fileConfig
	if err := dec.Decode(&fc); err != nil {
		return fmt.Errorf("解析 JSON 配置失败（字段名拼错也会在这里报）: %w", err)
	}

	if fc.ListenAddr != nil {
		c.ListenAddr = *fc.ListenAddr
	}
	if fc.AdminListenAddr != nil {
		c.AdminListenAddr = *fc.AdminListenAddr
	}
	if fc.JWTSecret != nil {
		c.JWTSecret = *fc.JWTSecret
	}
	if fc.APIKey != nil {
		c.APIKey = *fc.APIKey
	}
	if fc.TrustedProxies != nil {
		c.TrustedProxies = fc.TrustedProxies
	}
	if fc.Overload != nil {
		c.Overload = *fc.Overload
	}
	if fc.SharedState != nil {
		c.SharedState = SharedStateConfig{
			URL:          fc.SharedState.URL,
			Timeout:      fc.SharedState.Timeout,
			FailOpen:     fc.SharedState.FailOpen,
			SyncInterval: fc.SharedState.SyncInterval,
		}
	}
	if fc.Routes != nil {
		c.Routes = fc.Routes // 整体替换，见文件头第 2 条取舍
	}
	if fc.Services != nil {
		services := make(map[string]*Service, len(fc.Services))
		for name, fs := range fc.Services {
			svc := &Service{
				Name:        fs.Name,
				Balance:     fs.Balance,
				Upstreams:   fs.Upstreams,
				Timeout:     time.Duration(fs.Timeout),
				HashKeyFrom: fs.HashKeyFrom,
			}
			if svc.Name == "" {
				svc.Name = name // 允许省略 name，用 map 的 key 兜底
			}
			if fs.Breaker != nil {
				svc.Breaker = BreakerConfig{
					WindowSize:  fs.Breaker.WindowSize,
					FailRatio:   fs.Breaker.FailRatio,
					MinRequests: fs.Breaker.MinRequests,
					OpenFor:     time.Duration(fs.Breaker.OpenFor),
					HalfOpenMax: fs.Breaker.HalfOpenMax,
				}
			}
			if fs.Ejection != nil {
				svc.Ejection = EjectionConfig{
					FailThreshold: fs.Ejection.FailThreshold,
					Cooldown:      time.Duration(fs.Ejection.Cooldown),
				}
			}
			services[name] = svc
		}
		c.Services = services
	}
	return nil
}

// FromEnv 用环境变量覆盖配置（优先级最高：环境变量 > 文件 > 内置默认）。
// 只支持少量与部署/安全强相关的项，避免把配置系统做成"第二套配置语言"。
func (c *Config) FromEnv() {
	if v := os.Getenv("GW_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("GW_ADMIN_LISTEN_ADDR"); v != "" {
		c.AdminListenAddr = v
	}
	if v := os.Getenv("GW_JWT_SECRET"); v != "" {
		c.JWTSecret = v
	}
	if v := os.Getenv("GW_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("GW_TRUSTED_PROXIES"); strings.TrimSpace(v) != "" {
		c.TrustedProxies = splitList(v)
	}
}

// Load 构造配置：`Default()` → 可选的 JSON 文件（`GW_CONFIG` 指向）→ 环境变量覆盖 → 启动期校验。
//
// 第二个返回值是**配置来源描述**，用于启动横幅与热重载日志（出问题时第一眼要看的就是它）。
func Load() (*Config, string, error) {
	cfg := Default()
	source := "built-in（编译期默认值）"

	if path := strings.TrimSpace(os.Getenv("GW_CONFIG")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, path, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		if err := cfg.OverlayJSON(data); err != nil {
			return nil, path, fmt.Errorf("配置文件 %s 有误: %w", path, err)
		}
		source = path
	}

	before := cfg.ListenAddr + "|" + cfg.AdminListenAddr + "|" + cfg.JWTSecret + "|" + cfg.APIKey
	cfg.FromEnv()
	if cfg.ListenAddr+"|"+cfg.AdminListenAddr+"|"+cfg.JWTSecret+"|"+cfg.APIKey != before {
		source += " + 环境变量覆盖"
	}

	if err := cfg.Validate(); err != nil {
		return nil, source, fmt.Errorf("配置校验失败（来源 %s）: %w", source, err)
	}
	return cfg, source, nil
}

func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stripCommentFields 递归删除以下划线开头的 key —— JSON 配置的"注释"约定。
// 只认下划线开头，**不要**放宽成"未知字段一律忽略"：那会把拼错的字段名一起吞掉，
// 正是 `DisallowUnknownFields` 要防的事。
func stripCommentFields(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			if strings.HasPrefix(k, "_") {
				delete(t, k)
				continue
			}
			stripCommentFields(sub)
		}
	case []any:
		for _, sub := range t {
			stripCommentFields(sub)
		}
	}
}
