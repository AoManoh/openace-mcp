package daemon

import (
	"context"
	"os"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Status struct {
	Status string `json:"status"`
	runtimeinfo.ServedBy
	ActiveWorkspaceCount int    `json:"active_workspace_count"`
	WorkspaceStatusError string `json:"workspace_status_error,omitempty"`
}

func capabilities() map[string]bool {
	return map[string]bool{
		"provider_profiles":          true,
		"runtime_identity":           true,
		"workspace_canonicalization": true,
	}
}

func (s *Server) statusSnapshot(ctx context.Context) Status {
	status := Status{
		Status:   "ok",
		ServedBy: s.servedBy(),
	}
	if inspector, ok := s.workspaceInspector(); ok {
		workspaces, err := inspector.ListWorkspaceStatuses(ctx)
		if err == nil {
			status.ActiveWorkspaceCount = len(workspaces)
		} else {
			status.WorkspaceStatusError = err.Error()
		}
	}
	return status
}

func (s *Server) servedBy() runtimeinfo.ServedBy {
	identity := runtimeinfo.ServedBy{
		Service:      "openace-daemon",
		Capabilities: capabilities(),
		PID:          os.Getpid(),
		StartedAt:    s.startedAt,
		ListenAddr:   s.currentListenAddr(),
		Build:        buildinfo.Current(),
	}
	if cache, err := workspace.CurrentCacheSnapshot(); err == nil {
		identity.CacheDir = cache.Dir
		identity.CacheNamespace = cache.Namespace
	}
	return identity
}
