package localengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/reliability"
	"github.com/AoManoh/openace-mcp/internal/rerank"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// noHitsText 与既有 MCP 文案保持一致。
const noHitsText = "No relevant code sections were found."

// chunkMeta 是存活 chunk 的常驻元数据（Stage 4 D5：内容不常驻——
// 渲染/精排按需从 segment 的 chunks.jsonl pread，查询路 RSS 与仓库
// 文本体积解耦）。offset/length 定位该记录在段文件中的原始 JSON 行。
type chunkMeta struct {
	RelPath     string
	Language    string
	StartLine   int
	EndLine     int
	Symbol      string
	ContentHash string
	segIdx      int
	offset      int64
	length      int
}

// revisionHandle 是一个已打开 revision 的只读句柄（refcount 管理，暗坑 K3/K11）。
// 多 segment 经 lexical alias 聚合（D2）；chunks 只含存活（newest-wins +
// tombstone 过滤后）记录的元数据，作为两路召回的统一过滤基准（暗坑
// K39/K44）。向量索引按需懒加载并随句柄常驻（revision 不可变，加载一次
// 即定格）。
type revisionHandle struct {
	workspaceKey string
	manifest     *index.Manifest
	lex          *lexical.Index
	chunks       map[string]chunkMeta
	segmentDirs  []string
	refs         int
	retired      bool

	// contentMu 守护 contentFiles 的懒打开（segment 不可变，句柄生命
	// 周期内文件描述符 ≤ 段数，受 compaction 阈值约束，K49）。
	contentMu    sync.Mutex
	contentFiles []*os.File

	vecOnce sync.Once
	vecIxs  []*vector.Index
	vecErr  error
}

// record 按需取回 chunk 全量记录（pread + 行级校验，暗坑 K47）。
func (h *revisionHandle) record(id string) (chunkRecord, error) {
	meta, ok := h.chunks[id]
	if !ok {
		return chunkRecord{}, fmt.Errorf("chunk %s 不在存活集", id)
	}
	h.contentMu.Lock()
	if h.contentFiles == nil {
		h.contentFiles = make([]*os.File, len(h.segmentDirs))
	}
	file := h.contentFiles[meta.segIdx]
	if file == nil {
		opened, err := os.Open(filepath.Join(h.segmentDirs[meta.segIdx], index.ChunksFileName))
		if err != nil {
			h.contentMu.Unlock()
			return chunkRecord{}, fmt.Errorf("打开 chunk 数据: %w", err)
		}
		h.contentFiles[meta.segIdx] = opened
		file = opened
	}
	h.contentMu.Unlock()

	buf := make([]byte, meta.length)
	if _, err := file.ReadAt(buf, meta.offset); err != nil {
		return chunkRecord{}, fmt.Errorf("读取 chunk 内容: %w", err)
	}
	var record chunkRecord
	if err := json.Unmarshal(buf, &record); err != nil {
		return chunkRecord{}, fmt.Errorf("chunk 内容损坏（K47）: %w", err)
	}
	if record.ID != id {
		return chunkRecord{}, fmt.Errorf("chunk 偏移错位（K47）: 期望 %s 实际 %s", id, record.ID)
	}
	return record, nil
}

// closeContentFiles 释放按需读取的文件句柄（句柄关闭路径调用）。
func (h *revisionHandle) closeContentFiles() {
	h.contentMu.Lock()
	defer h.contentMu.Unlock()
	for i, file := range h.contentFiles {
		if file != nil {
			_ = file.Close()
			h.contentFiles[i] = nil
		}
	}
}

