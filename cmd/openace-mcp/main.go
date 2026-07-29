package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/managed"
	"github.com/AoManoh/openace-mcp/internal/mcp"
	"github.com/AoManoh/openace-mcp/internal/provider"
	"github.com/AoManoh/openace-mcp/internal/workspace"
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

	if err := server.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", err)
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
	loader := auth.NewLoader()
	profiles, err := loader.LoadProfiles(ctx)
	if err != nil {
		return nil, err
	}
	registry, err := provider.NewRegistry(profiles)
	if err != nil {
		return nil, err
	}
	return workspace.NewSyncerWithRouter(registry), nil
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
