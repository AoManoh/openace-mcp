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
}

// Summary 输出一行人类可读的结果摘要，供 MCP 工具文本渲染使用。
func (r Result) Summary() string {
	if r.ProviderProfileID != "" {
		return fmt.Sprintf("provider_profile_id=%s checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.ProviderProfileID, r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
	}
	return fmt.Sprintf("checkpoint=%s files=%d uploaded=%d added=%d deleted=%d", r.CheckpointID, r.FileCount, r.Uploaded, r.Added, r.Deleted)
}

// MultiRetrievalStatus 汇总一次跨工作区检索的每仓结果。
type MultiRetrievalStatus struct {
	ProviderProfileID string                 `json:"provider_profile_id,omitempty"`
	TotalWorkspaces   int                    `json:"total_workspaces"`
	SuccessCount      int                    `json:"success_count"`
	FailureCount      int                    `json:"failure_count"`
	PartialFailure    bool                   `json:"partial_failure"`
	Workspaces        []MultiWorkspaceStatus `json:"workspaces"`
}

// MultiWorkspaceStatus 是跨工作区检索中单个工作区的结果条目。
type MultiWorkspaceStatus struct {
	DirectoryPath string `json:"directory_path"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
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
}
