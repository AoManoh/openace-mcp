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

// DefaultFullResults 是 detail=full 时默认带正文返回的候选块数;其后的
// 候选只返回 `path:start-end symbol` 头行。该值由使用者经 MCP 配置
// (OPENACE_FULL_RESULTS)调整,不是调用方 AI 的逐次参数。
const DefaultFullResults = 20

// SearchRequest 描述一次工作区检索请求。
type SearchRequest struct {
	Workspace WorkspaceRef
	Query     string
	// FullResults 是 detail=full 时带正文返回的候选块数上限(按排名取
	// 前 N 个);其余候选以头行列出,不再按字节预算截断。0(零值,直接
	// 构造请求的调用方常见)表示未指定,引擎按 DefaultFullResults 处理;
	// 负值表示"一个都不带正文"(使用者配置 OPENACE_FULL_RESULTS=0 时由
	// wrapper 翻译为负值传入)。
	FullResults int
	// Detail 是输出详略:""/"full"=前 FullResults 个候选带正文、其余头行;
	// "paths"=全部只回 path:range 头行,内容由调用方按需 Read。
	Detail string
	// PathPrefix 可选索引相对路径前缀(如 internal/localengine):
	// 融合后/rerank 前过滤候选,用于 repo_map 定向后的子树检索。
	PathPrefix string
	// ArtifactKind 是调用方明示的产物类型:""/"any"=不分组(现行为);
	// "code"/"tests"/"docs"=精排之后把该类型候选按原相对顺序排到最前,其余
	// 候选原序跟随,不丢弃任何候选。类型按路径机械规则判定(见
	// localengine.artifactKind),不做意图推断。非法取值按请求类错误拒绝。
	// 依据:2026-09-03 D1 H1 实验——django 400 条"找实现"查询,精排把测试/
	// 文档排在实现之上,前五命中率因此低 8.75pp;伤害在精排窗口之内,只需
	// 输出层按类型分组即可拿回。
	ArtifactKind string
}

// RepoMapRequest 是仓库地图请求(repo_map R1,D4):快照只读,冷仓
// 显式 not-ready,永不隐式触发索引或 provider 调用。
type RepoMapRequest struct {
	Workspace    WorkspaceRef
	MaxOutputLen int
	// Focus 可选路径前缀,只出该子树(全仓→子树逐层导航)。
	Focus string
}

// RepoMapper 是可选能力:按现役 revision 产出预算内仓库地图
// (orientation 面,不进检索排序)。
type RepoMapper interface {
	RepoMap(context.Context, RepoMapRequest) (Result, error)
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
