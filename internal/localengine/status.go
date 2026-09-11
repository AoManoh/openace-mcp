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

// wsStatus 跟踪单个工作区的构建与索引状态，是 workspace_status 的数据源。
// 词法相关字段任何配置下都有；语义相关字段（覆盖、拒绝、嵌入进度）只在
// 配置了 embedding provider 时由 attachSemantic 挂到对外状态上，纯词法
// 配置的对外状态没有 semantic 块。
type wsStatus struct {
	mu sync.Mutex

	root         pathutil.WorkspaceRoot
	workspaceKey string

	inFlight      bool
	stage         engine.IndexStage
	revision      string
	fileCount     int
	chunkCount    int
	revisionCount int
	skippedFiles  int
	// permissionSkipped 是扫描期因无读权限被跳过的文件数。
	permissionSkipped int
	// oversizeSkipped 是扫描期因超过文本大小上限被跳过的文件数。
	oversizeSkipped int
	// gcFailedRevisions 是最近一次 revision GC 中删除失败的 revision 数，
	// 全部成功时归零。Windows 上被占用的映射或句柄会阻止删除；失败若不
	// 上报会积累成磁盘泄漏，下次启动的孤儿清理能补救，但长驻 daemon
	// 需要在状态里看到。
	gcFailedRevisions int
	capabilities      map[string]string
	lastError         string
	skippedRevisions  []string
	startedAt         *time.Time
	finishedAt        *time.Time
	updatedAt         time.Time

	// 语义路（向量检索）状态：coveredChunks 取 active manifest 的 VectorCount
	// （有向量的存活 chunk 数），rejectedChunks 与 embedError 来自最近一次
	// 构建的 provider 交互。
	coveredChunks  int
	rejectedChunks int
	embedError     string
	// 构建期 embedding 进度，按批更新，构建结束归零。
	embedPending int
	embedDone    int
	// bulkJob 是在途的 provider 批量嵌入作业标签，格式为
	// "voyage:<id> <state> <done>/<total>"；空串表示没有在途作业。
	bulkJob string
	// embedStartedAt 是本次构建嵌入阶段的起点，供速率与 ETA 估算，构建
	// 结束随进度一并归零。只有 pending 数没有速率时，等待方判断不出构建
	// 是否在推进、还要多久。
	embedStartedAt *time.Time
}

// setBulkJob 更新在途批量嵌入作业标签，空串表示清除。
func (s *wsStatus) setBulkJob(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bulkJob = label
}

