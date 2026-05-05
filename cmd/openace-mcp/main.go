package main

import (
	"context"
	"fmt"
	"os"

	"github.com/AoManoh/openace-mcp/internal/ace"
	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/mcp"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func main() {
	ctx := context.Background()

	syncer := buildSyncer()
	server := mcp.NewServer(syncer)

	if err := server.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", err)
		os.Exit(1)
	}
}

func buildSyncer() mcp.Syncer {
	if addr := os.Getenv("OPENACE_DAEMON_ADDR"); addr != "" {
		return daemon.NewClient(addr)
	}
	loader := auth.NewLoader()
	client := ace.NewClient(loader)
	return workspace.NewSyncer(client)
}
