package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opsagent/internal/cli"
	"opsagent/internal/observability"
)

func main() {
	// Ctrl+C 时取消进行中的采集与调用。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// OTel 未配置时 Init 为 noop。
	shutdown, err := observability.Init(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 追踪初始化失败（%v），继续执行\n", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdown != nil {
			_ = shutdown(flushCtx)
		}
	}()

	if err := cli.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