// setGCFailures 记录最近一次 revision GC 的删除失败数，0 表示全部成功。
func (s *wsStatus) setGCFailures(failed int) {
	s.mu.Lock()
	s.gcFailedRevisions = failed
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// setEmbedProgress 按批更新构建期 embedding 进度。done 为 0 的那次调用是
// 嵌入阶段的起点，记下时刻供速率与 ETA 估算。
func (s *wsStatus) setEmbedProgress(pending int, done int) {
	now := time.Now().UTC()
	s.mu.Lock()
	s.embedPending = pending
	s.embedDone = done
	if done == 0 {
		s.embedStartedAt = &now
	}
	s.updatedAt = now
	s.mu.Unlock()
}

// embedRateETALocked 按当前进度估算嵌入速率（每分钟 chunk 数，最低记 1）
// 与剩余秒数；没有起点、尚未完成任何批次或耗时为 0 时返回 (0, 0)。
// 调用方必须持有 s.mu。
func (s *wsStatus) embedRateETALocked(now time.Time) (int, int) {
	if s.embedStartedAt == nil || s.embedDone <= 0 {
		return 0, 0
	}
	elapsed := now.Sub(*s.embedStartedAt).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	perSecond := float64(s.embedDone) / elapsed
	rate := int(perSecond * 60)
	if rate < 1 {
		rate = 1
	}
	eta := 0
	if s.embedPending > 0 && perSecond > 0 {
		eta = int(float64(s.embedPending)/perSecond + 0.5)
	}
	return rate, eta
}

// publishedInterim 登记首次构建的词法中间发布：revision 与计数立即可见，
// 构建保持 in_flight（嵌入随后进行），stage 不回退。状态同时呈现已有可
// 服务的索引与构建仍在进行两件事。
func (s *wsStatus) publishedInterim(manifest *index.Manifest, revisions int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision = manifest.Revision
	s.fileCount = manifest.Counts.Files
	s.chunkCount = manifest.Counts.Chunks
	s.coveredChunks = manifest.VectorCount
	s.revisionCount = revisions
	s.capabilities = manifest.ChunkerCapabilities
	s.updatedAt = time.Now().UTC()
}

// setSemanticOutcome 记录最近一次构建的语义路结果：按唯一嵌入键累计的拒绝数
// （provider 返回零向量或 NaN）与脱敏后的最后一条错误。provider 熔断器
// 状态不存这里，attachSemantic 从客户端实时读取。
func (s *wsStatus) setSemanticOutcome(rejected int, lastError string) {
	s.mu.Lock()
	s.rejectedChunks = rejected
	s.embedError = lastError
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// setScannedFiles 在扫描期间与扫描完成时回填文件数。改动前构建期的
// file_count 一直是 0，与不断增长的 embedded_chunks 并列显示，状态失真；
// 发布时 ready 用 manifest 里的实际数覆盖。
func (s *wsStatus) setScannedFiles(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileCount = count
	s.updatedAt = time.Now().UTC()
}

// setSkippedFiles 记录被内容检查拒绝（二进制、非 UTF-8、超过大小上限）
// 或切出 0 chunk 而跳过的文件数。
func (s *wsStatus) setSkippedFiles(count int) {
	s.mu.Lock()
	s.skippedFiles = count
	s.mu.Unlock()
}

// setPermissionSkipped 记录扫描期因无读权限被跳过的文件数。
func (s *wsStatus) setPermissionSkipped(count int) {
	s.mu.Lock()
	s.permissionSkipped = count
	s.mu.Unlock()
}

// setOversizeSkipped 记录扫描期因超过文本大小上限被跳过的文件数。
func (s *wsStatus) setOversizeSkipped(count int) {
	s.mu.Lock()
	s.oversizeSkipped = count
	s.mu.Unlock()
}

// statusFor 返回该工作区的状态跟踪器，没有则以 idle 阶段创建。
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
	s.embedStartedAt = nil
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
	s.embedStartedAt = nil
	s.lastError = sanitizeError(err)
	s.finishedAt = &now
	s.updatedAt = now
}

// noteSkippedRevisions 记录检索期打开句柄时跳过的损坏 revision，回退到旧
// revision 这件事在状态里可见。工作区还没有状态跟踪器时不记录。
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

// sanitizeError 把错误压成一条单行文本：连续空白合并为一个空格，超过
// 512 字节截断，截断点落在 UTF-8 字符边界上，状态 JSON 里不会出现被切
// 半的多字节字符。nil 返回空串。
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return reliability.SanitizeMessage(err.Error())
}

// snapshot 生成对外状态的词法部分。LastAdded 字段承载当前 revision 的
// chunk 总数；切分能力、revision 保留数、各类跳过计数与损坏回退信息压成
// 一行文本放在 UpstreamStatus 字段。
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
	if len(s.capabilities) > 0 || s.revisionCount > 0 || len(s.skippedRevisions) > 0 || s.skippedFiles > 0 || s.permissionSkipped > 0 || s.oversizeSkipped > 0 || s.gcFailedRevisions > 0 {
		status.UpstreamStatus = capabilitySummary(s.capabilities, s.revisionCount, s.skippedFiles, s.permissionSkipped, s.oversizeSkipped, s.gcFailedRevisions, s.skippedRevisions)
	}
	return status
}

// capabilitySummary 把每种语言的切分能力、revision 保留数、各类跳过文件
// 数、GC 失败数与被跳过的损坏 revision 压成一行可读文本，形如
// "chunker[go=ast revisions=2 skipped_files=1]"。这些信息复用旧引擎遗留
// 的 UpstreamStatus 字符串字段承载，没有为它们单独增加结构化字段。
func capabilitySummary(capabilities map[string]string, revisions int, skippedFiles int, permissionSkipped int, oversizeSkipped int, gcFailed int, skippedRevisions []string) string {
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
	if permissionSkipped > 0 {
		parts = append(parts, "permission_skipped="+strconv.Itoa(permissionSkipped))
	}
	if oversizeSkipped > 0 {
		parts = append(parts, "oversize_skipped="+strconv.Itoa(oversizeSkipped))
	}
	if gcFailed > 0 {
		parts = append(parts, "gc_failed="+strconv.Itoa(gcFailed))
	}
	if len(skippedRevisions) > 0 {
		parts = append(parts, "skipped="+strings.Join(skippedRevisions, ","))
	}
	return "chunker[" + strings.Join(parts, " ") + "]"
}

