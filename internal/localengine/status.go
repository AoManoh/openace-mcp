package localengine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// wsStatus 跟踪单个工作区的构建与索引状态。
// 词法检索是本模式的完整能力：状态不出现 semantic/degraded 字段（阶段计划 D2）。
type wsStatus struct {
	mu sync.Mutex

	root         pathutil.WorkspaceRoot
	workspaceKey string

	inFlight         bool
	stage            engine.IndexStage
	revision         string
	fileCount        int
	chunkCount       int
	revisionCount    int
	skippedFiles     int
	capabilities     map[string]string
	lastError        string
	skippedRevisions []string
	startedAt        *time.Time
	finishedAt       *time.Time
	updatedAt        time.Time

	// 语义路状态（Stage 3）：covered 来自 active manifest（暗坑 K31），
	// rejected/embedError 来自最近一次构建的 provider 交互。
	coveredChunks  int
	rejectedChunks int
	embedError     string
	// 构建期 embedding 进度（Stage 4 D8）：按批更新，构建结束归零。
	embedPending int
	embedDone    int
}

// setEmbedProgress 按批更新构建期 embedding 进度（D8/G2 可见性）。
func (s *wsStatus) setEmbedProgress(pending int, done int) {
	s.mu.Lock()
	s.embedPending = pending
	s.embedDone = done
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// setSemanticOutcome 记录最近一次构建的语义路交互结果（K35 拒绝数与
// 脱敏错误；provider circuit 状态由 Engine 层实时提供）。
func (s *wsStatus) setSemanticOutcome(rejected int, lastError string) {
	s.mu.Lock()
	s.rejectedChunks = rejected
	s.embedError = lastError
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// setSkippedFiles 记录内容门禁跳过的文件数（暗坑 K6）。
func (s *wsStatus) setSkippedFiles(count int) {
	s.mu.Lock()
	s.skippedFiles = count
	s.mu.Unlock()
}

// statusFor 返回（必要时创建）工作区状态跟踪器。
func (e *Engine) statusFor(root pathutil.WorkspaceRoot, workspaceKey string) *wsStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if status, ok := e.statuses[workspaceKey]; ok {
		return status
	}
	status := &wsStatus{root: root, workspaceKey: workspaceKey, stage: engine.IndexStageIdle}
	e.statuses[workspaceKey] = status
	return status
}

func (s *wsStatus) begin() {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = true
	s.lastError = ""
	s.startedAt = &now
	s.finishedAt = nil
	s.updatedAt = now
}

func (s *wsStatus) setStage(stage engine.IndexStage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stage = stage
	s.updatedAt = time.Now().UTC()
}

func (s *wsStatus) ready(manifest *index.Manifest, revisions int) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = false
	s.stage = engine.IndexStageReady
	s.embedPending = 0
	s.embedDone = 0
	s.revision = manifest.Revision
	s.fileCount = manifest.Counts.Files
	s.chunkCount = manifest.Counts.Chunks
	s.coveredChunks = manifest.VectorCount
	s.revisionCount = revisions
	s.capabilities = manifest.ChunkerCapabilities
	s.lastError = ""
	s.finishedAt = &now
	s.updatedAt = now
}

func (s *wsStatus) fail(err error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = false
	s.stage = engine.IndexStageFailed
	s.embedPending = 0
	s.embedDone = 0
	s.lastError = sanitizeError(err)
	s.finishedAt = &now
	s.updatedAt = now
}

// noteSkippedRevisions 记录检索期发现的损坏 revision（回退可见性）。
func (e *Engine) noteSkippedRevisions(workspaceKey string, skipped []string) {
	e.mu.Lock()
	status, ok := e.statuses[workspaceKey]
	e.mu.Unlock()
	if !ok {
		return
	}
	status.mu.Lock()
	status.skippedRevisions = skipped
	status.updatedAt = time.Now().UTC()
	status.mu.Unlock()
}

// sanitizeError 收敛错误文本：去除多行与首尾空白，长度封顶(复用
// reliability 口径,L10 修复后截断安全落 rune 边界)。
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return reliability.SanitizeMessage(err.Error())
}

// snapshot 生成对外状态。
func (s *wsStatus) snapshot() engine.WorkspaceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := engine.WorkspaceStatus{
		DirectoryPath: s.root.CanonicalPath,
		PathKind:      string(s.root.PathKind),
		HostOS:        s.root.HostOS,
		Engine:        EngineID,
		IndexRevision: s.revision,
		FileCount:     s.fileCount,
		InFlight:      s.inFlight,
		Stage:         s.stage,
		LastAdded:     s.chunkCount,
		LastError:     s.lastError,
	}
	if s.startedAt != nil {
		started := *s.startedAt
		status.LastStartedAt = &started
	}
	if s.finishedAt != nil {
		finished := *s.finishedAt
		status.LastFinishedAt = &finished
	}
	if !s.updatedAt.IsZero() {
		updated := s.updatedAt
		status.UpdatedAt = &updated
	}
	if len(s.capabilities) > 0 || s.revisionCount > 0 || len(s.skippedRevisions) > 0 || s.skippedFiles > 0 {
		status.UpstreamStatus = capabilitySummary(s.capabilities, s.revisionCount, s.skippedFiles, s.skippedRevisions)
	}
	return status
}

