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
	// ProviderUsage 是本 daemon 启动以来向模型服务商成功送审、服务商在响应里
	// 报告的用量合计（全部工作区）。各工作区的分项在 workspace_status 的
	// semantic.usage。
	ProviderUsage *engine.ProviderUsage `json:"provider_usage,omitempty"`
	// LaunchedBy 是拉起本 daemon 的 wrapper 进程（见 EnvLaunchedBy）；手动
	// 启动时为空。
	LaunchedBy *LaunchInfo `json:"launched_by,omitempty"`
	// EngineProfileComponents 是配置指纹各组成项的说明（不含凭据）。只放在
	// daemon 状态里、不随每个响应的 served_by 携带；wrapper 在指纹不一致时
	// 读取它，逐项说明哪项设置与本会话不同。
	EngineProfileComponents map[string]string `json:"engine_profile_components,omitempty"`
}

func capabilities(engineID string) map[string]bool {
	return map[string]bool{
		// provider_profiles 随 legacy 引擎在 Stage 7 移除,恒 false;
		// 保留键位是 wire 兼容(旧 wrapper 读该键判能力)。
		"provider_profiles":          false,
		"runtime_identity":           true,
		"workspace_canonicalization": true,
		"engine_local_hybrid":        engineID == engine.EngineLocalHybrid,
	}
}

// engineID 返回当前 daemon 运行的引擎标识(Stage 7 后唯一实现是
// local-hybrid;Identifier 自述保留作防御)。
func (s *Server) engineID() string {
	if identifier, ok := s.service.(engine.Identifier); ok {
		return identifier.EngineID()
	}
	return engine.EngineLocalHybrid
}

func (s *Server) statusSnapshot(ctx context.Context) Status {
	status := Status{
		Status:     "ok",
		ServedBy:   s.servedBy(),
		LaunchedBy: s.launchedBy,
	}
	if inspector, ok := s.workspaceInspector(); ok {
		workspaces, err := inspector.ListWorkspaceStatuses(ctx)
		if err == nil {
			status.ActiveWorkspaceCount = len(workspaces)
		} else {
			status.WorkspaceStatusError = err.Error()
		}
	}
	if reporter, ok := s.service.(engine.UsageReporter); ok {
		usage := reporter.ProviderUsage()
		status.ProviderUsage = &usage
	}
	if reporter, ok := s.service.(engine.ProfileComponentReporter); ok {
		status.EngineProfileComponents = reporter.EngineProfileComponents()
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
	// 引擎配置指纹：复用判定要求与请求方一致（Stage 3 暗坑 K29）。
	if profiled, ok := s.service.(engine.ProfileIdentifier); ok {
		identity.EngineProfile = profiled.EngineProfileFingerprint()
	}
	if cache, err := workspace.CurrentCacheSnapshot(); err == nil {
		identity.CacheDir = cache.Dir
		identity.CacheNamespace = cache.Namespace
	}
	return identity
}
