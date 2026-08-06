package engine

import (
	"fmt"
	"time"

	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
)

// IndexStage 表示工作区索引生命周期所处阶段。
type IndexStage string

const (
	IndexStageIdle          IndexStage = "idle"
	IndexStageScanning      IndexStage = "scanning"
	IndexStageReconciling   IndexStage = "reconciling"
	IndexStageUploading     IndexStage = "uploading"
	IndexStageCheckpointing IndexStage = "checkpointing"
	IndexStageReady         IndexStage = "ready"
	IndexStageFailed        IndexStage = "failed"

	// local-hybrid 引擎的构建阶段（发现阶段复用 IndexStageScanning）。
	IndexStageChunking   IndexStage = "chunking"
	IndexStageEmbedding  IndexStage = "embedding"
	IndexStageIndexing   IndexStage = "indexing"
	IndexStagePublishing IndexStage = "publishing"
)

// SyncReason 表示一次同步的触发来源。
type SyncReason string

const (
	SyncReasonManual     SyncReason = "manual"
	SyncReasonRetrieval  SyncReason = "retrieval"
	SyncReasonBackground SyncReason = "background"
)

// Result 是同步与检索共享的通用结果载体。
//
// 字段名与 JSON 形状迁移自 workspace.Result，保持既有 daemon HTTP 与任务
// 快照的 wire 兼容（未加 tag 的字段沿用 Go 默认键名，属于既有格式，不得
// 追加 tag 改变）。IndexRevision/Engine 是通用引擎字段：legacy ACE 路径
// 不填充（omitempty 保证 wire 不变），由后续本地引擎实现填充。
type Result struct {
	Text              string
	ProviderProfileID string `json:"provider_profile_id,omitempty"`
	CheckpointID      string
	IndexRevision     string `json:"index_revision,omitempty"`
	Engine            string `json:"engine,omitempty"`
	FileCount         int
	Uploaded          int
	Added             int
	Deleted           int
	MultiStatus       *MultiRetrievalStatus `json:"multi_status,omitempty"`
	ServedBy          *runtimeinfo.ServedBy `json:"served_by,omitempty"`

	// —— Stage 3 检索透明性字段（决策 11；全部 omitempty：legacy ACE 与
	// 纯词法正常路径不填充，wire 不变，暗坑 K34）——

	// RetrievalMode 是本次检索实际执行的链路：lexical / hybrid /
	// hybrid+rerank / lexical+rerank。
	RetrievalMode string `json:"retrieval_mode,omitempty"`
	// DegradedReason 非空表示结果处于降级（逗号分隔的稳定原因 token；
	// 文本首行同步携带 [DEGRADED] 横幅）。
	DegradedReason string `json:"degraded_reason,omitempty"`
	// SemanticCoverage 是语义索引覆盖率（如 "87%"；语义路未配置时为空）。
	SemanticCoverage string `json:"semantic_coverage,omitempty"`

	// —— 方案④ quality-strict 可机读质量字段（2026-08-02;omitempty,
	// legacy 与纯词法路径不填充,wire 不变）——

	// RerankSent 是实际送审精排的候选数（rerank 未配置或未生效时为 0）。
	RerankSent int `json:"rerank_sent,omitempty"`
	// QueryPlan 是路由分立查询规划的可审计记录(方案 -13):触发时为
	// 词法路实际使用的结构 token 变体(零命中回退时追加 " fallback=original"),
	// 未触发为空——原查询本身永不被改写或丢弃(调研护栏 7/8)。
	QueryPlan string `json:"query_plan,omitempty"`
	// QueryEmbedFailed 表示本次查询嵌入失败（语义路仅缺查询侧）。
	QueryEmbedFailed bool `json:"query_embed_failed,omitempty"`
	// EmbeddingProfile 是语义路身份短指纹（provider/model/dimension）。
	EmbeddingProfile string `json:"embedding_profile,omitempty"`
	// BuildMode 是本次 sync 的构建形态与成因(灰度反馈四 §6.1:重建
	// 原因不可见时调用方只能从进度条反猜):"delta" 或
	// "full:<first-build|lexical-selfheal|vector-repair|schema-upgrade|
	// compaction-segments|compaction-garbage|semantic-fill>";no-op 不
	// 发布时为空。加性 omitempty。
	BuildMode string `json:"build_mode,omitempty"`
	// Timings 是检索阶段耗时分解(框架 18.3/灰度候选 (e):热检索
	// 13.1s 无法归因是 sync、embed、rerank 还是渲染)。检索路径恒填,
	// sync 路径为空;加性 omitempty。
	Timings *RetrievalTimings `json:"timings,omitempty"`
	// Hits 是结构化命中清单(框架 18.2/S2):每个合并后候选块一条,
	// 含 shown 标记——"候选存在但没交给调用方"从此机器可读,调用方
	// 可对未展示 hit 定向 Read 续取。加性 omitempty,检索路径填充。
	Hits []Hit `json:"hits,omitempty"`
	// Display 是展示统计(候选块/实展块/实展文件/是否截断)。
	Display *DisplayStats `json:"display,omitempty"`
}