// vectorIndexes 懒加载本 revision 全部 segment 的向量索引；任一校验失败
// 只降级语义路，不影响词法可用性（暗坑 K25）。总量超出 envelope 显式
// 拒绝（§18，暗坑 K49 同族）。
func (h *revisionHandle) vectorIndexes(dimension int) ([]*vector.Index, error) {
	h.vecOnce.Do(func() {
		if !h.manifest.HasVectors() {
			h.vecErr = errors.New("revision 无向量数据")
			return
		}
		total := 0
		indexes := make([]*vector.Index, 0, len(h.manifest.Segments))
		for i, segment := range h.manifest.Segments {
			if segment.VectorsChecksum == "" {
				continue
			}
			ix, err := vector.Load(h.segmentDirs[i], dimension,
				segment.VectorsChecksum, segment.VectorsIndexChecksum, 0)
			if err != nil {
				h.vecErr = err
				return
			}
			total += ix.Count()
			indexes = append(indexes, ix)
		}
		if total > vector.DefaultMaxResidentVectors {
			h.vecErr = fmt.Errorf("%w: %d > %d", vector.ErrEnvelopeExceeded, total, vector.DefaultMaxResidentVectors)
			return
		}
		h.vecIxs = indexes
	})
	return h.vecIxs, h.vecErr
}

// loadLiveChunkMetas 扫描 revision 全部 segment 的 chunks.jsonl，产出
// 存活 chunk 的元数据与文件内偏移（D5：内容本体不驻留；newest-wins +
// tombstone 以 manifest.Files 为最终裁决，暗坑 K39/K44）。
func loadLiveChunkMetas(manifest *index.Manifest, segmentDirs []string) (map[string]chunkMeta, error) {
	type located struct {
		id   string
		meta chunkMeta
	}
	byFile := make(map[string][]located, len(manifest.Files))
	for segIdx, dir := range segmentDirs {
		file, err := os.Open(filepath.Join(dir, index.ChunksFileName))
		if err != nil {
			return nil, fmt.Errorf("读取 segment chunk 数据: %w", err)
		}
		reader := bufio.NewReaderSize(file, 1<<20)
		var offset int64
		segByFile := make(map[string][]located)
		for {
			line, err := reader.ReadBytes('\n')
			lineLen := len(line)
			trimmed := bytes.TrimRight(line, "\n")
			if len(trimmed) > 0 {
				var record chunkRecord
				if unmarshalErr := json.Unmarshal(trimmed, &record); unmarshalErr != nil {
					file.Close()
					return nil, fmt.Errorf("chunk 数据损坏: %w", unmarshalErr)
				}
				segByFile[record.RelPath] = append(segByFile[record.RelPath], located{
					id: record.ID,
					meta: chunkMeta{
						RelPath: record.RelPath, Language: record.Language,
						StartLine: record.StartLine, EndLine: record.EndLine,
						Symbol: record.Symbol, ContentHash: record.ContentHash,
						segIdx: segIdx, offset: offset, length: len(trimmed),
					},
				})
			}
			offset += int64(lineLen)
			if err != nil {
				break
			}
		}
		file.Close()
		// 段序后者覆盖前者（newest-wins）。
		for path, group := range segByFile {
			byFile[path] = group
		}
	}
	metas := make(map[string]chunkMeta)
	for path, group := range byFile {
		if _, live := manifest.Files[path]; !live {
			continue
		}
		for _, item := range group {
			metas[item.id] = item.meta
		}
	}
	return metas, nil
}

// rankedHit 是进入渲染的最终排序候选；score 仅用于同文件合并后的
// 跨块排序（"越大越靠前"），Stage 2 纯词法路径沿用真实 BM25 分数。
type rankedHit struct {
	id    string
	score float64
}

