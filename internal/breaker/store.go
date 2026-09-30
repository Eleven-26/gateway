package breaker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Store 是跨副本共享熔断状态的接口（审计 C4）。
//
// 为什么需要：熔断器是**进程内**状态，多副本时每个副本各学各的 ——
// 结果就是"副本 A 已经把某个服务熔断了，副本 B 还在往那个服务打流量"，
// 上游实际收到的压力是阈值的 副本数 倍。共享状态让一个副本学到的结论对其它副本也生效。
//
// 两个方向的语义刻意不对称：
//   - Publish：**只在本地跳闸时**调用（一个方向，避免副本之间互相转发形成发布风暴）；
//   - Snapshot：周期性拉取，把"别人已经熔断到 T"采纳为本地状态（见 Breaker.ForceOpenUntil）。
//
// 接口只有这两个方法，是因为熔断状态的可共享部分就是"打开到什么时候" ——
// 滑动窗口的原始计数不适合、也不需要跨副本共享（各副本的样本本来就是不同的流量）。
type Store interface {
	// Publish 发布"某服务打开到 until"。
	Publish(service string, until time.Time) error
	// Snapshot 拉取所有服务的打开状态（未打开/已过期的服务不应出现在返回值里）。
	Snapshot() (map[string]time.Time, error)
}

// HTTPStore 用 HTTP 与共享状态服务交换熔断状态（协议与 cmd/statestore 一致）：
//
//	POST /breaker  {"service":"order-svc","until_ms":1730000000000}
//	GET  /breaker  → {"services":{"order-svc":1730000000000}}
//
// 选择 HTTP 而非 Redis 的理由与限流后端相同（见 internal/ratelimit/backend.go）。
type HTTPStore struct {
	base    string // 形如 http://127.0.0.1:18090（不含路径）
	client  *http.Client
	onError func(error)
}

// NewHTTPStore 创建共享熔断状态存储。timeout <= 0 时用 200ms。
func NewHTTPStore(base string, timeout time.Duration) *HTTPStore {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	return &HTTPStore{
		base: base,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// OnError 设置失败回调（打点用）。共享状态读不到**不影响本地熔断**：
// 本地状态机仍然完整工作，只是退化回"单副本语义"——这是刻意的降级策略。
func (s *HTTPStore) OnError(f func(error)) { s.onError = f }

func (s *HTTPStore) fail(err error) error {
	if s.onError != nil {
		s.onError(err)
	}
	return err
}

// Publish 上报一次本地跳闸。
func (s *HTTPStore) Publish(service string, until time.Time) error {
	body, err := json.Marshal(map[string]any{"service": service, "until_ms": until.UnixMilli()})
	if err != nil {
		return s.fail(err)
	}
	req, err := http.NewRequest(http.MethodPost, s.base+"/breaker", bytes.NewReader(body))
	if err != nil {
		return s.fail(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return s.fail(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return s.fail(fmt.Errorf("共享状态服务返回 %d", resp.StatusCode))
	}
	return nil
}

// Snapshot 拉取所有服务的打开状态。
func (s *HTTPStore) Snapshot() (map[string]time.Time, error) {
	resp, err := s.client.Get(s.base + "/breaker")
	if err != nil {
		return nil, s.fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, s.fail(fmt.Errorf("共享状态服务返回 %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, s.fail(err)
	}
	var doc struct {
		Services map[string]int64 `json:"services"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, s.fail(fmt.Errorf("解析共享状态失败: %w", err))
	}
	out := make(map[string]time.Time, len(doc.Services))
	for name, ms := range doc.Services {
		out[name] = time.UnixMilli(ms)
	}
	return out, nil
}
