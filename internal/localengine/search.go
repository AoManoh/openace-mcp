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
	"unicode/utf8"

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

// retrieval 是一次检索的核心产物（渲染前）；handle 由调用方负责释放。
type retrieval struct {
	handle     *revisionHandle
	ordered    []rankedHit
	mode       string
	reasons    []string
	coverage   string
	syncResult engine.Result
	// 方案④ 质量字段素材:实际送审精排数 / 精排是否生效 / 查询嵌入失败。
	rerankSent       int
	rerankApplied    bool
	queryEmbedFailed bool
	// queryPlan 是路由分立规划记录(方案 -13,触发时非空)。
	queryPlan string
}

// Search 实现 engine.Service：按需同步后执行 lexical（+dense RRF）
// （+optional rerank）检索；降级行为受 D8 支配，禁止静默。
func (e *Engine) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	out, err := e.retrieve(ctx, req)
	if err != nil {
		return engine.Result{}, err
	}
	handle := out.handle
	defer e.releaseHandle(handle)

	text, renderErr := renderHits(handle, out.ordered, req.MaxOutputLen)
	if renderErr != nil {
		return engine.Result{}, renderErr
	}
	degradedReason := strings.Join(out.reasons, ",")
	if degradedReason != "" {
		text = degradedBanner(degradedReason, out.mode, out.coverage) + text
	}
	result := out.syncResult
	result.Text = text
	result.Engine = EngineID
	result.IndexRevision = handle.manifest.Revision
	// 路由分立可审计记录:触发即携带(含纯词法形态),未触发保持空
	// (omitempty,wire 不变)。
	result.QueryPlan = out.queryPlan
	// 透明性字段：provider 已配置或发生降级时填充；纯词法正常路径
	// 保持空（Stage 2 wire 不变，K32/K34）。
	if e.semanticEnabled() || e.rerankClient != nil || degradedReason != "" {
		result.RetrievalMode = out.mode
		result.DegradedReason = degradedReason
		result.SemanticCoverage = out.coverage
		// 方案④ 质量字段:rerank 实际送审数 / 查询嵌入失败 / 语义身份。
		result.RerankSent = out.rerankSent
		result.QueryEmbedFailed = out.queryEmbedFailed
		if e.semanticEnabled() {
			result.EmbeddingProfile = fmt.Sprintf("%s/%s/%d",
				e.embedCfg.ProviderType, e.embedCfg.Model, e.embedCfg.Dimension)
		}
	}
	return result, nil
}

// CandidateRef 是评测 harness 可见的候选块引用（Stage 5 P5A-T03 专用
// hook；不进入 MCP 工具面）。
type CandidateRef struct {
	ID        string
	RelPath   string
	StartLine int
	EndLine   int
}

// SearchCandidates 返回渲染前的最终候选块序（含精排效果），供评测
// harness 按 doc/文件粒度评分；语义与 Search 完全一致，仅省去渲染。
func (e *Engine) SearchCandidates(ctx context.Context, req engine.SearchRequest) ([]CandidateRef, error) {
	out, err := e.retrieve(ctx, req)
	if err != nil {
		return nil, err
	}
	defer e.releaseHandle(out.handle)
	candidates := make([]CandidateRef, 0, len(out.ordered))
	for _, hit := range out.ordered {
		meta, ok := out.handle.chunks[hit.id]
		if !ok {
			continue
		}
		candidates = append(candidates, CandidateRef{
			ID: hit.id, RelPath: meta.RelPath, StartLine: meta.StartLine, EndLine: meta.EndLine,
		})
	}
	return candidates, nil
}

// ChunkDocTexts 按 chunk ID 取回 rerank 送审文本（T10b-4 head 定值
// harness 专用 hook，不进入 MCP 工具面）：与 shipped rerank 路径共用
// rerankDocText 构造，保证离线打分与在线送审逐字节同文本。未知 ID
// 静默跳过（调用方按返回 map 对齐）。
func (e *Engine) ChunkDocTexts(ctx context.Context, ref engine.WorkspaceRef, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return nil, err
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		return nil, err
	}
	defer e.releaseHandle(handle)
	texts := make(map[string]string, len(ids))
	for _, id := range ids {
		record, err := handle.record(id)
		if err != nil {
			continue
		}
		texts[id] = rerankDocText(record)
	}
	return texts, nil
}

