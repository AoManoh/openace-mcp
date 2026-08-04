package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("OPENACE_DAEMON_LISTEN_ADDR")
	if addr == "" {
		addr = os.Getenv("OPENACE_DAEMON_ADDR")
	}
	if addr == "" {
		addr = daemon.DefaultAddr
	}

	service, err := buildLocalService(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openace-daemon: %v\n", err)
		os.Exit(1)
	}
	server := daemon.NewServer(service)

	fmt.Fprintf(os.Stderr, "openace-daemon: listening on %s\n", addr)
	if err := server.ListenAndServe(ctx, addr); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "openace-daemon: %v\n", err)
		os.Exit(1)
	}
}

func buildLocalService(ctx context.Context) (engine.Service, error) {
	_ = ctx
	// Stage 7:legacy ACE 引擎已删除,唯一引擎 = local-hybrid;零凭据
	// 可启动(缺 key = semantic off,词法照常,阶段计划 D1)。
	if _, err := engine.NormalizeEngineID(os.Getenv("OPENACE_ENGINE")); err != nil {
		return nil, err
	}
	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		return nil, err
	}
	return localengine.New(opts)
}
