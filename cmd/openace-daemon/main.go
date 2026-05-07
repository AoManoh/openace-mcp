package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/daemon"
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

	syncer, err := buildLocalSyncer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openace-daemon: %v\n", err)
		os.Exit(1)
	}
	server := daemon.NewServer(syncer)

	fmt.Fprintf(os.Stderr, "openace-daemon: listening on %s\n", addr)
	if err := server.ListenAndServe(ctx, addr); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "openace-daemon: %v\n", err)
		os.Exit(1)
	}
}

func buildLocalSyncer(ctx context.Context) (*workspace.Syncer, error) {
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