// Hit 是单个检索命中的结构化引用(合并后块粒度,与正文 header 同源)。
type Hit struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Symbol    string `json:"symbol,omitempty"`
	// Rank 是融合(+精排)后的展示序(1 起);Shown 表示正文实际包含
	// 该块内容(paths 模式=头行已列出)。
	Rank  int  `json:"rank"`
	Shown bool `json:"shown"`
}

// DisplayStats 是单次检索的展示完整性统计(框架 18.2)。
type DisplayStats struct {
	CandidateBlocks int  `json:"candidate_blocks"`
	ShownBlocks     int  `json:"shown_blocks"`
	ShownFiles      int  `json:"shown_files"`
	Truncated       bool `json:"truncated"`
}

// RetrievalTimings 是单次检索各阶段耗时(毫秒)。SyncMs 含查询前置
// (内联同步/有界等待/句柄获取);QueryEmbedMs/VectorMs 仅语义路;
// FuseMs 为 RRF 融合本体;RenderMs 含合并与预算渲染。
type RetrievalTimings struct {
	SyncMs       int64 `json:"sync_ms"`
	LexicalMs    int64 `json:"lexical_ms"`
	QueryEmbedMs int64 `json:"query_embed_ms,omitempty"`
	VectorMs     int64 `json:"vector_ms,omitempty"`
	FuseMs       int64 `json:"fuse_ms,omitempty"`
	RerankMs     int64 `json:"rerank_ms,omitempty"`
	RenderMs     int64 `json:"render_ms"`
	TotalMs      int64 `json:"total_ms"`
}

