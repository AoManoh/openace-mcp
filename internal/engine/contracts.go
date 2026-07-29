// Package engine 定义 openACE 检索引擎的通用 contract：接口、请求与结果类型。
//
// 依赖方向恒定为 cmd/mcp/daemon -> engine <- 具体实现（迁移期 workspace/ace，
// 后续 localengine）；本包不得导入 workspace、ace、mcp、daemon。
//
// Stage 1 决定：迁移方案 §7.2 伪代码中的 SyncResult/SearchResult 统一为共享
// Result 载体——当前 legacy ACE 行为下二者本就共享同一结构（daemon 任务快照
// 也以单一载体持久化），提前拆分会引入第三个载体类型与 JSON 兼容风险；拆分
// 决策推迟到 SearchResult 引入结构化 hits 的阶段执行。
package engine

import "context"

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