// ChunkDumpRecord 是评测 harness 导出的 chunk 全量记录(E3 嵌入模板
// A/B 专用 hook,不进入 MCP 工具面):字段与索引内 chunkRecord 对齐,
// 供离线按模板重组文本、直连 provider 重嵌与召回对比。
type ChunkDumpRecord struct {
	ID        string `json:"id"`
	RelPath   string `json:"path"`
	Language  string `json:"language"`
	StartLine int    `json:"start"`
	EndLine   int    `json:"end"`
	Symbol    string `json:"symbol,omitempty"`
	Content   string `json:"content"`
}

// DumpChunkRecords 按 chunk ID 升序遍历当前 revision 存活集并逐条回调
// (E3 专用 hook)。与查询路径共享句柄与存活过滤语义;emit 返回错误即中止。
func (e *Engine) DumpChunkRecords(ctx context.Context, ref engine.WorkspaceRef, emit func(ChunkDumpRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return err
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		return err
	}
	defer e.releaseHandle(handle)
	ids := make([]string, 0, len(handle.chunks))
	for id := range handle.chunks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record, err := handle.record(id)
		if err != nil {
			continue
		}
		if err := emit(ChunkDumpRecord{
			ID: record.ID, RelPath: record.RelPath, Language: record.Language,
			StartLine: record.StartLine, EndLine: record.EndLine,
			Symbol: record.Symbol, Content: record.Content,
		}); err != nil {
			return err
		}
	}
	return nil
}

// RouteCandidates 是融合前的双路召回镜像（Stage 5 T10b 专用 hook，
// 不进入 MCP 工具面）：词法与语义候选各按原始路内序返回，供离线融合
// 参数扫描（RRF k、召回深度、子句权重交互）复用已付费的 query
// embedding 结果——参数扫描零新增嵌入费用（裁决 A1 的执行机制）。
type RouteCandidates struct {
	Lex   []CandidateRef
	Dense []CandidateRef
	// Reasons 是语义路降级原因（如有）；Dense 为 nil 且 Reasons 为空
	// 表示语义未配置或覆盖为空。
	Reasons []string
}

// SearchRoutes 返回融合前双路候选（每路深度 depth，不融合、不精排、
// 不渲染）。与 retrieve 共享同步、句柄与存活过滤语义。
func (e *Engine) SearchRoutes(ctx context.Context, req engine.SearchRequest, depth int) (RouteCandidates, error) {
	if err := rejectProfileID(req.Workspace); err != nil {
		return RouteCandidates{}, err
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return RouteCandidates{}, engine.AsInvalidRequest(errors.New("查询内容为空"))
	}
	if depth <= 0 {
		depth = hybridRouteTopK
	}
	if _, syncErr := e.syncWorkspace(ctx, req.Workspace); syncErr != nil {
		return RouteCandidates{}, syncErr
	}
	_, workspaceKey, err := e.resolveRoot(req.Workspace.DirectoryPath)
	if err != nil {
		return RouteCandidates{}, err
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		return RouteCandidates{}, err
	}
	defer e.releaseHandle(handle)

	// 与 retrieve 同口径应用路由分立(方案 -13),保证 routes dump 诊断
	// 与生产词法路一致;dense 路仍用原查询。
	lexQuery := query
	if plan := planLexicalQuery(query); plan.Triggered {
		lexQuery = plan.LexicalQuery
	}
	lexHits, err := handle.lex.SearchWeighted(ctx, lexQuery, depth, e.lexWeights)
	if err != nil {
		return RouteCandidates{}, fmt.Errorf("词法检索: %w", err)
	}
	if lexQuery != query && len(lexHits) == 0 {
		lexHits, err = handle.lex.SearchWeighted(ctx, query, depth, e.lexWeights)
		if err != nil {
			return RouteCandidates{}, fmt.Errorf("词法检索(回退): %w", err)
		}
	}
	lexHits = filterLiveHits(handle, lexHits)
	out := RouteCandidates{Lex: make([]CandidateRef, 0, len(lexHits))}
	toRef := func(id string) (CandidateRef, bool) {
		meta, ok := handle.chunks[id]
		if !ok {
			return CandidateRef{}, false
		}
		return CandidateRef{ID: id, RelPath: meta.RelPath, StartLine: meta.StartLine, EndLine: meta.EndLine}, true
	}
	for _, hit := range lexHits {
		if ref, ok := toRef(hit.ID); ok {
			out.Lex = append(out.Lex, ref)
		}
	}
	if e.semanticEnabled() {
		denseIDs, reasons, denseErr := e.denseRoute(ctx, workspaceKey, handle, query, depth)
		if denseErr != nil {
			return RouteCandidates{}, denseErr
		}
		out.Reasons = reasons
		if denseIDs != nil {
			out.Dense = make([]CandidateRef, 0, len(denseIDs))
			for _, id := range denseIDs {
				if ref, ok := toRef(id); ok {
					out.Dense = append(out.Dense, ref)
				}
			}
		}
	}
	return out, nil
}

