// Package engine 定义 openACE 检索引擎的通用 contract：接口、请求与结果类型。
//
// 依赖方向恒定为 cmd/mcp/daemon -> engine <- 具体实现（local-hybrid）；
// 本包不得导入 workspace、mcp、daemon。
//
// Stage 1 决定：迁移方案 §7.2 伪代码中的 SyncResult/SearchResult 统一为共享
// Result 载体——当前 legacy ACE 行为下二者本就共享同一结构（daemon 任务快照
// 也以单一载体持久化），提前拆分会引入第三个载体类型与 JSON 兼容风险；拆分
// 决策推迟到 SearchResult 引入结构化 hits 的阶段执行。
package engine

import (
	"context"
	"fmt"
	"strings"
)

// 引擎标识常量；OPENACE_ENGINE 环境变量取值。
const (
	EngineLocalHybrid = "local-hybrid"
)

// NormalizeEngineID 规范化引擎选择：空值默认 local-hybrid(Stage 6,
// 2026-08-02 批准);legacy "ace" 已于 Stage 7(2026-08-04 用户裁决)删除,
// 显式给出可行动错误;其余非法值显式报错(不静默回退)。
func NormalizeEngineID(value string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", EngineLocalHybrid:
		return EngineLocalHybrid, nil
	case "ace":
		return "", fmt.Errorf("OPENACE_ENGINE=ace 已在 Stage 7 移除(legacy 上游引擎退役);删除该 env 或设为 %q", EngineLocalHybrid)
	default:
		return "", fmt.Errorf("invalid OPENACE_ENGINE %q; use %q", value, EngineLocalHybrid)
	}
}

// Identifier 是可选能力：实现方自述引擎类型，供 daemon 身份广播
// 与跨进程复用兼容性判定使用（阶段计划暗坑 K8）。
type Identifier interface {
	EngineID() string
}

// ProfileIdentifier 是可选能力：实现方自述引擎配置指纹（provider 身份与
// 降级开关的 hash，不含任何凭据与运维参数），供 daemon 复用兼容性判定——
// 用户改 provider env 后不得静默复用旧配置 daemon（Stage 3 暗坑 K29）。
type ProfileIdentifier interface {
	EngineProfileFingerprint() string
}

// WorkspaceRef 标识一次请求指向的工作区与引擎/档案身份。
// DirectoryPath 允许是调用方提供的原始路径；canonical 化由实现负责。
type WorkspaceRef struct {
	DirectoryPath string
	// Engine 是目标引擎标识；迁移期唯一实现是 legacy ACE，恒为空。
	Engine string
	// ProviderProfileID 是 legacy ACE 的 provider 档案标识；空值表示默认档案。
	// local-hybrid 实现收到非空值时必须返回明确错误，禁止静默忽略。
	ProviderProfileID string
}

// SyncRequest 描述一次工作区索引同步请求。
type SyncRequest struct {
	Workspace WorkspaceRef
}

// SearchRequest 描述一次工作区检索请求。
type SearchRequest struct {
	Workspace    WorkspaceRef
	Query        string
	MaxOutputLen int
}

// Service 是检索引擎的核心行为契约：同步索引与执行检索。
type Service interface {
	Sync(context.Context, SyncRequest) (Result, error)
	Search(context.Context, SearchRequest) (Result, error)
}

// WorkspaceInspector 暴露工作区状态查询能力。
type WorkspaceInspector interface {
	WorkspaceStatus(context.Context, WorkspaceRef) (WorkspaceStatus, error)
	ListWorkspaceStatuses(context.Context) ([]WorkspaceStatus, error)
}

// ChangeDetector 判断工作区自上次索引后是否发生变化。
type ChangeDetector interface {
	WorkspaceChanged(context.Context, WorkspaceRef) (bool, error)
}

// BackgroundSyncer 以后台低优先级语义执行同步。
type BackgroundSyncer interface {
	SyncBackground(context.Context, SyncRequest) (Result, error)
}

// Lifecycle 由持有本地资源（索引句柄、后台任务）的引擎实现，
// 用于在宿主进程退出前有序释放。
type Lifecycle interface {
	Close(context.Context) error
}
