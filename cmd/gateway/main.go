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
	cfg := config.Default()

	gw, err := gateway.New(cfg)
	if err != nil {
		return err
	}
	defer gw.Close()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           gw,
		ReadHeaderTimeout: 3 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	printBanner(cfg)

	// 优雅退出：收到中断后停止接收新连接，给在途请求 10 秒收尾时间。
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("\ngateway shutting down ...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idle
	return nil
}

func printBanner(cfg *config.Config) {
	fmt.Printf("gateway listening on http://%s  (routes=%d services=%d)\n", cfg.ListenAddr, len(cfg.Routes), len(cfg.Services))
	fmt.Printf("  /metrics      指标    /debug/logs  最近访问日志\n")
	for _, rt := range cfg.Routes {
		fmt.Printf("  route %-14s host=%-16s path=%-22s type=%-6s -> %s\n",
			rt.Name, observability.OrDash(rt.Host), rt.Path, rt.PathType, rt.Upstream)
	}
}
