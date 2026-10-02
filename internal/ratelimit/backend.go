package ratelimit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gwlab/internal/config"
)

// Backend 是「谁来判断放行」的可插拔后端。
//
// 默认的 LocalBackend 把令牌桶放在进程内 —— 单副本够用，但**多副本时每个副本各有一份桶**，
// 实际放行量会变成「副本数 × 阈值」。这是扩容时最容易忽略的陷阱：压测（单副本）一切正常，
// 一上两副本限流就失效了。
//
// ⚠️ 接口刻意设计成「一次判定」而不是「取桶」：
//
//	Backend.Allow(key, policy) (bool, time.Duration)
//
// 分布式实现里根本没有「本地桶对象」这个东西 —— 任何形如
// `GetBucket(key).Allow()` 的接口都无法远程实现（要么把桶缓存到本地从而又变成各算各的，
// 要么每次请求都往返两次）。这是设计限流抽象时最容易走错的一步。
type Backend interface {
	// Allow 判断这次请求是否放行；被拒时返回还需等待多久（用于 Retry-After）。
	Allow(key string, p config.LimitPolicy) (allow bool, retryAfter time.Duration)
	// Name 用于日志、/readyz 与指标：一眼能看出当前是本地限流还是共享限流。
	Name() string
}

// LocalBackend 进程内令牌桶（默认实现，单副本语义）。
type LocalBackend struct{ l *Limiter }

// NewLocalBackend 创建进程内限流后端。
func NewLocalBackend() *LocalBackend { return &LocalBackend{l: NewLimiter()} }

// Allow 走本地令牌桶。
func (b *LocalBackend) Allow(key string, p config.LimitPolicy) (bool, time.Duration) {
	return b.l.Bucket(key, p).Allow(1)
}

// Name 实现 Backend。
func (b *LocalBackend) Name() string { return "local" }

// Len 返回当前桶数量（观测用）。
func (b *LocalBackend) Len() int { return b.l.Len() }

// ---- 共享（HTTP）后端 ----

// limitRequest / limitResponse 是共享限流后端的线上协议（JSON over HTTP）。
//
// 为什么用 HTTP 而不是 Redis：项目定位是「除 grpc 外只用标准库」，
// 引一个 Redis 客户端会破坏它；而 HTTP 用 net/http 就能实现，
// 且状态服务可以用**任何语言**写（`cmd/statestore` 是 Go 参考实现）。
// 代价是每次判定多一次网络往返 —— 所以只在确实需要跨副本一致时打开（见 config.RateLimit）。
type limitRequest struct {
	Key        string  `json:"key"`
	RatePerSec float64 `json:"rate_per_sec"`
	Burst      int     `json:"burst"`
}

type limitResponse struct {
	Allow        bool `json:"allow"`
	RetryAfterMs int  `json:"retry_after_ms"`
}

// HTTPBackend 把限流判定交给一个共享的状态服务。
//
// ⚠️ 失败策略必须显式想清楚：状态服务挂了的时候，是「全部拒绝」还是「全部放行」？
//   - fail-closed（全拒）：状态服务一次抖动就把自己的网关打成 503，比被刷更糟；
//   - fail-open（全放，默认）：限流暂时失效，但服务还活着。
//
// 所以默认 fail-open，**并且必须计数**（OnError → gw_shared_state_errors_total）：
// 静默放行等于把限流失效藏起来，只有指标能让你发现它。
type HTTPBackend struct {
	url      string
	client   *http.Client
	failOpen bool
	// OnError 在判定失败（超时/连不上/非 2xx/解析失败）时被调用，用于打点。
	OnError func(error)
}

// NewHTTPBackend 创建共享限流后端。timeout <= 0 时用 200ms。
func NewHTTPBackend(url string, timeout time.Duration, failOpen bool) *HTTPBackend {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	return &HTTPBackend{
		url:      url,
		failOpen: failOpen,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil, // 明确不走环境变量里的代理（否则会把状态服务请求也代理走）
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Name 实现 Backend。
func (b *HTTPBackend) Name() string { return "http:" + b.url }

// Allow 询问共享状态服务。
func (b *HTTPBackend) Allow(key string, p config.LimitPolicy) (bool, time.Duration) {
	body, err := json.Marshal(limitRequest{Key: key, RatePerSec: p.RatePerSec, Burst: p.Burst})
	if err != nil {
		return b.degrade(err)
	}
	req, err := http.NewRequest(http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		return b.degrade(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return b.degrade(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return b.degrade(fmt.Errorf("状态服务返回 %d", resp.StatusCode))
	}
	// 限制读取长度：状态服务的响应应该很小，读到异常大的内容说明对接错了
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return b.degrade(err)
	}
	var out limitResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return b.degrade(fmt.Errorf("解析状态服务响应失败: %w", err))
	}
	if out.Allow {
		return true, 0
	}
	return false, time.Duration(out.RetryAfterMs) * time.Millisecond
}

// degrade 处理"判定失败"：按 fail-open/fail-closed 决策并打点。
func (b *HTTPBackend) degrade(err error) (bool, time.Duration) {
	if b.OnError != nil {
		b.OnError(err)
	}
	return b.failOpen, 0
}