// Search 实现 engine.Service：按需同步后执行 lexical（+dense RRF）
// （+optional rerank）检索；降级行为受 D8 支配，禁止静默。
func (e *Engine) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	if err := rejectProfileID(req.Workspace); err != nil {
		return engine.Result{}, err
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return engine.Result{}, errors.New("查询内容为空")
	}
	// 检索前确保索引就绪（与 legacy Syncer 的 retrieval 语义一致）。
	syncResult, syncErr := e.syncWorkspace(ctx, req.Workspace)
	if syncErr != nil && ctx.Err() != nil {
		return engine.Result{}, ctx.Err()
	}
	_, workspaceKey, err := e.resolveRoot(req.Workspace.DirectoryPath)
	if err != nil {
		return engine.Result{}, err
	}
	handle, handleErr := e.acquireHandle(workspaceKey)
	var reasons []string
	if syncErr != nil {
		// D8(d)/review S23：索引刷新失败但存在可用 revision 时，
		// allow 以旧索引服务并显式标记 stale，deny 报错。
		if handleErr != nil {
			return engine.Result{}, syncErr
		}
		if e.retrievalDegrade == DegradeDeny {
			e.releaseHandle(handle)
			return engine.Result{}, degradeDeniedError("index refresh failed", syncErr, EnvRetrievalDegrade)
		}
		reasons = append(reasons, "stale-index")
		syncResult = engine.Result{Engine: EngineID, FileCount: handle.manifest.Counts.Files}
	} else if handleErr != nil {
		return engine.Result{}, handleErr
	}
	defer e.releaseHandle(handle)

	mode := "lexical"
	lexTopK := defaultTopK
	if e.semanticEnabled() {
		lexTopK = hybridRouteTopK
	}
	lexHits, err := handle.lex.Search(ctx, query, lexTopK)
	if err != nil {
		return engine.Result{}, fmt.Errorf("词法检索: %w", err)
	}
	// 统一过滤 choke point（暗坑 K39/K44）：两路召回都可能命中旧
	// segment 中被 supersede/tombstone 的死 chunk，进入融合前按存活
	// 集过滤，杜绝死内容占据候选位。
	lexHits = filterLiveHits(handle, lexHits)

	var ordered []rankedHit
	coverage := ""
	if e.semanticEnabled() {
		denseIDs, denseReasons, denseErr := e.denseRoute(ctx, workspaceKey, handle, query)
		if denseErr != nil {
			return engine.Result{}, denseErr
		}
		reasons = append(reasons, denseReasons...)
		lexIDs := make([]string, 0, len(lexHits))
		for _, hit := range lexHits {
			lexIDs = append(lexIDs, hit.ID)
		}
		if denseIDs != nil {
			mode = "hybrid"
			fused := fusion.RRF(lexIDs, denseIDs)
			ids := make([]string, 0, len(fused))
			for _, f := range fused {
				ids = append(ids, f.ID)
			}
			ordered = rankByPosition(ids)
		} else {
			ordered = rankByPosition(lexIDs)
		}
		coverage = coveragePercent(handle.manifest)
		if !handle.manifest.SemanticComplete() {
			reasons = append(reasons, "semantic-coverage-partial")
		}
	} else {
		// 纯词法：沿用真实 BM25 分数，渲染行为与 Stage 2 逐字节一致（K32）。
		ordered = make([]rankedHit, 0, len(lexHits))
		for _, hit := range lexHits {
			ordered = append(ordered, rankedHit{id: hit.ID, score: hit.Score})
		}
	}

	// 可选精排：只重排已召回候选头部，失败绝不丢候选（D7/D8(b)）。
	if e.rerankClient != nil && len(ordered) > 0 {
		reordered, applied, rerankReason, rerankErr := e.rerankOrder(ctx, handle, query, ordered)
		if rerankErr != nil {
			return engine.Result{}, rerankErr
		}
		if applied {
			mode += "+rerank"
			ordered = reordered
		}
		if rerankReason != "" {
			reasons = append(reasons, rerankReason)
		}
	}

	text, renderErr := renderHits(handle, ordered, req.MaxOutputLen)
	if renderErr != nil {
		return engine.Result{}, renderErr
	}
	degradedReason := strings.Join(reasons, ",")
	if degradedReason != "" {
		text = degradedBanner(degradedReason, mode, coverage) + text
	}
	result := syncResult
	result.Text = text
	result.Engine = EngineID
	result.IndexRevision = handle.manifest.Revision
	// 透明性字段：provider 已配置或发生降级时填充；纯词法正常路径
	// 保持空（Stage 2 wire 不变，K32/K34）。
	if e.semanticEnabled() || e.rerankClient != nil || degradedReason != "" {
		result.RetrievalMode = mode
		result.DegradedReason = degradedReason
		result.SemanticCoverage = coverage
	}
	return result, nil
}