// capabilitySummary 把 chunker 能力、revision 保留数、内容门禁跳过数
// 与损坏回退信息压缩为一行可读文本（Stage 2 复用现有 UpstreamStatus
// 字段承载本地引擎详情，避免提前扩表；Stage 3 状态扩展时再字段化）。
func capabilitySummary(capabilities map[string]string, revisions int, skippedFiles int, skippedRevisions []string) string {
	parts := make([]string, 0, len(capabilities)+3)
	languages := make([]string, 0, len(capabilities))
	for language := range capabilities {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	for _, language := range languages {
		parts = append(parts, language+"="+capabilities[language])
	}
	if revisions > 0 {
		parts = append(parts, "revisions="+strconv.Itoa(revisions))
	}
	if skippedFiles > 0 {
		parts = append(parts, "skipped_files="+strconv.Itoa(skippedFiles))
	}
	if len(skippedRevisions) > 0 {
		parts = append(parts, "skipped="+strings.Join(skippedRevisions, ","))
	}
	return "chunker[" + strings.Join(parts, " ") + "]"
}

// attachSemantic 为状态挂接语义路/精排 provider 视图（阶段计划 T08）。
// 两个 provider 均为零配置（Options 零值）时不挂接，保持 Stage 2 wire
// 不变（K32/K34）；配置了但未启用（如缺 key）时如实给出原因（D1）。
func (e *Engine) attachSemantic(status *engine.WorkspaceStatus, tracker *wsStatus) {
	embedConfigured := e.embedCfg.Enabled || e.embedCfg.DisabledReason != ""
	rerankConfigured := e.rerankCfg.Enabled || e.rerankCfg.DisabledReason != ""
	if !embedConfigured && !rerankConfigured {
		return
	}
	semantic := &engine.SemanticStatus{Enabled: e.embedCfg.Enabled}
	if e.embedCfg.Enabled {
		semantic.Provider = e.embedCfg.ProviderType
		semantic.Model = e.embedCfg.Model
		semantic.Dimension = e.embedCfg.Dimension
		if tracker != nil {
			tracker.mu.Lock()
			semantic.CoveredChunks = tracker.coveredChunks
			semantic.TotalChunks = tracker.chunkCount
			semantic.RejectedChunks = tracker.rejectedChunks
			semantic.LastError = tracker.embedError
			semantic.PendingChunks = tracker.embedPending
			semantic.EmbeddedChunks = tracker.embedDone
			tracker.mu.Unlock()
			// journal 条数（D4 可见性）：已打开的暂存区实时读取。
			e.mu.Lock()
			journal := e.journals[tracker.workspaceKey]
			e.mu.Unlock()
			if journal != nil {
				semantic.JournalEntries = len(journal.Snapshot())
			}
			if semantic.TotalChunks == 0 {
				semantic.Coverage = "100%"
			} else {
				semantic.Coverage = fmt.Sprintf("%d%%", semantic.CoveredChunks*100/semantic.TotalChunks)
			}
		}
		circuit := e.embedClient.CircuitSnapshot()
		semantic.ProviderState = circuit.State
		if !circuit.BackoffUntil.IsZero() {
			until := circuit.BackoffUntil
			semantic.BackoffUntil = &until
		}
		if semantic.LastError == "" && circuit.LastError != "" {
			semantic.LastError = circuit.LastError
		}
	} else if embedConfigured {
		semantic.DisabledReason = e.embedCfg.DisabledReason
	}
	if e.rerankCfg.Enabled {
		semantic.RerankProvider = e.rerankCfg.Identity()
		semantic.RerankState = e.rerankClient.CircuitSnapshot().State
	} else if rerankConfigured {
		semantic.RerankDisabledReason = e.rerankCfg.DisabledReason
	}
	status.Semantic = semantic
}

// WorkspaceStatus 实现 engine.WorkspaceInspector。
func (e *Engine) WorkspaceStatus(ctx context.Context, ref engine.WorkspaceRef) (engine.WorkspaceStatus, error) {
	if err := rejectProfileID(ref); err != nil {
		return engine.WorkspaceStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return engine.WorkspaceStatus{}, err
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return engine.WorkspaceStatus{}, err
	}
	e.mu.Lock()
	status, ok := e.statuses[workspaceKey]
	e.mu.Unlock()
	if ok {
		snapshot := status.snapshot()
		e.attachSemantic(&snapshot, status)
		return snapshot, nil
	}
	// 冷启动：内存无状态时从持久化 manifest 恢复视图。
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return engine.WorkspaceStatus{}, err
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil {
		if isNoRevision(err) {
			cold := engine.WorkspaceStatus{
				DirectoryPath: root.CanonicalPath,
				PathKind:      string(root.PathKind),
				HostOS:        root.HostOS,
				Engine:        EngineID,
				Stage:         engine.IndexStageIdle,
			}
			e.attachSemantic(&cold, nil)
			return cold, nil
		}
		return engine.WorkspaceStatus{}, err
	}
	tracker := e.statusFor(root, workspaceKey)
	tracker.ready(manifest, revisionCount(store, manifest))
	snapshot := tracker.snapshot()
	e.attachSemantic(&snapshot, tracker)
	return snapshot, nil
}

// ListWorkspaceStatuses 实现 engine.WorkspaceInspector。
func (e *Engine) ListWorkspaceStatuses(ctx context.Context) ([]engine.WorkspaceStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	trackers := make([]*wsStatus, 0, len(e.statuses))
	for _, status := range e.statuses {
		trackers = append(trackers, status)
	}
	e.mu.Unlock()
	statuses := make([]engine.WorkspaceStatus, 0, len(trackers))
	for _, tracker := range trackers {
		snapshot := tracker.snapshot()
		e.attachSemantic(&snapshot, tracker)
		statuses = append(statuses, snapshot)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].DirectoryPath < statuses[j].DirectoryPath })
	return statuses, nil
}