// attachSemantic 给对外状态挂上 embedding 与 rerank provider 的视图。
//   - 两个 provider 都完全没有配置（Options 零值）：不挂接，对外状态与
//     纯词法配置一致，没有 semantic 块。
//   - embedding 已启用：写入 provider、模型、维度；有跟踪器时写入覆盖、
//     拒绝、进度、速率与 ETA、在途批作业、journal（已付费但尚未进入
//     revision 的向量暂存文件）条数、覆盖率（没有任何 chunk 时记 100%，
//     否则空仓库会显示零覆盖并触发降级横幅）；再写入熔断器（provider
//     连续失败后暂停向它发请求）的状态、退避截止时刻与最后错误，以及
//     查询路径的熔断器状态与吞吐治理器的速率学习、目标 TPM、并发窗口、
//     在途数、暂停截止时刻。
//   - embedding 配置了但未启用（如缺 key）：写入 DisabledReason。
//   - rerank 同理：启用时写入身份与熔断器状态，配置了未启用时写入原因。
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
			semantic.EmbedRatePerMin, semantic.EmbedETASeconds = tracker.embedRateETALocked(time.Now().UTC())
			semantic.BulkJob = tracker.bulkJob
			tracker.mu.Unlock()
			// journal 条数从已打开的 journal 实时读取；journal 尚未打开时不
			// 显示。
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
		// 查询路径的熔断器状态与吞吐治理器（按 provider 反馈调节请求速率
		// 与并发窗口的组件）的窗口、速率、暂停截止时刻都进状态：否则构建
		// 慢的时候从状态里判断不出是限速、退避还是别的原因，曾有人因此
		// 误重启 daemon。
		semantic.QueryProviderState = e.embedClient.QueryCircuitSnapshot().State
		governor := e.embedClient.GovernorSnapshot()
		semantic.GovernorRateLearning = governor.RateLearning
		semantic.GovernorTargetTPM = governor.TargetTokensPerMin
		semantic.GovernorWindow = governor.Window
		semantic.GovernorMaxWindow = governor.MaxWindow
		semantic.GovernorInFlight = governor.InFlight
		if !governor.PausedUntil.IsZero() {
			paused := governor.PausedUntil
			semantic.GovernorPausedUntil = &paused
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

// WorkspaceStatus 实现 engine.WorkspaceInspector，返回单个工作区的状态。
// 内存里有跟踪器时直接取快照；没有（daemon 刚启动）时从磁盘上的可用
// manifest 恢复视图并建立跟踪器；磁盘上也没有 revision 时返回 idle 状态，
// 只带 provider 配置视图。
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
		e.attachTopLevelCounts(&snapshot, workspaceKey)
		return snapshot, nil
	}
	// 内存里没有跟踪器（daemon 刚启动）：从磁盘上的 manifest 恢复视图。
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
	e.attachTopLevelCounts(&snapshot, workspaceKey)
	return snapshot, nil
}

// attachTopLevelCounts 按 active manifest 统计每个顶层目录下被索引的文件
// 数，根目录下的文件归 "."。计数帮助核对实际索引范围；预期目录缺失时，
// 还需区分忽略规则与尚未同步等原因，不能仅据该计数裁定原因。打不开
// 句柄时跳过，状态查询不因这项附加信息失败。
func (e *Engine) attachTopLevelCounts(status *engine.WorkspaceStatus, workspaceKey string) {
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		return
	}
	defer e.releaseHandle(handle)
	counts := make(map[string]int, 16)
	for path := range handle.manifest.Files {
		top := "."
		if i := strings.IndexByte(path, '/'); i > 0 {
			top = path[:i]
		}
		counts[top]++
	}
	if len(counts) > 0 {
		status.TopLevelFileCounts = counts
	}
}

// ListWorkspaceStatuses 实现 engine.WorkspaceInspector：返回本进程内有状态
// 跟踪器的全部工作区，按路径排序。只列内存里的跟踪器，不扫磁盘；不附
// 顶层目录计数，该项只在 WorkspaceStatus 单工作区查询时附带。
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