// filterLiveHits 过滤词法命中中的死 chunk（统一 choke point 的词法半边）。
func filterLiveHits(handle *revisionHandle, hits []lexical.Hit) []lexical.Hit {
	live := hits[:0]
	for _, hit := range hits {
		if _, ok := handle.chunks[hit.ID]; ok {
			live = append(live, hit)
		}
	}
	return live
}

// denseRoute 执行语义召回：返回 dense 候选 ID（nil 表示语义路未执行）、
// 降级原因与致命错误（ctx 取消或 deny 拒绝）。多 segment 逐段 exact
// 检索后确定性归并（score desc, ID asc，暗坑 K27），死 chunk 在归并后
// 过滤（暗坑 K39）。
func (e *Engine) denseRoute(ctx context.Context, workspaceKey string, handle *revisionHandle, query string) ([]string, []string, error) {
	ixs, loadErr := handle.vectorIndexes(e.embedCfg.Dimension)
	if loadErr != nil {
		if errors.Is(loadErr, vector.ErrEnvelopeExceeded) {
			// 超出已验证 envelope：重建无济于事，不标记 repair（§18）。
			if e.retrievalDegrade == DegradeDeny {
				return nil, nil, degradeDeniedError("semantic path unavailable", loadErr, EnvRetrievalDegrade)
			}
			return nil, []string{"vector-envelope-exceeded"}, nil
		}
		// 向量数据损坏/缺失：登记自愈并降级（暗坑 K25）。
		e.markVectorRepair(workspaceKey)
		if e.retrievalDegrade == DegradeDeny {
			return nil, nil, degradeDeniedError("semantic path unavailable", loadErr, EnvRetrievalDegrade)
		}
		return nil, []string{"vector-data-unavailable"}, nil
	}
	total := 0
	for _, ix := range ixs {
		total += ix.Count()
	}
	if total == 0 {
		// 覆盖为空（如 provider 长期故障后的零覆盖 revision）：
		// 语义路无候选可召回，覆盖缺口由调用方按 manifest 如实上报。
		return nil, nil, nil
	}
	queryVector, embedErr := e.embedClient.EmbedQuery(ctx, query)
	if embedErr != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if e.retrievalDegrade == DegradeDeny {
			return nil, nil, degradeDeniedError("query embedding failed", embedErr, EnvRetrievalDegrade)
		}
		return nil, []string{"query-embedding-failed(" + failureClass(embedErr) + ")"}, nil
	}
	var merged []vector.Hit
	for _, ix := range ixs {
		queryCopy := make([]float32, len(queryVector))
		copy(queryCopy, queryVector)
		segmentHits, searchErr := ix.Search(ctx, queryCopy, hybridRouteTopK)
		if searchErr != nil {
			return nil, nil, searchErr
		}
		merged = append(merged, segmentHits...)
	}
	sort.SliceStable(merged, func(a, b int) bool {
		if merged[a].Score != merged[b].Score {
			return merged[a].Score > merged[b].Score
		}
		return merged[a].ID < merged[b].ID
	})
	ids := make([]string, 0, hybridRouteTopK)
	for _, hit := range merged {
		if _, live := handle.chunks[hit.ID]; !live {
			continue
		}
		ids = append(ids, hit.ID)
		if len(ids) >= hybridRouteTopK {
			break
		}
	}
	return ids, nil, nil
}