// retrieve 执行检索核心（同步→句柄→双路召回→融合→精排），返回渲染前
// 候选；错误路径不返回句柄。
func (e *Engine) retrieve(ctx context.Context, req engine.SearchRequest) (retrieval, error) {
	if err := rejectProfileID(req.Workspace); err != nil {
		return retrieval{}, err
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		// P7:请求类标记,daemon 面映射 400 而非 502。
		return retrieval{}, engine.AsInvalidRequest(errors.New("查询内容为空"))
	}
	// 检索前确保索引就绪（与 legacy Syncer 的 retrieval 语义一致）。
	syncResult, syncErr := e.syncWorkspaceForQuery(ctx, req.Workspace)
	if syncErr != nil && ctx.Err() != nil {
		return retrieval{}, ctx.Err()
	}
	_, workspaceKey, err := e.resolveRoot(req.Workspace.DirectoryPath)
	if err != nil {
		return retrieval{}, err
	}
	handle, handleErr := e.acquireHandle(workspaceKey)
	var reasons []string
	if syncErr != nil {
		// D8(d)/review S23：索引刷新失败但存在可用 revision 时，
		// allow 以旧索引服务并显式标记 stale，deny 报错。查询等待在建
		// 索引超界(P1 有界化)同构处理,原因区分为 index-building。
		if handleErr != nil {
			return retrieval{}, syncErr
		}
		reason, label := "stale-index", "index refresh failed"
		if errors.Is(syncErr, errQueryBuildWait) {
			reason, label = "index-building", "index still building"
		}
		if e.retrievalDegrade == DegradeDeny {
			e.releaseHandle(handle)
			return retrieval{}, degradeDeniedError(label, syncErr, EnvRetrievalDegrade)
		}
		reasons = append(reasons, reason)
		syncResult = engine.Result{Engine: EngineID, FileCount: handle.manifest.Counts.Files}
	} else if handleErr != nil {
		return retrieval{}, handleErr
	}

	mode := "lexical"
	lexTopK := defaultTopK
	if e.semanticEnabled() {
		lexTopK = hybridRouteTopK
	}
	// 路由分立(方案 -13):含结构 token 的非 CJK 自然语言查询,词法路
	// 改用结构 token 变体聚焦;dense 与 rerank 路保持原查询。零命中
	// 兜底回退原查询,不触发时行为与历史逐字节一致。
	plan := planLexicalQuery(query)
	planLabel := ""
	lexQuery := query
	if plan.Triggered {
		lexQuery = plan.LexicalQuery
		planLabel = plan.LexicalQuery
	}
	lexHits, err := handle.lex.SearchWeighted(ctx, lexQuery, lexTopK, e.lexWeights)
	if err != nil {
		e.releaseHandle(handle)
		return retrieval{}, fmt.Errorf("词法检索: %w", err)
	}
	if plan.Triggered && len(lexHits) == 0 {
		planLabel += " fallback=original"
		lexHits, err = handle.lex.SearchWeighted(ctx, query, lexTopK, e.lexWeights)
		if err != nil {
			e.releaseHandle(handle)
			return retrieval{}, fmt.Errorf("词法检索(回退): %w", err)
		}
	}
	// 统一过滤 choke point（暗坑 K39/K44）：两路召回都可能命中旧
	// segment 中被 supersede/tombstone 的死 chunk，进入融合前按存活
	// 集过滤，杜绝死内容占据候选位。
	lexHits = filterLiveHits(handle, lexHits)

	var ordered []rankedHit
	coverage := ""
	if e.semanticEnabled() {
		denseIDs, denseReasons, denseErr := e.denseRoute(ctx, workspaceKey, handle, query, hybridRouteTopK)
		if denseErr != nil {
			e.releaseHandle(handle)
			return retrieval{}, denseErr
		}
		reasons = append(reasons, denseReasons...)
		lexIDs := make([]string, 0, len(lexHits))
		for _, hit := range lexHits {
			lexIDs = append(lexIDs, hit.ID)
		}
		if denseIDs != nil {
			mode = "hybrid"
			fused := fusion.RRFWeighted(lexIDs, denseIDs, e.fusionParams())
			ids := make([]string, 0, len(fused))
			for _, f := range fused {
				ids = append(ids, f.ID)
			}
			// 词法锚(T11 业务冒烟发现的边界):dense 路存活但语义失真
			// (弱模型/错配)时,加权融合会把词法唯一强命中压出精排窗口。
			// 保证词法首位进入 rerank head 窗口——四语料 12,497 查询离线
			// 复算证明零质量代价(R@5/R@10 逐位不变),而精排从此必然
			// "看得见"最强词法信号;窗口 5 的激进版被证伪(cosqa -3.6pp)。
			if len(lexIDs) > 0 {
				ids = anchorWithinWindow(ids, lexIDs[0], rerankHeadLimit)
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
	rerankApplied := false
	rerankSent := 0
	if e.rerankClient != nil && len(ordered) > 0 {
		reordered, applied, sent, rerankReason, rerankErr := e.rerankOrder(ctx, handle, query, ordered)
		if rerankErr != nil {
			e.releaseHandle(handle)
			return retrieval{}, rerankErr
		}
		if applied {
			mode += "+rerank"
			ordered = reordered
			rerankApplied = true
			rerankSent = sent
		}
		if rerankReason != "" {
			reasons = append(reasons, rerankReason)
		}
	}

	out := retrieval{
		handle: handle, ordered: ordered, mode: mode,
		reasons: reasons, coverage: coverage, syncResult: syncResult,
		rerankSent: rerankSent, rerankApplied: rerankApplied,
		queryEmbedFailed: hasQueryEmbedFailure(reasons),
		queryPlan:        planLabel,
	}
	// 方案④ strict 闸:语义链路任一缺口显式报错而非降级放行。触发面 =
	// 任何降级 reason(覆盖缺口/查询嵌入失败/rerank 跳过/stale/向量不可用)
	// ∪ 配置了 rerank 但未生效。错误指名 env 与缺口 token,供调用方定位。
	if e.qualityStrict {
		violations := append([]string(nil), reasons...)
		if e.rerankClient != nil && !rerankApplied && len(ordered) > 0 && !containsRerankReason(reasons) {
			violations = append(violations, "rerank-not-applied")
		}
		if len(violations) > 0 {
			e.releaseHandle(handle)
			return retrieval{}, fmt.Errorf("%s=on: 语义质量缺口 [%s],拒绝返回不完整结果(改用 %s=off 可降级放行)",
				EnvQualityStrict, strings.Join(violations, ","), EnvQualityStrict)
		}
	}
	return out, nil
}

// hasQueryEmbedFailure 判定降级原因中是否含查询嵌入失败(方案④字段)。
func hasQueryEmbedFailure(reasons []string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, "query-embedding-failed") {
			return true
		}
	}
	return false
}

// containsRerankReason 判定 reasons 是否已含 rerank 缺口 token(避免
// strict 违规清单重复)。
func containsRerankReason(reasons []string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, "rerank-skipped") {
			return true
		}
	}
	return false
}

