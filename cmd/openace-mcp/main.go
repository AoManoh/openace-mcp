package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/managed"
	"github.com/AoManoh/openace-mcp/internal/mcp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		runDaemon()
		return
	}

	ctx := context.Background()

	service, err := buildService(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", err)
		os.Exit(1)
	}
	server := mcp.NewServer(service)

	runErr := server.Run(ctx, os.Stdin, os.Stdout)
	// direct 模式下引擎持有本地句柄，退出前有序释放（review S7，
	// 与 daemon Shutdown 路径对称）。
	if lifecycle, ok := service.(engine.Lifecycle); ok {
		if err := lifecycle.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "openace-mcp: close engine: %v\n", err)
		}
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", runErr)
		os.Exit(1)
	}
}

func buildService(ctx context.Context) (engine.Service, error) {
	switch openaceMode() {
	case "direct":
		return buildLocalService(ctx)
	case "manual-daemon":
		return daemon.NewClient(daemonAddr()), nil
	case "auto":
		return managed.Connect(ctx)
	default:
		return nil, fmt.Errorf("invalid OPENACE_MODE %q; use auto, direct, or manual-daemon", os.Getenv("OPENACE_MODE"))
	}
}

func openaceMode() string {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("OPENACE_MODE")))
	switch mode {
	case "", "auto", "managed", "managed-daemon":
		return "auto"
	case "direct":
		return "direct"
	case "manual", "daemon", "manual-daemon":
		return "manual-daemon"
	default:
		return mode
	}
}

func daemonAddr() string {
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_ADDR")); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("OPENACE_DAEMON_LISTEN_ADDR")); addr != "" {
		return addr
	}
	return daemon.DefaultAddr
}

func buildLocalService(ctx context.Context) (engine.Service, error) {
	_ = ctx
	// Stage 7:legacy ACE 引擎已删除,唯一引擎 = local-hybrid;
	// NormalizeEngineID 对 "ace" 给出可行动错误。零凭据可启动
	// (缺 key = semantic off,词法照常,阶段计划 D1)。
	if _, err := engine.NormalizeEngineID(os.Getenv("OPENACE_ENGINE")); err != nil {
		return nil, err
	}
	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		return nil, err
	}
	return localengine.New(opts)
}

func runDaemon() {
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