// rerankOrder 精排 ordered 头部（≤rerankHeadLimit，再受 token 预算截断），
// 未送审部分按原序跟随（暗坑 K28）。送审集与 ordered 显式对齐：chunks
// 表缺失的头部候选（防御性路径）保持原位跟随，禁止因 docs/ordered
// 错位造成条目重复或丢失（Stage 3 自审修复）。
func (e *Engine) rerankOrder(ctx context.Context, handle *revisionHandle, query string, ordered []rankedHit) ([]rankedHit, bool, string, error) {
	head := rerankHeadLimit
	if head > len(ordered) {
		head = len(ordered)
	}
	docs := make([]rerank.Document, 0, head)
	included := make([]rankedHit, 0, head)
	skippedHead := make([]rankedHit, 0)
	for _, hit := range ordered[:head] {
		if _, ok := handle.chunks[hit.id]; !ok {
			skippedHead = append(skippedHead, hit)
			continue
		}
		record, err := handle.record(hit.id)
		if err != nil {
			return nil, false, "", err
		}
		docs = append(docs, rerank.Document{ID: hit.id, Text: rerankDocText(record)})
		included = append(included, hit)
	}
	hits, sent, err := e.rerankClient.Rerank(ctx, query, docs)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, "", ctx.Err()
		}
		if e.rerankDegrade == DegradeDeny {
			return nil, false, "", degradeDeniedError("rerank failed", err, EnvRerankDegrade)
		}
		return nil, false, "rerank-skipped(" + failureClass(err) + ")", nil
	}
	if sent == 0 {
		return nil, false, "rerank-skipped(token-budget)", nil
	}
	ids := make([]string, 0, len(ordered))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	// 顺序：重排头部 → 因 token 预算未送审的头部（原序）→ chunks 缺失的
	// 头部（原序）→ 头部之外的尾部（原序）。
	for _, hit := range included[sent:] {
		ids = append(ids, hit.id)
	}
	for _, hit := range skippedHead {
		ids = append(ids, hit.id)
	}
	for _, hit := range ordered[head:] {
		ids = append(ids, hit.id)
	}
	return rankByPosition(ids), true, "", nil
}

// rerankDocText 构造送审文本：path:start-end symbol 头 + 内容（D6，
// rerank 无缓存语义，头信息帮助精排理解上下文）。
func rerankDocText(record chunkRecord) string {
	header := fmt.Sprintf("%s:%d-%d", record.RelPath, record.StartLine, record.EndLine)
	if record.Symbol != "" {
		header += " " + record.Symbol
	}
	return header + "\n" + record.Content
}

// rankByPosition 把最终 ID 序转换为合成序分（1/(pos+1)），保证渲染合并
// 后的跨块排序与最终排名一致（暗坑 K27 确定性）。
func rankByPosition(ids []string) []rankedHit {
	ordered := make([]rankedHit, 0, len(ids))
	for i, id := range ids {
		ordered = append(ordered, rankedHit{id: id, score: 1.0 / float64(i+1)})
	}
	return ordered
}

// coveragePercent 计算语义覆盖率（向下取整；空仓库按 100%，暗坑 K31）。
func coveragePercent(manifest *index.Manifest) string {
	if manifest.Counts.Chunks == 0 {
		return "100%"
	}
	return fmt.Sprintf("%d%%", manifest.VectorCount*100/manifest.Counts.Chunks)
}

// degradedBanner 构造 [DEGRADED] 首行横幅（D8 定稿格式）。
func degradedBanner(reason string, mode string, coverage string) string {
	banner := "[DEGRADED] " + reason + "; mode=" + mode
	if coverage != "" {
		banner += "; semantic_coverage=" + coverage
	}
	return banner + "\n\n"
}

// degradeDeniedError 构造 deny 模式的可行动错误（暗坑 K33）：
// 保留分类诊断并附恢复路径提示。
func degradeDeniedError(stage string, cause error, envName string) error {
	return fmt.Errorf("%s: %v (degrade mode is deny; set %s=allow to accept degraded results)", stage, cause, envName)
}

// failureClass 提取失败类别 token（进入 degraded_reason）。
func failureClass(err error) string {
	callErr := &reliability.CallError{}
	if errors.As(err, &callErr) {
		return string(callErr.Class)
	}
	return "error"
}

// handleKey 是句柄表键：workspaceKey + revision 复合，避免跨工作区
// revision 碰撞与同名键位覆盖（review B3）。
func handleKey(workspaceKey string, revision string) string {
	return workspaceKey + "\x00" + revision
}

