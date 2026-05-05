package main

import (
	"context"
	"fmt"
	"os"

	"github.com/AoManoh/openace-mcp/internal/ace"
	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/mcp"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func main() {
	ctx := context.Background()

	loader := auth.NewLoader()
	client := ace.NewClient(loader)
	syncer := workspace.NewSyncer(client)
	server := mcp.NewServer(syncer)

	if err := server.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "openace-mcp: %v\n", err)
		os.Exit(1)
	}
}
