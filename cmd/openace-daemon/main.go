package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/provider"
	"github.com/AoManoh/openace-mcp/internal/workspace"
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
	engineID, err := engine.NormalizeEngineID(os.Getenv("OPENACE_ENGINE"))
	if err != nil {
		return nil, err
	}
	// local-hybrid 不依赖任何上游凭据：无 AUGMENT_* 也必须可启动。
	if engineID == engine.EngineLocalHybrid {
		return localengine.New(), nil
	}
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
