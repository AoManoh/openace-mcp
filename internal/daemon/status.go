package daemon

import (
	"context"
	"os"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Status struct {
	Status string `json:"status"`
	runtimeinfo.ServedBy
	ActiveWorkspaceCount int    `json:"active_workspace_count"`
	WorkspaceStatusError string `json:"workspace_status_error,omitempty"`
}

func capabilities(engineID string) map[string]bool {
	return map[string]bool{
		"provider_profiles":          engineID == engine.EngineACE,
		"runtime_identity":           true,
		"workspace_canonicalization": true,
		"engine_local_hybrid":        engineID == engine.EngineLocalHybrid,
	}
}

// engineID 返回当前 daemon 运行的引擎标识；无法自述时按 legacy ACE 处理。
func (s *Server) engineID() string {
	if identifier, ok := s.service.(engine.Identifier); ok {
		return identifier.EngineID()
	}
	return engine.EngineACE
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
	engineID := s.engineID()
	identity := runtimeinfo.ServedBy{
		Service:      "openace-daemon",
		Engine:       engineID,
		Capabilities: capabilities(engineID),
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