// 机制②-B(locale 类别负先验)已于 2026-08-05 经证据裁决移除(决策台账
// §7 -8):门 B 复扫证明 hybrid 下惩罚在全部防护面零收益(防护由机制 A
// latin guard 与 dense 融合承担),代价集中于 locale-gold 查询(sealed v2
// 门 B 翻转实例;关闭后 dev 端到端 R@5 +15pp)。回归钉板:locale_gold_test.go。

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

// denseRoute 执行语义召回：返回 depth 深度的 dense 候选 ID（nil 表示
// 语义路未执行）、降级原因与致命错误（ctx 取消或 deny 拒绝）。多
// segment 逐段 exact 检索后确定性归并（score desc, ID asc，暗坑 K27），
// 死 chunk 在归并后过滤（暗坑 K39）。
func (e *Engine) denseRoute(ctx context.Context, workspaceKey string, handle *revisionHandle, query string, depth int) ([]string, []string, error) {
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
		segmentHits, searchErr := ix.Search(ctx, queryCopy, depth)
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
	ids := make([]string, 0, depth)
	for _, hit := range merged {
		if _, live := handle.chunks[hit.ID]; !live {
			continue
		}
		ids = append(ids, hit.ID)
		if len(ids) >= depth {
			break
		}
	}
	return ids, nil, nil
}