// acquireHandle 打开（或复用）首个真正可打开的 revision 句柄并增加引用。
// manifest 校验通过但 Bleve 打开失败的 revision 视为损坏，沿 previous 链
// 继续回退（review B2），全部失败时返回最后错误。
func (e *Engine) acquireHandle(workspaceKey string) (*revisionHandle, error) {
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return nil, err
	}
	manifest, skipped, err := store.ResolveUsable()
	if err != nil {
		return nil, err
	}
	var lastErr error
	visited := make(map[string]bool)
	for manifest != nil && !visited[manifest.Revision] && len(visited) < index.MaxRevisionChain {
		visited[manifest.Revision] = true
		handle, openErr := e.openOrReuseHandle(store, workspaceKey, manifest)
		if openErr == nil {
			if len(skipped) > 0 {
				e.noteSkippedRevisions(workspaceKey, skipped)
			}
			return handle, nil
		}
		lastErr = openErr
		skipped = append(skipped, manifest.Revision)
		previousRevision := manifest.PreviousRevision
		manifest = nil
		if previousRevision != "" {
			if previous, loadErr := store.LoadManifest(previousRevision); loadErr == nil {
				if verifyErr := store.VerifyManifest(previous); verifyErr == nil {
					manifest = previous
				}
			}
		}
	}
	e.noteSkippedRevisions(workspaceKey, skipped)
	if lastErr == nil {
		lastErr = index.ErrNoUsableRevision
	}
	return nil, lastErr
}

// openOrReuseHandle 复用缓存句柄（含已退役者：revision 不可变，数据仍有效，
// 重新启用即可），否则打开新句柄并注册。
func (e *Engine) openOrReuseHandle(store *index.Store, workspaceKey string, manifest *index.Manifest) (*revisionHandle, error) {
	key := handleKey(workspaceKey, manifest.Revision)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("local-hybrid 引擎已关闭")
	}
	if handle, ok := e.handles[key]; ok {
		handle.retired = false
		handle.refs++
		e.mu.Unlock()
		return handle, nil
	}
	e.mu.Unlock()

	segmentDirs := make([]string, 0, len(manifest.Segments))
	lexicalDirs := make([]string, 0, len(manifest.Segments))
	for _, segment := range manifest.Segments {
		dir := store.SegmentPathFor(segment.ID)
		segmentDirs = append(segmentDirs, dir)
		lexicalDirs = append(lexicalDirs, filepath.Join(dir, index.LexicalDirName))
	}
	lex, err := lexical.OpenMulti(lexicalDirs)
	if err != nil {
		return nil, fmt.Errorf("打开词法索引（revision %s）: %w", manifest.Revision, err)
	}
	chunks, err := loadLiveChunkMetas(manifest, segmentDirs)
	if err != nil {
		lex.Close()
		return nil, err
	}
	handle := &revisionHandle{
		workspaceKey: workspaceKey, manifest: manifest, lex: lex, chunks: chunks,
		segmentDirs: segmentDirs, refs: 1,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		go lex.Close()
		return nil, errors.New("local-hybrid 引擎已关闭")
	}
	if existing, ok := e.handles[key]; ok {
		// 并发打开竞争：复用已注册句柄，丢弃本次打开。
		existing.retired = false
		existing.refs++
		go lex.Close()
		return existing, nil
	}
	e.handles[key] = handle
	return handle, nil
}

// releaseHandle 归还引用；已退役且无引用的句柄立即关闭。
// 删除表项前校验身份，防止误删同键新句柄（review B3）。
func (e *Engine) releaseHandle(handle *revisionHandle) {
	key := handleKey(handle.workspaceKey, handle.manifest.Revision)
	e.mu.Lock()
	handle.refs--
	shouldClose := handle.retired && handle.refs <= 0
	if shouldClose && e.handles[key] == handle {
		delete(e.handles, key)
	}
	e.mu.Unlock()
	if shouldClose {
		_ = handle.lex.Close()
		handle.closeContentFiles()
	}
}

// retireHandles 在新 revision 发布后退役不再保留的句柄
// （只保留 active 与 previous；使用中的句柄延迟到引用归零关闭）。
func (e *Engine) retireHandles(workspaceKey string, activeRevision string, previousRevision string) {
	keep := map[string]bool{
		handleKey(workspaceKey, activeRevision):   true,
		handleKey(workspaceKey, previousRevision): true,
	}
	var closable []*revisionHandle
	e.mu.Lock()
	for key, handle := range e.handles {
		if handle.workspaceKey != workspaceKey || keep[key] {
			continue
		}
		handle.retired = true
		if handle.refs <= 0 {
			delete(e.handles, key)
			closable = append(closable, handle)
		}
	}
	e.mu.Unlock()
	for _, handle := range closable {
		_ = handle.lex.Close()
		handle.closeContentFiles()
	}
}