// Summary 输出一行人类可读的结果摘要，供 MCP 工具文本渲染使用。
// local-hybrid 无 checkpoint 概念，以 revision 口径输出（Stage 4 D8/S19，
// 文案变化仅影响实验性引擎，e2e golden 同步更新并在阶段记录声明）。
func (r Result) Summary() string {
	if r.Engine == EngineLocalHybrid && r.CheckpointID == "" {
		summary := fmt.Sprintf("revision=%s files=%d added_chunks=%d deleted=%d", r.IndexRevision, r.FileCount, r.Added, r.Deleted)
		// 构建形态外显(灰度反馈四 §6.1):delta/full 及其成因直说,
		// 调用方不再从进度与计数反猜"是否被全量重建"。
		if r.BuildMode != "" {
			summary += " build=" + r.BuildMode
		}
		// P8:同步面的语义覆盖如实外显(覆盖率恒随语义路携带;降级原因
		// 仅缺口时出现),direct/daemon 两形态同文案。
		if r.SemanticCoverage != "" {
			summary += " semantic_coverage=" + r.SemanticCoverage
		}
		if r.DegradedReason != "" {
			summary += " degraded=" + r.DegradedReason
		}
		return summary
	}
	if r.ProviderProfileID != "" {
		return fmt.Sprintf("provider_profile_id=%s checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.ProviderProfileID, r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
	}
	return fmt.Sprintf("checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
}

// SemanticStatus 描述 local-hybrid 语义路与精排 provider 的运行状态
// （Stage 3；整块 omitempty，legacy ACE 与纯词法零配置路径不出现，暗坑
// K34）。所有字段不含凭据（暗坑 K21）。
type SemanticStatus struct {
	// Enabled 为 false 时语义路未启用，DisabledReason 解释原因（off/缺 key）。
	Enabled        bool   `json:"enabled"`
	DisabledReason string `json:"disabled_reason,omitempty"`

	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Dimension int    `json:"dimension,omitempty"`

	// Coverage 是 active revision 的语义覆盖率（如 "87%"）；
	// CoveredChunks/TotalChunks 是其分子分母（暗坑 K31）。
	Coverage      string `json:"coverage,omitempty"`
	CoveredChunks int    `json:"covered_chunks,omitempty"`
	TotalChunks   int    `json:"total_chunks,omitempty"`
	// RejectedChunks 是被拒绝的非法向量数（零向量/NaN，暗坑 K35）。
	RejectedChunks int `json:"rejected_chunks,omitempty"`

	// 构建期 embedding 进度（Stage 4 D8，加性 omitempty，暗坑 K53）：
	// PendingChunks 是本次构建仍待嵌入的唯一内容数，EmbeddedChunks 是
	// 已完成数；构建结束后归零。JournalEntries 是断点续嵌暂存区中
	// 已付费未发布的向量条数（D4）。
	PendingChunks  int `json:"pending_chunks,omitempty"`
	EmbeddedChunks int `json:"embedded_chunks,omitempty"`
	JournalEntries int `json:"journal_entries,omitempty"`
	// EmbedRatePerMin/EmbedETASeconds 是构建期嵌入速率与剩余时间估算
	// (灰度反馈 2026-08-07:只见 pending 无 ETA;加性 omitempty,
	// 构建结束归零)。估算按本次构建平均速率,provider 限速波动下仅供
	// 参考。
	EmbedRatePerMin int `json:"embed_rate_per_min,omitempty"`
	EmbedETASeconds int `json:"embed_eta_seconds,omitempty"`

	// ProviderState 是 embedding circuit 状态：healthy/backoff/candidate；
	// backoff 时 BackoffUntil 指明恢复探测时间（§15）。
	ProviderState string     `json:"provider_state,omitempty"`
	BackoffUntil  *time.Time `json:"backoff_until,omitempty"`
	// LastError 是最近一次 provider 失败的脱敏消息。
	LastError string `json:"last_error,omitempty"`

	// Rerank* 描述精排 provider（未配置时 RerankDisabledReason 解释原因）。
	RerankProvider       string `json:"rerank_provider,omitempty"`
	RerankState          string `json:"rerank_state,omitempty"`
	RerankDisabledReason string `json:"rerank_disabled_reason,omitempty"`
}

// MultiRetrievalStatus 汇总一次跨工作区检索的每仓结果。
type MultiRetrievalStatus struct {
	ProviderProfileID string `json:"provider_profile_id,omitempty"`
	TotalWorkspaces   int    `json:"total_workspaces"`
	SuccessCount      int    `json:"success_count"`
	FailureCount      int    `json:"failure_count"`
	// DegradedCount 是成功但处于降级的工作区数（P2，review 二批；
	// omitempty 加性，wire 不变）。降级仓计入 SuccessCount 而非
	// FailureCount——结果可用，但质量受限须外显。
	DegradedCount  int                    `json:"degraded_count,omitempty"`
	PartialFailure bool                   `json:"partial_failure"`
	Workspaces     []MultiWorkspaceStatus `json:"workspaces"`
}

// DegradedBanner 返回跨仓降级聚合横幅（无降级仓时为空串）。放在聚合
// 文本首行，与单仓 [DEGRADED] 首行横幅同一心智模型（决策 11）；文本被
// 截断时每仓状态仍可经 multi_status 结构化字段获取（P3 兜底）。
func (s MultiRetrievalStatus) DegradedBanner() string {
	if s.DegradedCount == 0 {
		return ""
	}
	return fmt.Sprintf("[DEGRADED] %d of %d workspaces returned degraded results; see per-workspace banners and multi_status\n", s.DegradedCount, s.TotalWorkspaces)
}

// MultiWorkspaceStatus 是跨工作区检索中单个工作区的结果条目。
type MultiWorkspaceStatus struct {
	DirectoryPath string `json:"directory_path"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`

	// —— 每仓降级透明性字段（P2，review 二批：此前两条聚合路径都把
	// Result 的降级字段剥离，multi_status 里降级仓与健康仓同形——
	// "禁止静默降级"在 multi 工具面的缺口）。语义同 Result 的同名
	// 字段；全部 omitempty，错误仓与纯词法零配置路径不出现，wire 加性。
	RetrievalMode    string `json:"retrieval_mode,omitempty"`
	DegradedReason   string `json:"degraded_reason,omitempty"`
	SemanticCoverage string `json:"semantic_coverage,omitempty"`
}

// WorkspaceStatus 描述单个工作区的索引与运行状态。
//
// 字段与 JSON 形状迁移自 workspace.WorkspaceStatus；IndexRevision/Engine
// 为通用引擎字段，legacy ACE 路径不填充。
type WorkspaceStatus struct {
	DirectoryPath          string                `json:"directory_path"`
	PathKind               string                `json:"path_kind,omitempty"`
	HostOS                 string                `json:"host_os,omitempty"`
	ServedBy               *runtimeinfo.ServedBy `json:"served_by,omitempty"`
	ProviderProfileID      string                `json:"provider_profile_id,omitempty"`
	ProviderState          string                `json:"provider_state,omitempty"`
	CheckpointID           string                `json:"checkpoint_id,omitempty"`
	IndexRevision          string                `json:"index_revision,omitempty"`
	Engine                 string                `json:"engine,omitempty"`
	Semantic               *SemanticStatus       `json:"semantic,omitempty"`
	FileCount              int                   `json:"file_count"`
	InFlight               bool                  `json:"in_flight"`
	Stage                  IndexStage            `json:"stage"`
	LastSyncReason         SyncReason            `json:"last_sync_reason,omitempty"`
	LastErrorStage         IndexStage            `json:"last_error_stage,omitempty"`
	LastUploaded           int                   `json:"last_uploaded,omitempty"`
	LastAdded              int                   `json:"last_added,omitempty"`
	LastDeleted            int                   `json:"last_deleted,omitempty"`
	WatchEnabled           bool                  `json:"watch_enabled,omitempty"`
	WatchScheduled         bool                  `json:"watch_scheduled,omitempty"`
	WatchRunning           bool                  `json:"watch_running,omitempty"`
	LastError              string                `json:"last_error,omitempty"`
	WatchError             string                `json:"watch_error,omitempty"`
	UpstreamStatus         string                `json:"upstream_status,omitempty"`
	UpstreamLastStatusCode int                   `json:"upstream_last_status_code,omitempty"`
	UpstreamRetryAfter     string                `json:"upstream_retry_after,omitempty"`
	UpstreamBackoffUntil   *time.Time            `json:"upstream_backoff_until,omitempty"`
	UpstreamLastError      string                `json:"upstream_last_error,omitempty"`
	UpstreamLastFailure    *time.Time            `json:"upstream_last_failure,omitempty"`
	UpstreamLastSuccess    *time.Time            `json:"upstream_last_success,omitempty"`
	LastStartedAt          *time.Time            `json:"last_started_at,omitempty"`
	LastFinishedAt         *time.Time            `json:"last_finished_at,omitempty"`
	StageStartedAt         *time.Time            `json:"stage_started_at,omitempty"`
	LastWatchAt            *time.Time            `json:"last_watch_at,omitempty"`
	NextWatchAt            *time.Time            `json:"next_watch_at,omitempty"`
	LastBackgroundSyncAt   *time.Time            `json:"last_background_sync_at,omitempty"`
	UpdatedAt              *time.Time            `json:"updated_at,omitempty"`
	// TopLevelFileCounts 是现役索引按顶层目录的文件计数(灰度反馈三
	// C.1:文件选择遵循 ignore 链但排除面完全黑盒,使用方为定位
	// docs/ 缺失做了一整轮对照实验)。根文件归 "."。加性 omitempty。
	TopLevelFileCounts map[string]int `json:"top_level_file_counts,omitempty"`
}
