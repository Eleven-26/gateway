// Command statestore 是「共享限流状态」的参考实现。
//
// 它做的事很小：维护一批令牌桶，把判定结果通过 HTTP 告诉网关。之所以需要它，是因为
// 网关默认的限流是**进程内**的 —— 多副本时每个副本各有一份桶，实际放行量变成
// 「副本数 × 阈值」。把判定挪到这个进程，多副本才共享同一个桶。
//
// 协议（JSON over HTTP，见 internal/ratelimit/backend.go）：
//
//	POST /ratelimit  {"key":"route:x|user:u","rate_per_sec":5,"burst":2}
//	  → 200 {"allow":true}
//	  → 200 {"allow":false,"retry_after_ms":200}
//	GET  /healthz    → 200 {"status":"ok","buckets":3}
//
// 用法：
//
//	go run ./cmd/statestore -listen 127.0.0.1:18090
//	# 网关侧：GW_CONFIG 里配 "rate_limit": {"url": "http://127.0.0.1:18090/ratelimit"}
//
// ⚠️ 生产注意：
//   - 它成了**新的单点**：要么多副本（每个副本各管一部分 key 就不一致了，所以要么做无状态共享存储，
//     要么用一致性哈希把 key 分片并让网关知道分片规则）；要么保证快速重启；
//   - 它不做持久化 —— 重启后桶会重置（限流短暂放宽），这比"重启后永久拒绝"安全；
//   - 它必须限制 key 的数量：本实现复用 ratelimit 的空闲回收（3 分钟），否则一个扫描器就能把内存打满。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gwlab/internal/config"
	"gwlab/internal/ratelimit"
)

type limitRequest struct {
	Key        string  `json:"key"`
	RatePerSec float64 `json:"rate_per_sec"`
	Burst      int     `json:"burst"`
}

type limitResponse struct {
	Allow        bool `json:"allow"`
	RetryAfterMs int  `json:"retry_after_ms"`
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18090", "监听地址")
	flag.Parse()

	// 直接复用网关自己的令牌桶实现：状态服务与本地限流必须行为一致，
	// 否则"切到共享限流"会变成一次语义变更（而不是单纯把状态挪个地方）。
	backend := ratelimit.NewLocalBackend()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","buckets":%d}`+"\n", backend.Len())
	})
	mux.HandleFunc("/ratelimit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if err != nil {
			http.Error(w, `{"error":"read_failed"}`, http.StatusBadRequest)
			return
		}
		var req limitRequest
		if err := json.Unmarshal(raw, &req); err != nil || req.Key == "" || req.RatePerSec <= 0 {
			http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
			return
		}
		allow, retry := backend.Allow(req.Key, config.LimitPolicy{RatePerSec: req.RatePerSec, Burst: req.Burst})
		out := limitResponse{Allow: allow}
		if !allow {
			out.RetryAfterMs = int(retry.Milliseconds())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	// ---- 共享熔断状态----
	//
	// 存储极简（一个 map），但有两件事必须做对：
	//  ① 只保留**未过期**的条目，否则一个服务名写错就能让这个 map 无限增长；
	//  ② GET 时顺手清理过期项（懒清理，不需要额外的 GC 协程）。
	// 语义是"打开到什么时候"，不是"现在是否打开"：网关采纳后会自己走半开恢复流程。
	var (
		bmu       sync.Mutex
		openUntil = map[string]int64{} // service → unix millis
	)
	mux.HandleFunc("/breaker", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
			if err != nil {
				http.Error(w, `{"error":"read_failed"}`, http.StatusBadRequest)
				return
			}
			var req struct {
				Service string `json:"service"`
				UntilMs int64  `json:"until_ms"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || req.Service == "" || req.UntilMs <= 0 {
				http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
				return
			}
			bmu.Lock()
			openUntil[req.Service] = req.UntilMs
			bmu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))

		case http.MethodGet:
			now := time.Now().UnixMilli()
			out := map[string]int64{}
			bmu.Lock()
			for name, until := range openUntil {
				if until > now {
					out[name] = until
				} else {
					delete(openUntil, name)
				}
			}
			bmu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"services": out})

		default:
			http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		}
	})
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("statestore shutting down ...")
		_ = srv.Close()
	}()

	log.Printf("statestore listening on http://%s  (POST /ratelimit, GET /healthz)", *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