// rerankOrder 精排 ordered 头部（≤rerankHeadLimit，再受 token 预算截断），
// 未送审部分按原序跟随（暗坑 K28）。送审集与 ordered 显式对齐：chunks
// 表缺失的头部候选（防御性路径）保持原位跟随，禁止因 docs/ordered
// 错位造成条目重复或丢失（Stage 3 自审修复）；已送审但 provider 未返回
// 的条目按原序补回并显式上报（H1 兜底，见 rerankAssembleOrder）。
func (e *Engine) rerankOrder(ctx context.Context, handle *revisionHandle, query string, ordered []rankedHit) ([]rankedHit, bool, int, string, error) {
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
			return nil, false, 0, "", err
		}
		docs = append(docs, rerank.Document{ID: hit.id, Text: rerankDocText(record)})
		included = append(included, hit)
	}
	hits, sent, err := e.rerankClient.Rerank(ctx, query, docs)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, 0, "", ctx.Err()
		}
		if e.rerankDegrade == DegradeDeny {
			return nil, false, 0, "", degradeDeniedError("rerank failed", err, EnvRerankDegrade)
		}
		return nil, false, 0, "rerank-skipped(" + failureClass(err) + ")", nil
	}
	if sent == 0 {
		return nil, false, 0, "rerank-skipped(token-budget)", nil
	}
	ids, missing := rerankAssembleOrder(hits, included, sent, skippedHead, ordered[head:])
	reason := ""
	if missing > 0 {
		// 防御纵深（H1）：client 层 all-or-nothing 校验保证正常路径
		// missing==0，此分支仅在该校验被绕过的形状下兜底——候选已按原
		// 序补回，但精排只覆盖了部分送审集，必须显式上报（决策 11），
		// 不得静默冒充完整精排。
		reason = "rerank-partial-response"
	}
	return rankByPosition(ids), true, sent, reason, nil
}

// rerankAssembleOrder 组装精排最终序：重排命中 → 已送审但 provider 未
// 返回的条目（原序补回，H1 兜底；正常路径为空——client 端对条数不足
// 已按 malformed 整体拒绝）→ 因 token 预算未送审的头部（原序，K28）→
// chunks 缺失的头部（原序）→ 头部之外的尾部（原序）。返回最终 ID 序与
// 兜底补回条数；任何响应形状下已召回候选不重复、不丢失（P3-T04）。
func rerankAssembleOrder(hits []rerank.Hit, included []rankedHit, sent int, skippedHead []rankedHit, tail []rankedHit) ([]string, int) {
	ids := make([]string, 0, len(hits)+len(included)+len(skippedHead)+len(tail))
	returned := make(map[string]bool, len(hits))
	for _, hit := range hits {
		returned[hit.ID] = true
		ids = append(ids, hit.ID)
	}
	missing := 0
	for _, hit := range included[:sent] {
		if returned[hit.id] {
			continue
		}
		missing++
		ids = append(ids, hit.id)
	}
	for _, hit := range included[sent:] {
		ids = append(ids, hit.id)
	}
	for _, hit := range skippedHead {
		ids = append(ids, hit.id)
	}
	for _, hit := range tail {
		ids = append(ids, hit.id)
	}
	return ids, missing
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

// anchorWithinWindow 把 anchor 提升到窗口内末位(若在窗口外);
// 窗口内命中保持原位,确定性重排不引入分数语义。
func anchorWithinWindow(ids []string, anchor string, window int) []string {
	if window <= 0 || len(ids) <= window {
		return ids
	}
	idx := -1
	for i, id := range ids {
		if id == anchor {
			idx = i
			break
		}
	}
	if idx < 0 || idx < window {
		return ids
	}
	item := ids[idx]
	copy(ids[window:idx+1], ids[window-1:idx])
	ids[window-1] = item
	return ids
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
		if i == 0 && len(section) > budget {
			// F3(review 2026-08-06):首块超预算硬截断到预算并打标——
			// 历史豁免使 MaxOutputLen=200 也可能返回数 KB,静默打爆
			// 调用方的上下文预算管理。按字节截断可能落在多字节字符
			// 中间,回退到最近的合法 UTF-8 边界。
			cut := budget
			for cut > 0 && !utf8.RuneStart(section[cut]) {
				cut--
			}
			out.WriteString(section[:cut])
			truncated = true
			break
		}
		if i > 0 && out.Len()+len(section) > budget {
			truncated = true
			break
		}
		out.WriteString(section)
	}
	if truncated {
		out.WriteString(truncationMarker)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// truncationMarker 是输出预算截断的稳定标记(golden/调用方可依赖)。
const truncationMarker = "\n[output truncated by max_output_length]\n"

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