// renderBlock 是渲染前的合并单元。
type renderBlock struct {
	record chunkRecord
	score  float64
}

// renderHits 把命中渲染为稳定文本格式（golden 锁定，暗坑 K13）：
// 同文件重叠/相邻 chunk 合并，按最高分排序，MaxOutputLen 预算截断。
// 内容按需 pread（D5），候选数受召回深度约束。
func renderHits(handle *revisionHandle, hits []rankedHit, maxOutputLen int) (string, error) {
	if len(hits) == 0 {
		return noHitsText, nil
	}
	blocks := make([]renderBlock, 0, len(hits))
	for _, hit := range hits {
		if _, ok := handle.chunks[hit.id]; !ok {
			continue
		}
		record, err := handle.record(hit.id)
		if err != nil {
			return "", err
		}
		blocks = append(blocks, renderBlock{record: record, score: hit.score})
	}
	if len(blocks) == 0 {
		return noHitsText, nil
	}
	merged := mergeBlocks(blocks)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].score > merged[j].score })

	var out strings.Builder
	budget := maxOutputLen
	if budget <= 0 {
		budget = 20000
	}
	truncated := false
	for i, block := range merged {
		section := formatBlock(block.record)
		if i > 0 && out.Len()+len(section) > budget {
			truncated = true
			break
		}
		out.WriteString(section)
	}
	if truncated {
		out.WriteString("\n[output truncated by max_output_length]\n")
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// mergeBlocks 只合并同文件真正重叠或严格相邻的块（next.Start ≤ current.End+1），
// 取最高分。禁止跨间隙合并：缺行会让 header 行区间与内容错位（review B1）。
func mergeBlocks(blocks []renderBlock) []renderBlock {
	byFile := make(map[string][]renderBlock)
	for _, block := range blocks {
		byFile[block.record.RelPath] = append(byFile[block.record.RelPath], block)
	}
	var merged []renderBlock
	for _, group := range byFile {
		sort.Slice(group, func(i, j int) bool { return group[i].record.StartLine < group[j].record.StartLine })
		current := group[0]
		for _, next := range group[1:] {
			if next.record.StartLine <= current.record.EndLine+1 {
				if next.record.EndLine > current.record.EndLine {
					current.record.Content = current.record.Content + "\n" + tailLines(next.record, current.record.EndLine)
					current.record.EndLine = next.record.EndLine
				}
				if next.score > current.score {
					current.score = next.score
				}
				if current.record.Symbol == "" {
					current.record.Symbol = next.record.Symbol
				}
				continue
			}
			merged = append(merged, current)
			current = next
		}
		merged = append(merged, current)
	}
	return merged
}

// tailLines 取 next 中位于 afterLine 之后的部分，避免合并时内容重复。
func tailLines(record chunkRecord, afterLine int) string {
	skip := afterLine - record.StartLine + 1
	if skip <= 0 {
		return record.Content
	}
	lines := strings.Split(record.Content, "\n")
	if skip >= len(lines) {
		return ""
	}
	return strings.Join(lines[skip:], "\n")
}

// formatBlock 渲染单个块：`## path:start-end [symbol]` + 代码围栏。
func formatBlock(record chunkRecord) string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(record.RelPath)
	b.WriteString(":")
	fmt.Fprintf(&b, "%d-%d", record.StartLine, record.EndLine)
	if record.Symbol != "" {
		b.WriteString(" ")
		b.WriteString(record.Symbol)
	}
	b.WriteString("\n```")
	if record.Language != "" && record.Language != "text" {
		b.WriteString(record.Language)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(record.Content, "\n"))
	b.WriteString("\n```\n\n")
	return b.String()
}
