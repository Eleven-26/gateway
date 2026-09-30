// Command gateway 启动 API 网关。
//
// 用法：
//
//	go run ./cmd/gateway
//
// 默认监听 127.0.0.1:18080，路由与上游要求本机 19001/19002/19003 与 19100
// 上有演示后端在跑（见 cmd/backend）。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gwlab/internal/config"
	"gwlab/internal/gateway"
	"gwlab/internal/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway 退出:", err)
		os.Exit(1)
	}
}

func run() error {
	// 配置来源：内置默认 → GW_CONFIG 指向的 JSON → 环境变量覆盖 → 启动期校验（审计 C1）。
	// 校验不过就直接退出：错误配置必须在启动期暴露，而不是等某个请求变成 500。
	cfg, source, err := config.Load()
	if err != nil {
		return err
	}

	gw, err := gateway.New(cfg)
	if err != nil {
		return err
	}
	defer gw.Close()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           gw,
		ReadHeaderTimeout: 3 * time.Second,
		// ReadTimeout 覆盖「请求体读取」：只设 ReadHeaderTimeout 时，慢速滴流的 body
		// 能长期占住连接与 goroutine（审计 P0-4）。
		ReadTimeout: 30 * time.Second,
		// WriteTimeout 由配置里最大的 Service.Timeout 推导 + 余量，必须容得下最慢的上游往返。
		// ⚠️ 它会掐断超过该时长的「长连接流式响应」（SSE）；要支持长流需按路由放宽，
		// 或在该路由里用 http.ResponseController.SetWriteDeadline 续期。
		WriteTimeout: cfg.MaxServiceTimeout() + 15*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// 管理端点单独监听（审计 P1-6）：业务端口不再暴露 /metrics、/debug/logs、/readyz。
	// 生产可以只让管理端口绑定回环或内网；AdminListenAddr 为空则退回「与业务同端口」。
	var adminSrv *http.Server
	if cfg.AdminListenAddr != "" {
		adminSrv = &http.Server{
			Addr:              cfg.AdminListenAddr,
			Handler:           gw.AdminHandler(),
			ReadHeaderTimeout: 3 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "admin 监听失败:", err)
			}
		}()
	}

	printBanner(cfg, source)

	// 配置热重载（审计 C1）：轮询 GW_CONFIG 指向文件的 mtime，变了就重新加载并原地替换。
	// 为什么轮询：标准库没有跨平台的 inotify，而 Windows 没有 SIGHUP —— 轮询在两个平台上行为一致。
	// 失败时**保留旧配置**并打日志：把配置写坏不应该让在跑的网关挂掉。
	if path := strings.TrimSpace(os.Getenv("GW_CONFIG")); path != "" {
		go watchConfig(path, gw)
	}

	// 优雅退出：收到中断后停止接收新连接，给在途请求 10 秒收尾时间。
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("\ngateway shutting down ...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if adminSrv != nil {
			_ = adminSrv.Shutdown(ctx)
		}
		_ = srv.Shutdown(ctx)
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idle
	return nil
}

func printBanner(cfg *config.Config, source string) {
	fmt.Printf("gateway listening on http://%s  (routes=%d services=%d)\n", cfg.ListenAddr, len(cfg.Routes), len(cfg.Services))
	fmt.Printf("  config        %s\n", source)
	if cfg.Overload.MaxInflight > 0 {
		// 打印**生效值**：QueueTimeout 没配时实际用的是 1s 默认值，
		// 只打印配置值会让人误以为"排队立刻超时"。
		if cfg.Overload.MaxQueue == 0 {
			fmt.Printf("  overload     并发上限 %d，不排队（并发满立刻 503 + Retry-After）\n", cfg.Overload.MaxInflight)
		} else {
			qto := time.Duration(cfg.Overload.QueueTimeout)
			if qto <= 0 {
				qto = time.Second
			}
			fmt.Printf("  overload     并发上限 %d，排队 %d，排队超时 %s\n",
				cfg.Overload.MaxInflight, cfg.Overload.MaxQueue, qto)
		}
	}
	if cfg.SharedState.URL != "" {
		fmt.Printf("  shared state  %s（限流桶 + 熔断状态跨副本共享）\n", cfg.SharedState.URL)
	} else {
		fmt.Printf("  shared state  local（限流与熔断都在进程内：多副本时限流阈值×副本数、熔断各算各的）\n")
	}
	if cfg.AdminListenAddr != "" {
		fmt.Printf("  admin         http://%s  (/metrics  /debug/logs  /readyz)\n", cfg.AdminListenAddr)
	} else {
		fmt.Printf("  /metrics      指标    /debug/logs  最近访问日志    /readyz  就绪检查（与业务同端口）\n")
	}
	for _, rt := range cfg.Routes {
		fmt.Printf("  route %-14s host=%-16s path=%-22s type=%-6s -> %s\n",
			rt.Name, observability.OrDash(rt.Host), rt.Path, rt.PathType, rt.Upstream)
	}
}

// watchConfig 每 3 秒看一次配置文件是否变化，变了就热重载。
//
// 3 秒是个折中：配置变更不是高频操作，而 3 秒的生效延迟远小于"重新编译 + 重启"；
// 代价是每次 stat 一次文件（可忽略）。要更快可以换成 fsnotify，但要引第三方依赖，
// 与项目"除 grpc 外只用标准库"的定位冲突。
//
// 重载失败（JSON 写坏、校验不过）时只打日志、**不改变正在服务的配置** —— 这是热重载最重要的性质：
// 一次手抖不该把线上打挂。
func watchConfig(path string, gw *gateway.Gateway) {
	last := time.Time{}
	if fi, err := os.Stat(path); err == nil {
		last = fi.ModTime()
	}
	for range time.Tick(3 * time.Second) {
		fi, err := os.Stat(path)
		if err != nil {
			continue // 文件被临时挪走（编辑器保存的常见行为）：下一轮再看
		}
		if !fi.ModTime().After(last) {
			continue
		}
		last = fi.ModTime()

		cfg, _, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "热重载失败，保留旧配置:", err)
			continue
		}
		sum, err := gw.Reload(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "热重载失败，保留旧配置:", err)
			continue
		}
		fmt.Printf("配置已热重载: %s\n", sum)
	}
}
