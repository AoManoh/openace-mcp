package localengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/chunk"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/reliability"
	"github.com/AoManoh/openace-mcp/internal/rerank"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// noHitsText 是没有命中时返回的正文。MCP 层对空正文也补同一句话，两层
// 用同一文案，调用方看到的无结果输出只有一种形态。
const noHitsText = "No relevant code sections were found."

// chunkMeta 是一个存活 chunk 常驻内存的元数据。chunk 内容本体不常驻：
// 渲染与精排需要内容时按 offset/length 从所属 segment（revision 内不可变
// 的索引分段）的 chunks.jsonl 文件 pread 出那一行 JSON 再解码。这样查询
// 进程的内存只随 chunk 数增长，不随仓库文本总量增长。
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

// revisionHandle 是一个已打开的 revision（一次发布的不可变索引快照）的
// 只读句柄，按引用计数管理：查询期间持有引用，发布新 revision 后旧句柄
// 等到引用归零才关闭，正在进行的查询不会读到被替换或被回收的数据，
// Bleve 索引句柄也不会泄漏。
//   - lex 把 revision 的全部 segment 用 Bleve 的 IndexAlias 聚合成一个词法
//     索引，一次查询覆盖所有分段。
//   - chunks 只含存活 chunk 的元数据。同一文件在多个 segment 里有多个版本
//     时只保留最新 segment 的（newest-wins），已删除或改名文件的 chunk
//     不在其中。词法与语义两路召回都用它过滤，已删除内容从任一路都进
//     不了结果。
//   - 向量索引在首次语义查询时才加载，加载后随句柄常驻；revision 不可变，
//     加载一次即可。
type revisionHandle struct {
	engine       *Engine
	workspaceKey string
	manifest     *index.Manifest
	lex          *lexical.Index
	chunks       map[string]chunkMeta
	segmentDirs  []string
	refs         int
	retired      bool

	// contentMu 保护 contentFiles 的按需打开。每个 segment 的 chunks.jsonl
	// 最多打开一次并保留到句柄关闭；segment 不可变，文件描述符数不超过
	// 段数，段数又被 compaction 阈值（8 段）限制。
	contentMu    sync.Mutex
	contentFiles []*os.File

	vecOnce sync.Once
	vecIxs  []*vector.Index
	vecKeys []string
	vecErr  error

	// repoMapFiles 缓存 repo_map 的按文件聚合结果。revision 不可变，聚合
	// 一次即可；大仓库每次重扫全部 chunkMeta 要秒级。focus 只过滤这份缓存。
	repoMapOnce  sync.Once
	repoMapFiles []mapFile
}

// record 按需取回一个存活 chunk 的完整记录：查 chunks 得到所属 segment 与
// 文件内偏移，按需打开该 segment 的 chunks.jsonl，pread 出那一行并解码。
// 解码后核对记录里的 ID 与请求的 ID 一致：偏移错位时返回错误，而不是把
// 别的 chunk 内容当成这个 chunk 渲染出去。不在存活集里的 ID 直接报错。
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

// closeContentFiles 关闭 record 按需打开的 chunks.jsonl 文件，由句柄关闭
// 路径调用。
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

// sharedVectorIndex 是 Engine 级向量段缓存的一个条目：ready 在加载结束时
// 关闭，ix 与 err 只在 ready 关闭后可读；refs 是当前持有该段的句柄数。
type sharedVectorIndex struct {
	ready chan struct{}
	ix    *vector.Index
	err   error
	refs  int
}

func vectorSegmentKey(dir string, dimension int, dataChecksum string, indexChecksum string) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", dir, dimension, dataChecksum, indexChecksum)
}

// acquireVectorSegment 从 Engine 级缓存取得一个 segment 的向量索引并增加
// 一个引用。active 与 previous 两个 revision 共享同一 segment 时只常驻一份。
//   - 缓存已有该段：在锁内加引用，在锁外等待 ready，不持锁等磁盘加载。
//     加载失败或行数超过 maxVectors 时归还引用并返回错误。
//   - 缓存没有该段：先登记占位再在锁外加载，加载结束写回结果并关闭
//     ready；加载失败时删除占位，后续调用会重新加载。
//
// 缓存键含目录、维度与数据/索引两个 checksum，文件内容变化后不会命中
// 旧条目。
func (e *Engine) acquireVectorSegment(dir string, dimension int, dataChecksum string, indexChecksum string, maxVectors int) (*vector.Index, string, error) {
	key := vectorSegmentKey(dir, dimension, dataChecksum, indexChecksum)
	e.vectorMu.Lock()
	if shared, ok := e.vectorSegments[key]; ok {
		shared.refs++
		e.vectorMu.Unlock()
		<-shared.ready
		if shared.err != nil {
			e.releaseVectorSegments([]string{key})
			return nil, "", shared.err
		}
		if maxVectors > 0 && shared.ix.Count() > maxVectors {
			e.releaseVectorSegments([]string{key})
			return nil, "", fmt.Errorf("%w: %d > %d", vector.ErrEnvelopeExceeded, shared.ix.Count(), maxVectors)
		}
		return shared.ix, key, nil
	}
	shared := &sharedVectorIndex{ready: make(chan struct{}), refs: 1}
	e.vectorSegments[key] = shared
	e.vectorMu.Unlock()

	ix, err := vector.Load(dir, dimension, dataChecksum, indexChecksum, maxVectors)
	e.vectorMu.Lock()
	shared.ix, shared.err = ix, err
	close(shared.ready)
	if err != nil {
		delete(e.vectorSegments, key)
	}
	e.vectorMu.Unlock()
	if err != nil {
		return nil, "", err
	}
	return ix, key, nil
}

// releaseVectorSegments 归还一组段引用。引用归零的段从缓存移除并 Close：
// mmap 形态下 Close 会解除映射并关闭文件；revision GC 与 compaction 要删除
// 段目录，而 Windows 不允许删除仍被映射的文件，所以必须先走到这里。
// Close 时没有正在进行的检索：检索期间调用方一直经 revisionHandle 的
// 引用计数持有这些段。
func (e *Engine) releaseVectorSegments(keys []string) {
	if e == nil || len(keys) == 0 {
		return
	}
	var closable []*vector.Index
	e.vectorMu.Lock()
	for _, key := range keys {
		shared, ok := e.vectorSegments[key]
		if !ok {
			continue
		}
		shared.refs--
		if shared.refs <= 0 {
			delete(e.vectorSegments, key)
			if shared.ix != nil {
				closable = append(closable, shared.ix)
			}
		}
	}
	e.vectorMu.Unlock()
	for _, ix := range closable {
		_ = ix.Close()
	}
}

// vectorIndexes 在首次调用时加载本 revision 全部 segment 的向量索引，之后
// 返回同一结果。
//   - revision 没有向量数据：返回错误，调用方按语义路不可用处理。
//   - 任一段校验失败：整体返回该错误并释放已取得的段。只有语义路受影响，
//     词法检索照常。
//   - 有 engine 时经 Engine 级段缓存取段，active 与 previous 共享的段只常驻
//     一份；engine 为 nil（测试直连）时直接 vector.Load。
//   - 用户配置了 OPENACE_VECTOR_MEMORY_BUDGET 时，每段加载前把剩余行数预算
//     传给加载器，累计超出即停止并返回 vector.ErrEnvelopeExceeded，不会先
//     把全部段读进内存再发现超预算。默认不限。
func (h *revisionHandle) vectorIndexes(dimension int) ([]*vector.Index, error) {
	h.vecOnce.Do(func() {
		if !h.manifest.HasVectors() {
			h.vecErr = errors.New("revision 无向量数据")
			return
		}
		total := 0
		limit := 0 // 0 表示不限（默认）；>0 来自 OPENACE_VECTOR_MEMORY_BUDGET 折算的行数。
		if h.engine != nil {
			limit = h.engine.vectorMaxResident
		}
		indexes := make([]*vector.Index, 0, len(h.manifest.Segments))
		keys := make([]string, 0, len(h.manifest.Segments))
		for i, segment := range h.manifest.Segments {
			if segment.VectorsChecksum == "" {
				continue
			}
			remaining := 0
			if limit > 0 {
				remaining = limit - total
				if remaining <= 0 {
					h.vecErr = fmt.Errorf("%w: cumulative segment rows exceed %d", vector.ErrEnvelopeExceeded, limit)
					break
				}
			}
			var ix *vector.Index
			var key string
			var err error
			if h.engine != nil {
				ix, key, err = h.engine.acquireVectorSegment(h.segmentDirs[i], dimension,
					segment.VectorsChecksum, segment.VectorsIndexChecksum, remaining)
			} else {
				ix, err = vector.Load(h.segmentDirs[i], dimension,
					segment.VectorsChecksum, segment.VectorsIndexChecksum, remaining)
			}
			if err != nil {
				h.vecErr = err
				break
			}
			total += ix.Count()
			indexes = append(indexes, ix)
			if key != "" {
				keys = append(keys, key)
			}
		}
		if h.vecErr != nil {
			if h.engine != nil {
				h.engine.releaseVectorSegments(keys)
			} else {
				for _, ix := range indexes {
					_ = ix.Close()
				}
			}
			return
		}
		h.vecIxs = indexes
		h.vecKeys = keys
	})
	return h.vecIxs, h.vecErr
}

func (h *revisionHandle) releaseVectorIndexes() {
	if h.engine != nil {
		h.engine.releaseVectorSegments(h.vecKeys)
	} else {
		// engine 为 nil 的直连加载（测试路径）没有共享缓存，索引由本句柄
		// 独占，直接 Close 释放映射或堆数据。
		for _, ix := range h.vecIxs {
			_ = ix.Close()
		}
	}
	h.vecKeys = nil
	h.vecIxs = nil
}

// loadLiveChunkMetas 顺序扫描 revision 全部 segment 的 chunks.jsonl，产出
// 存活 chunk 的元数据与各记录在文件内的偏移；内容本体不保留在内存。
// 存活判定分两步：同一相对路径出现在多个 segment 时，后面（更新）的
// segment 整体覆盖前面的记录（newest-wins）；路径不在 manifest.Files 里
// （文件已删除或改名）的记录全部丢弃。
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
		// manifest.Segments 按构建先后排列，后面的 segment 含同一文件的更新
		// 版本，整组覆盖前面的记录。
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

// rankedHit 是进入渲染的最终排序候选。score 只用于渲染时同文件块合并后
// 的跨块排序，越大越靠前：纯词法模式下是真实 BM25 分数，其他模式下是按
// 最终名次合成的 1/(名次+1)。reranked、rerankScore、source 逐条带到结果
// 的 hits[] 里：只给聚合值 rerank_sent 时，调用方分不出第 51 位起哪些块
// 没有精排，也不知道某条命中来自哪一路。
type rankedHit struct {
	id    string
	score float64
	// reranked 表示该候选进入精排窗口并被 provider 打分；rerankScore 是
	// provider 返回的相关度，只在 reranked 为 true 时有意义。
	reranked    bool
	rerankScore float64
	// source 是融合来源（fusion.SourceLexical、SourceDense、SourceBoth）；
	// 纯词法模式恒为 fusion.SourceLexical。
	source string
}

// retrieval 是一次检索在渲染之前的全部产物。handle 由调用方负责释放。
type retrieval struct {
	handle *revisionHandle
	// ordered 是最终候选序；mode 是检索模式（lexical、hybrid，精排生效时
	// 追加 +rerank）；reasons 是降级原因列表；coverage 是语义覆盖率文本。
	ordered    []rankedHit
	mode       string
	reasons    []string
	coverage   string
	syncResult engine.Result
	// rerankSent 是实际送审精排的候选数，queryEmbedFailed 表示本次查询嵌入
	// 失败，两者复制进 Result 的质量字段；rerankApplied 表示精排结果已生效，
	// 只决定 mode 的 "+rerank" 后缀并作为 OPENACE_QUALITY_STRICT 判定的输入，
	// 不进入 Result。
	rerankSent       int
	rerankApplied    bool
	queryEmbedFailed bool
	// queryPlan 记录本次查询的规划：词法路改用结构 token 查询时为该查询
	// 串（零命中回退时追加 " fallback=original"），另追加 path_prefix=… 与
	// artifact_kind=… 标签；全部未触发时为空。
	queryPlan string
	// timings 是各阶段耗时；RenderMs 与 TotalMs 由 Search 在渲染后补齐。
	timings *engine.RetrievalTimings
	started time.Time
}

// Search 实现 engine.Service：按需同步索引后执行检索并渲染正文。检索链
// 为词法召回，配置了 embedding provider 时加语义召回并做 RRF 融合，配置
// 了 rerank provider 时对头部候选精排。
//   - 任一降级原因存在时，正文首行加 [DEGRADED] 横幅，原因逐条写出；是否
//     放行由 OPENACE_RETRIEVAL_DEGRADE / OPENACE_RERANK_DEGRADE 决定，不在
//     任何情况下静默返回不完整结果。
//   - 结果恒携带 Timings、Hits 与 QueryPlan（未触发为空，序列化时省略）。
//   - RetrievalMode 等透明性字段只在配置了 provider 或发生降级时填充；
//     纯词法且无降级时保持空值，输出形状与最初的纯词法版本完全一致，
//     新旧进程混布时旧端仍能解析。
func (e *Engine) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	out, err := e.retrieve(ctx, req)
	if err != nil {
		return engine.Result{}, err
	}
	handle := out.handle
	defer e.releaseHandle(handle)

	detail, err := parseDetail(req.Detail)
	if err != nil {
		return engine.Result{}, err
	}
	renderStart := time.Now()
	rendered, renderErr := renderHitsDetail(handle, out.ordered, req.FullResults, detail)
	if renderErr != nil {
		return engine.Result{}, renderErr
	}
	text := rendered.text
	if out.timings != nil {
		out.timings.RenderMs = time.Since(renderStart).Milliseconds()
		out.timings.TotalMs = time.Since(out.started).Milliseconds()
	}
	degradedReason := strings.Join(out.reasons, ",")
	if degradedReason != "" {
		text = degradedBanner(degradedReason, out.mode, out.coverage) + text
	}
	result := out.syncResult
	result.Text = text
	result.Engine = EngineID
	result.IndexRevision = handle.manifest.Revision
	// 阶段耗时随每次检索结果返回，字段为 omitempty 的加性字段。
	result.Timings = out.timings
	// 结构化 hits 清单与展示统计：每个合并后的候选块一条，调用方可以按
	// 头行对没有正文的块自行 Read。
	result.Hits = rendered.hits
	if rendered.display.CandidateBlocks > 0 {
		display := rendered.display
		result.Display = &display
	}
	// 查询规划记录只要触发就携带（纯词法模式也一样），未触发保持空，
	// 序列化时省略，旧调用方看到的形状不变。
	result.QueryPlan = out.queryPlan
	// 透明性字段只在配置了 provider 或发生降级时填充；纯词法且无降级时
	// 保持空值，输出形状与最初的纯词法版本一致。
	if e.semanticEnabled() || e.rerankClient != nil || degradedReason != "" {
		result.RetrievalMode = out.mode
		result.DegradedReason = degradedReason
		result.SemanticCoverage = out.coverage
		// 质量字段：实际送审精排数、查询嵌入是否失败、语义路身份指纹。
		result.RerankSent = out.rerankSent
		result.QueryEmbedFailed = out.queryEmbedFailed
		if e.semanticEnabled() {
			result.EmbeddingProfile = fmt.Sprintf("%s/%s/%d",
				e.embedCfg.ProviderType, e.embedCfg.Model, e.embedCfg.Dimension)
		}
	}
	return result, nil
}

// CandidateRef 是评测工具（cmd/openace-bench）使用的候选块引用，只含定位
// 信息；不进入 MCP 工具面。
type CandidateRef struct {
	ID        string
	RelPath   string
	StartLine int
	EndLine   int
}

// SearchCandidates 返回渲染前的最终候选块序（已含精排效果），供评测工具
// 按块或文件粒度评分。同步、召回、融合、精排与 Search 走同一条 retrieve
// 路径，只省去渲染；不在存活集里的 ID 跳过。
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

// ChunkDocTexts 按 chunk ID 取回 rerank 送审文本，供评测工具离线给候选打
// 分；不进入 MCP 工具面。文本用与生产精排相同的 rerankDocText 构造，离线
// 打分与在线送审逐字节同文本。未知或读取失败的 ID 不出现在返回 map 里，
// 调用方按 map 的键匹配，不报错。不触发同步。
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

// ChunkDumpRecord 是评测工具导出的 chunk 完整记录，不进入 MCP 工具面。
// 字段与索引内的 chunkRecord 一一对应，供离线实验按不同模板重组嵌入文
// 本、直接调用 provider 重嵌并比较召回。
type ChunkDumpRecord struct {
	ID        string `json:"id"`
	RelPath   string `json:"path"`
	Language  string `json:"language"`
	StartLine int    `json:"start"`
	EndLine   int    `json:"end"`
	Symbol    string `json:"symbol,omitempty"`
	Content   string `json:"content"`
}

// DumpChunkRecords 按 chunk ID 升序遍历当前 revision 的存活 chunk 并逐条
// 回调 emit，供评测工具导出语料；不进入 MCP 工具面。与查询路径共用句柄
// 与存活过滤，导出的集合就是检索能命中的集合。读取失败的记录跳过；emit
// 返回错误即中止并返回该错误。不触发同步。
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

// RouteCandidates 是融合之前两路召回的原始候选，供评测工具使用；不进入
// MCP 工具面。词法与语义候选各按本路的原始顺序返回，评测工具据此离线
// 扫描融合参数（RRF 的 k、召回深度、子句权重），一次查询嵌入的付费结果
// 可以反复复用，参数扫描不再产生嵌入费用。
type RouteCandidates struct {
	Lex   []CandidateRef
	Dense []CandidateRef
	// Reasons 是语义路的降级原因。Dense 为 nil 且 Reasons 为空表示语义路
	// 未配置，或当前 revision 没有任何向量。
	Reasons []string
}

// SearchRoutes 返回融合前的两路候选，每路深度为 depth（≤0 时取
// hybridRouteTopK），不融合、不精排、不渲染。句柄获取与存活过滤与
// retrieve 相同；同步失败时直接返回错误，不像 retrieve 那样降级服务旧
// revision。语义路的致命错误（ctx 取消或 deny 模式拒绝）原样返回。
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

	// 词法路与 retrieve 一样应用查询规划（见 queryplan.go），导出的候选与
	// 生产词法路一致；dense 路仍用原查询。结构变体零命中时回退原查询。
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
		denseIDs, reasons, denseErr := e.denseRoute(ctx, workspaceKey, handle, query, depth, nil, nil)
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

// retrieve 执行检索主体并返回渲染前的候选：校验请求、按需同步并取句柄、
// 词法召回、语义召回与融合、精排、可选的碎片过滤与产物类型分组、strict
// 检查。任何错误路径都已释放句柄，只有成功返回时句柄归调用方释放。
func (e *Engine) retrieve(ctx context.Context, req engine.SearchRequest) (retrieval, error) {
	if err := rejectProfileID(req.Workspace); err != nil {
		return retrieval{}, err
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		// 标记为请求类错误：daemon 以 HTTP 400 返回，不当作上游故障的 502。
		return retrieval{}, engine.AsInvalidRequest(errors.New("查询内容为空"))
	}
	pathPrefix, err := normalizeSearchPathPrefix(req.PathPrefix)
	if err != nil {
		return retrieval{}, err
	}
	artifact, err := parseArtifactKind(req.ArtifactKind)
	if err != nil {
		return retrieval{}, err
	}
	timings := &engine.RetrievalTimings{}
	started := time.Now()
	handle, workspaceKey, syncResult, reasons, err := e.acquireQueryHandle(ctx, req.Workspace)
	timings.SyncMs = time.Since(started).Milliseconds()
	if err != nil {
		return retrieval{}, err
	}

	lexStart := time.Now()
	lexHits, planLabel, err := e.lexicalRoute(ctx, handle, query, pathPrefix)
	timings.LexicalMs = time.Since(lexStart).Milliseconds()
	if err != nil {
		e.releaseHandle(handle)
		return retrieval{}, err
	}
	ordered, mode, coverage, fuseReasons, err := e.fuseRoutes(ctx, workspaceKey, handle, query, lexHits, timings, pathPrefix)
	if err != nil {
		e.releaseHandle(handle)
		return retrieval{}, err
	}
	reasons = append(reasons, fuseReasons...)
	if pathPrefix != "" {
		// path_prefix 已在词法与语义两路召回时下推过滤，正常情况下这里
		// 一个候选也不会被去掉。保留这一步是为了在词法锚或融合实现改动
		// 后仍不会有前缀之外的候选进入结果。
		ordered = filterRankedByPathPrefix(handle, ordered, pathPrefix)
		if planLabel != "" {
			planLabel += " "
		}
		planLabel += "path_prefix=" + pathPrefix
	}

	// 精排：配置了 rerank provider 即默认启用（12 个仓库的评测中，精排相对
	// 只做 RRF 融合把前五召回率提高 12.82 个百分点）。只重排已召回候选的
	// 头部，精排失败时候选原序保留、追加降级原因，不丢任何候选。
	rerankApplied := false
	rerankSent := 0
	if e.rerankClient != nil && len(ordered) > 0 {
		rerankStart := time.Now()
		reordered, applied, sent, rerankReason, rerankErr := e.rerankOrder(ctx, handle, query, ordered)
		timings.RerankMs = time.Since(rerankStart).Milliseconds()
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
	} else if e.semanticEnabled() && !e.rerankCfg.Enabled && e.rerankCfg.ProviderType != rerank.ProviderOff {
		// 配置了 embedding provider 却没有可用的 rerank 配置（缺 key，而不是
		// 显式设为 off）：结果按 RRF 融合序返回，但追加 rerank-unconfigured
		// 原因进 [DEGRADED] 横幅，让用户知道精排没有生效；
		// OPENACE_QUALITY_STRICT=on 时 checkQualityStrict 会把它升级为报错。
		// 显式 off 表示用户主动放弃精排，不提示。未配置 embedding 的纯词法
		// 路径不进此分支。
		reasons = append(reasons, "rerank-unconfigured(quality-first default; set "+rerank.EnvAPIKey+" or VOYAGE_API_KEY, or "+rerank.EnvProvider+"=off to opt out)")
	}
	// 碎片过滤是实验性开关：只在程序化 Options 开启 fragmentGate 时去掉行
	// 窗口切分产生的纯日期、纯符号碎片块；生产环境没有对应 env，默认关闭，
	// 行为不变。放在精排之后、渲染之前，不改变召回与精排的候选池，关掉即
	// 恢复原行为。
	ordered, _, fragmentErr := e.filterFragmentNoise(handle, ordered)
	if fragmentErr != nil {
		e.releaseHandle(handle)
		return retrieval{}, fragmentErr
	}
	// 调用方明示 artifact_kind 时按产物类型分组：放在精排与碎片过滤之后、
	// 渲染之前，只改输出次序，不改召回与精排的候选池，不丢弃候选。
	if artifact != artifactAny {
		ordered = groupByArtifactKind(handle, ordered, artifact)
		if planLabel != "" {
			planLabel += " "
		}
		planLabel += "artifact_kind=" + artifact
	}

	out := retrieval{
		handle: handle, ordered: ordered, mode: mode,
		reasons: reasons, coverage: coverage, syncResult: syncResult,
		rerankSent: rerankSent, rerankApplied: rerankApplied,
		queryEmbedFailed: hasQueryEmbedFailure(reasons),
		queryPlan:        planLabel,
		timings:          timings,
		started:          started,
	}
	if err := e.checkQualityStrict(reasons, rerankApplied, len(ordered) > 0); err != nil {
		e.releaseHandle(handle)
		return retrieval{}, err
	}
	return out, nil
}

// acquireQueryHandle 完成查询前置：经 syncWorkspaceForQuery 按需同步（可能
// 受 OPENACE_QUERY_BUILD_WAIT 限制等待时长），解析工作区键并取得当前
// revision 的句柄。返回的 reasons 是本步产生的降级原因。
//   - 同步成功：正常返回句柄与同步结果。
//   - 同步失败且没有任何可用 revision：返回同步错误。
//   - 同步失败但有可用 revision：OPENACE_RETRIEVAL_DEGRADE=allow（默认）时
//     用旧索引服务并追加原因——一般失败记 stale-index，等待在建构建超时
//     记 index-building，首次触达后台刷新记 index-refreshing；deny 时释放
//     句柄并返回带恢复提示的错误。
//   - ctx 已取消：原样返回 ctx 错误。
//
// 错误路径不持有句柄。
func (e *Engine) acquireQueryHandle(ctx context.Context, ref engine.WorkspaceRef) (*revisionHandle, string, engine.Result, []string, error) {
	// 检索前先确保索引就绪，查询本身触发按需同步。
	syncResult, syncErr := e.syncWorkspaceForQuery(ctx, ref)
	if syncErr != nil && ctx.Err() != nil {
		return nil, "", engine.Result{}, nil, ctx.Err()
	}
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return nil, "", engine.Result{}, nil, err
	}
	handle, handleErr := e.acquireHandle(workspaceKey)
	var reasons []string
	if syncErr != nil {
		if handleErr != nil {
			return nil, "", engine.Result{}, nil, syncErr
		}
		reason, label := "stale-index", "index refresh failed"
		if errors.Is(syncErr, errQueryBuildWait) {
			reason, label = "index-building", "index still building"
		}
		if errors.Is(syncErr, errFirstTouchRefresh) {
			// daemon 启动后该工作区的首次查询：磁盘上的 revision 立即服务，
			// 真正的同步已在后台进行。这不是失败，只是告知索引可能不新鲜；
			// deny 与 strict 配置在 syncWorkspaceForQuery 入口就不走这条路。
			reason, label = "index-refreshing", "first-touch refresh in background"
		}
		if e.retrievalDegrade == DegradeDeny {
			e.releaseHandle(handle)
			return nil, "", engine.Result{}, nil, degradeDeniedError(label, syncErr, EnvRetrievalDegrade)
		}
		reasons = append(reasons, reason)
		syncResult = engine.Result{Engine: EngineID, FileCount: handle.manifest.Counts.Files}
	} else if handleErr != nil {
		return nil, "", engine.Result{}, nil, handleErr
	}
	return handle, workspaceKey, syncResult, reasons, nil
}

// lexicalRoute 执行词法召回，返回存活命中、查询规划标签与错误。不负责
// 句柄释放。
//   - 召回深度：纯词法模式 defaultTopK，配置了语义路时 hybridRouteTopK。
//   - 查询规划（见 queryplan.go）：含结构 token 的非 CJK 自然语言查询，
//     词法路只用结构 token 检索；结构查询零命中时用原查询再检索一次，
//     标签追加 " fallback=original"。未触发时行为与没有规划时一致。
//   - pathPrefix 在 Bleve 查询中直接过滤，不在召回后再截。
//   - 存活过滤：Bleve 会命中旧 segment 中已被新版本覆盖或文件已删除的
//     chunk，进入融合前按句柄的存活集过滤掉，已删除内容不进结果。
func (e *Engine) lexicalRoute(ctx context.Context, handle *revisionHandle, query string, pathPrefix string) ([]lexical.Hit, string, error) {
	lexTopK := defaultTopK
	if e.semanticEnabled() {
		lexTopK = hybridRouteTopK
	}
	plan := planLexicalQuery(query)
	planLabel := ""
	lexQuery := query
	if plan.Triggered {
		lexQuery = plan.LexicalQuery
		planLabel = plan.LexicalQuery
	}
	lexHits, err := handle.lex.SearchWeightedPrefix(ctx, lexQuery, lexTopK, e.lexWeights, pathPrefix)
	if err != nil {
		return nil, "", fmt.Errorf("词法检索: %w", err)
	}
	if plan.Triggered && len(lexHits) == 0 {
		planLabel += " fallback=original"
		lexHits, err = handle.lex.SearchWeightedPrefix(ctx, query, lexTopK, e.lexWeights, pathPrefix)
		if err != nil {
			return nil, "", fmt.Errorf("词法检索(回退): %w", err)
		}
	}
	return filterLiveHits(handle, lexHits), planLabel, nil
}

// fuseRoutes 把词法命中与语义召回融合成最终候选序，返回（候选序、模式、
// 覆盖率、降级原因、错误）。不负责句柄释放。
//   - 未配置语义路：直接返回词法序，score 保留真实 BM25 分数，模式为
//     lexical，覆盖率为空；渲染结果与纯词法版本完全一致。
//   - 配置了语义路且 dense 召回执行成功（返回的候选切片非 nil，哪怕为
//     空）：按 fusionParams 做加权 RRF 融合，再用 anchorWithinWindow 把词法
//     首位放进精排窗口（词法锚，理由见函数体内注释），模式为 hybrid。
//   - 配置了语义路但 dense 未执行（向量不可用、覆盖为空、查询嵌入失败）：
//     按词法序输出，模式仍为 lexical，降级原因由 denseRoute 给出。
//   - 语义覆盖不完整时追加 semantic-coverage-partial 原因。
//   - denseRoute 的致命错误（ctx 取消或 deny 拒绝）原样返回。
func (e *Engine) fuseRoutes(ctx context.Context, workspaceKey string, handle *revisionHandle, query string, lexHits []lexical.Hit, timings *engine.RetrievalTimings, pathPrefix string) ([]rankedHit, string, string, []string, error) {
	if !e.semanticEnabled() {
		ordered := make([]rankedHit, 0, len(lexHits))
		for _, hit := range lexHits {
			ordered = append(ordered, rankedHit{id: hit.ID, score: hit.Score, source: fusion.SourceLexical})
		}
		return ordered, "lexical", "", nil, nil
	}
	denseIDs, reasons, denseErr := e.denseRoute(ctx, workspaceKey, handle, query, hybridRouteTopK, timings, chunkPrefixPredicate(handle, pathPrefix))
	if denseErr != nil {
		return nil, "", "", nil, denseErr
	}
	lexIDs := make([]string, 0, len(lexHits))
	for _, hit := range lexHits {
		lexIDs = append(lexIDs, hit.ID)
	}
	mode := "lexical"
	var ordered []rankedHit
	if denseIDs != nil {
		mode = "hybrid"
		fuseStart := time.Now()
		fused := fusion.RRFWeighted(lexIDs, denseIDs, e.fusionParams())
		defer func() {
			if timings != nil {
				timings.FuseMs = time.Since(fuseStart).Milliseconds()
			}
		}()
		ids := make([]string, 0, len(fused))
		sources := make(map[string]string, len(fused))
		for _, f := range fused {
			ids = append(ids, f.ID)
			sources[f.ID] = f.Source
		}
		// 词法锚：保证词法路的首位候选进入精排窗口（前 rerankHeadLimit
		// 个）。dense 路能返回候选但语义失真时（模型弱或与语料不匹配），
		// 加权融合（默认 dense 权重 0.85）会把词法唯一的强命中压出精排窗
		// 口，精排的送审集里就没有它。四个语料共 12,497 条查询的离线复算
		// 显示这一步对 R@5/R@10 逐位无变化，没有质量代价；把窗口缩到 5
		// 的版本在 CoSQA 语料上 R@5 下降 3.6 个百分点，因此窗口取精排头部
		// 的大小。
		if len(lexIDs) > 0 {
			ids = anchorWithinWindow(ids, lexIDs[0], rerankHeadLimit)
		}
		ordered = rankByPosition(ids)
		for i := range ordered {
			ordered[i].source = sources[ordered[i].id]
		}
	} else {
		ordered = rankByPosition(lexIDs)
		for i := range ordered {
			ordered[i].source = fusion.SourceLexical
		}
	}
	coverage := coveragePercent(handle.manifest)
	if !handle.manifest.SemanticComplete() {
		reasons = append(reasons, "semantic-coverage-partial")
	}
	return ordered, mode, coverage, reasons, nil
}

// checkQualityStrict 实现 OPENACE_QUALITY_STRICT=on 的检查：语义链路有任何
// 缺口就返回错误，而不是带降级横幅放行。缺口包括每一条降级原因（覆盖
// 不完整、查询嵌入失败、精排跳过、索引陈旧、向量不可用等），以及配置了
// rerank、有候选却未生效且没有对应 rerank-skipped 原因的情况（记为
// rerank-not-applied）。错误文本列出全部缺口 token 与 env 名，调用方据此
// 定位。未开启 strict 时不做任何事。
func (e *Engine) checkQualityStrict(reasons []string, rerankApplied bool, hasCandidates bool) error {
	if !e.qualityStrict {
		return nil
	}
	violations := append([]string(nil), reasons...)
	if e.rerankClient != nil && !rerankApplied && hasCandidates && !containsRerankReason(reasons) {
		violations = append(violations, "rerank-not-applied")
	}
	if len(violations) > 0 {
		return fmt.Errorf("%s=on: 语义质量缺口 [%s],拒绝返回不完整结果(改用 %s=off 可降级放行)",
			EnvQualityStrict, strings.Join(violations, ","), EnvQualityStrict)
	}
	return nil
}

// hasQueryEmbedFailure 判定降级原因中是否含查询嵌入失败，结果进入
// Result.QueryEmbedFailed。
func hasQueryEmbedFailure(reasons []string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, "query-embedding-failed") {
			return true
		}
	}
	return false
}

// containsRerankReason 判定 reasons 是否已含 rerank-skipped 原因；已含时
// checkQualityStrict 不再追加 rerank-not-applied，同一缺口只报一次。
func containsRerankReason(reasons []string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, "rerank-skipped") {
			return true
		}
	}
	return false
}

// 词法路不再对 locale 文件（如 locales/zh-CN.js）做类别降权。此前在 retrieve
// 与 SearchRoutes 的词法命中过滤之后、融合之前，有过把这类文件的 BM25 分数
// 乘 0.35 再重排的步骤：评测复核显示它在 hybrid 模式下对所有防护场景都没有
// 收益（防护由词法层的 CJK 守卫子句 latinGuardTokens 与 dense 融合承担），
// 却把"查询是菜单短标签、locale 文件是唯一正确答案"的查询压出前十；去掉
// 后开发集上该类查询的最终 R@5 从 0.35 升到 0.50。回归测试
// TestLocaleGoldNotDemoted（locale_gold_test.go）固定当前行为。

// normalizeSearchPathPrefix 校验并归一化 path_prefix 参数：统一为正斜杠、
// 去掉首尾斜杠、pathpkg.Clean。它只用于与索引内相对路径比较，不拼接磁
// 盘路径，但仍拒绝绝对路径与含 .. 的逃逸形态，并按请求类错误返回：调用
// 方对它的理解应与 workspace 根目录边界一致。空值返回空串表示不过滤。
func normalizeSearchPathPrefix(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	prefix := strings.Trim(strings.ReplaceAll(raw, "\\", "/"), "/")
	if prefix == "" {
		return "", nil
	}
	absolute := strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") || (len(raw) >= 2 && raw[1] == ':')
	if absolute || prefix == ".." || strings.HasPrefix(prefix, "../") || strings.Contains(prefix, "/../") {
		return "", engine.AsInvalidRequest(fmt.Errorf("invalid path_prefix %q: expected an indexed relative path prefix", raw))
	}
	clean := pathpkg.Clean(prefix)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", engine.AsInvalidRequest(fmt.Errorf("invalid path_prefix %q: expected an indexed relative path prefix", raw))
	}
	return clean, nil
}

// chunkPrefixPredicate 构造 dense 路的路径前缀谓词：只放行路径等于 prefix
// 或位于 prefix/ 之下的 chunk。prefix 为空返回 nil，表示不过滤。谓词在
// 向量选择阶段生效，目标子树不会被前缀之外的候选挤出受限深度的 top-k。
// 它会被多个评分 worker 并发调用，只读不可变的 handle.chunks，并发安全。
func chunkPrefixPredicate(handle *revisionHandle, prefix string) func(id string) bool {
	if prefix == "" {
		return nil
	}
	return func(id string) bool {
		meta, ok := handle.chunks[id]
		return ok && (meta.RelPath == prefix || strings.HasPrefix(meta.RelPath, prefix+"/"))
	}
}

func filterRankedByPathPrefix(handle *revisionHandle, ordered []rankedHit, prefix string) []rankedHit {
	filtered := ordered[:0]
	for _, hit := range ordered {
		meta, ok := handle.chunks[hit.id]
		if ok && (meta.RelPath == prefix || strings.HasPrefix(meta.RelPath, prefix+"/")) {
			filtered = append(filtered, hit)
		}
	}
	return filtered
}

// filterLiveHits 去掉词法命中中不在句柄存活集里的 chunk（旧 segment 中已
// 被新版本覆盖或文件已删除的记录）。语义路在 denseRoute 内做同一过滤，
// 两路都在进入融合之前完成。
func filterLiveHits(handle *revisionHandle, hits []lexical.Hit) []lexical.Hit {
	live := hits[:0]
	for _, hit := range hits {
		if _, ok := handle.chunks[hit.ID]; ok {
			live = append(live, hit)
		}
	}
	return live
}

// denseRoute 执行语义召回，返回最多 depth 个 dense 候选 ID（nil 表示语义
// 路未执行）、降级原因与致命错误。
//   - 向量行数超过用户配置的内存预算：不标记修复（重建也超预算，没有
//     用）；OPENACE_RETRIEVAL_DEGRADE=deny 时报错，allow 时记
//     vector-envelope-exceeded 并只用词法结果。
//   - 向量文件损坏或缺失：登记该工作区下次同步强制重建向量，deny 报错，
//     allow 记 vector-data-unavailable。
//   - revision 一个向量也没有（例如 provider 长期故障后发布的零覆盖
//     revision）：返回 nil 且不记原因，覆盖缺口由 fuseRoutes 按 manifest
//     的覆盖率如实上报。
//   - 查询嵌入失败：ctx 已取消则返回 ctx 错误；deny 报错；allow 记
//     query-embedding-failed(失败类别)。
//   - 正常路径：每个 segment 各做一次精确（非近似）检索，合并后按分数
//     降序、ID 升序排序，同一查询多次执行结果逐位相同；再按存活集过滤
//     掉已删除或已被覆盖的 chunk，取前 depth 个。
//
// 只有 ctx 取消、deny 拒绝与向量检索本身出错才返回 error。
func (e *Engine) denseRoute(ctx context.Context, workspaceKey string, handle *revisionHandle, query string, depth int, timings *engine.RetrievalTimings, allow func(id string) bool) ([]string, []string, error) {
	ixs, loadErr := handle.vectorIndexes(e.embedCfg.Dimension)
	if loadErr != nil {
		if errors.Is(loadErr, vector.ErrEnvelopeExceeded) {
			// 超出内存预算是配置与规模的关系，重建向量文件改变不了行数，
			// 因此不标记修复。
			if e.retrievalDegrade == DegradeDeny {
				return nil, nil, degradeDeniedError("semantic path unavailable", loadErr, EnvRetrievalDegrade)
			}
			return nil, []string{"vector-envelope-exceeded"}, nil
		}
		// 向量文件损坏或缺失：登记该工作区，下次同步强制重建向量；本次
		// 只用词法结果并如实上报。
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
		// 一个向量也没有（如 provider 长期故障后发布的零覆盖 revision）：
		// 语义路没有候选可召回，覆盖缺口由 fuseRoutes 按 manifest 如实上报，
		// 这里不重复记原因。
		return nil, nil, nil
	}
	embedStart := time.Now()
	queryVector, embedErr := e.embedClient.EmbedQuery(ctx, query)
	if timings != nil {
		timings.QueryEmbedMs = time.Since(embedStart).Milliseconds()
	}
	if embedErr != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if e.retrievalDegrade == DegradeDeny {
			return nil, nil, degradeDeniedError("query embedding failed", embedErr, EnvRetrievalDegrade)
		}
		return nil, []string{"query-embedding-failed(" + failureClass(embedErr) + ")"}, nil
	}
	vectorStart := time.Now()
	var merged []vector.Hit
	for _, ix := range ixs {
		queryCopy := make([]float32, len(queryVector))
		copy(queryCopy, queryVector)
		segmentHits, searchErr := ix.SearchFiltered(ctx, queryCopy, depth, allow)
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
	if timings != nil {
		timings.VectorMs = time.Since(vectorStart).Milliseconds()
	}
	return ids, nil, nil
}

// rerankOrder 把 ordered 的头部送给 rerank provider 精排，返回（新序、是否
// 生效、实际送审数、降级原因、错误）。任何路径下候选都不重复、不丢失。
//   - 送审集是前 rerankHeadLimit 个候选，rerank 客户端再按
//     OPENACE_RERANK_MAX_TOKENS 估算截断，实际送审数为 sent；未送审的候选
//     按原序跟在精排结果之后。
//   - 头部里不在存活集的候选（防御性路径，正常不发生）不送审、原位保留；
//     docs 与送审候选逐项对应，避免错位造成条目重复或丢失。
//   - provider 调用失败：ctx 已取消返回 ctx 错误；
//     OPENACE_RERANK_DEGRADE=deny 返回带恢复提示的错误；allow 返回
//     rerank-skipped(失败类别)，调用方保留融合序。sent 为 0（token 预算
//     连第一条都装不下）记 rerank-skipped(token-budget)。
//   - 已送审但 provider 未返回的候选按原序补回并记 rerank-partial-response
//     （见 rerankAssembleOrder）。
//   - 生效时每条候选带上 reranked/rerankScore/source。
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
		// rerank 客户端已把条数与送审集不一致的响应整体按 malformed 拒绝，
		// 正常路径 missing 恒为 0。此分支只在那道校验被绕过时生效：候选
		// 已按原序补回，但精排只覆盖了部分送审集，必须上报，不得当成完
		// 整精排返回。
		reason = "rerank-partial-response"
	}
	// 逐条标记：ids 的前 len(hits) 项是 provider 实际打分并返回的候选，
	// 其后是按原序补回或跟随的未精排项；融合来源从原候选带过来。
	sources := make(map[string]string, len(ordered))
	for _, hit := range ordered {
		sources[hit.id] = hit.source
	}
	final := rankByPosition(ids)
	for i := range final {
		final[i].source = sources[final[i].id]
		if i < len(hits) {
			final[i].reranked = true
			final[i].rerankScore = hits[i].Score
		}
	}
	return final, true, sent, reason, nil
}

// rerankAssembleOrder 组装精排后的最终 ID 序，依次为：provider 返回的重排
// 命中；已送审但 provider 未返回的条目，按原序补回（正常路径为空，客户端
// 已把条数不足的响应整体拒绝）；因 token 预算未送审的头部候选，按原序；
// 不在存活集而未送审的头部候选，按原序；头部之外的尾部，按原序。第二个
// 返回值是补回的条数。任何响应形状下，已召回的候选都不重复、不丢失：
// 精排失败或异常只影响顺序，调用方看到的候选集不变。
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

// rerankDocText 构造精排送审文本：首行 path:start-end，有符号时追加符号
// 名，换行后接 chunk 内容。头部帮助 rerank 模型理解代码所在位置；精排
// 文本逐查询构造、不缓存，路径进入文本不带来任何缓存失效代价（对比
// embedDocText 里对行号的取舍）。
func rerankDocText(record chunkRecord) string {
	header := fmt.Sprintf("%s:%d-%d", record.RelPath, record.StartLine, record.EndLine)
	if record.Symbol != "" {
		header += " " + record.Symbol
	}
	return header + "\n" + record.Content
}

// anchorWithinWindow 保证 anchor 位于 ids 的前 window 个之内：anchor 已在
// 窗口内、不在 ids 中、或 ids 不超过窗口时原样返回；否则把 anchor 移到
// 窗口的最后一位（下标 window-1），原窗口末位及其后、anchor 原位置之前
// 的候选各后移一位，anchor 原位置之后的候选不动。只移动位置，不改分数。
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

// 产物类型（artifact kind）：只按路径的固定规则把候选分为代码、测试、文档
// 三类，供调用方经 SearchRequest.ArtifactKind 明示意图时分组输出。引擎不
// 从查询推断意图，分类也不参与打分。
const (
	artifactAny   = "any"
	artifactCode  = "code"
	artifactTests = "tests"
	artifactDocs  = "docs"
)

// parseArtifactKind 校验请求的产物类型：空或 any 表示不分组；code、tests、
// docs 原样接受；其他值按请求类错误拒绝（daemon 返回 400，MCP 返回工具
// 错误），不悄悄按 any 处理。
func parseArtifactKind(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", artifactAny:
		return artifactAny, nil
	case artifactCode, artifactTests, artifactDocs:
		return strings.TrimSpace(raw), nil
	}
	return "", engine.AsInvalidRequest(fmt.Errorf("invalid artifact_kind %q; use any, code, tests or docs", raw))
}

// artifactKind 按相对路径判定产物类型，规则按顺序匹配，先命中先返回：
//   - tests：路径中有 test、tests、spec、__tests__、testdata 目录段；或文件名
//     含 _test.、.test.、.spec.，以 test_ 开头，去扩展名后以 test 或 tests
//     结尾（Java、C#、PHP 的 FooTest 命名）。
//   - docs：路径中有 doc、docs、documentation 目录段；或扩展名为 .md、
//     .mdx、.rst、.adoc、.txt；或文件名以 changelog、readme 开头。
//   - 其余为 code。
//
// tests 先于 docs 判定，tests/ 目录下的 README 归测试。已知边界：框架自身
// 的 testing/ 目录不算测试，代码目录里的 .md 算文档。误分只影响
// artifact_kind 的分组次序，候选不会被隐藏。
func artifactKind(relPath string) string {
	lower := strings.ToLower(relPath)
	segments := strings.Split(lower, "/")
	base := segments[len(segments)-1]
	for _, dir := range segments[:len(segments)-1] {
		switch dir {
		case "test", "tests", "spec", "__tests__", "testdata":
			return artifactTests
		}
	}
	stem := base
	if dot := strings.LastIndex(base, "."); dot > 0 {
		stem = base[:dot]
	}
	if strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(stem, "test") || strings.HasSuffix(stem, "tests") {
		return artifactTests
	}
	for _, dir := range segments[:len(segments)-1] {
		switch dir {
		case "doc", "docs", "documentation":
			return artifactDocs
		}
	}
	for _, ext := range []string{".md", ".mdx", ".rst", ".adoc", ".txt"} {
		if strings.HasSuffix(base, ext) {
			return artifactDocs
		}
	}
	if strings.HasPrefix(base, "changelog") || strings.HasPrefix(base, "readme") {
		return artifactDocs
	}
	return artifactCode
}

// groupByArtifactKind 把属于请求类型的候选按原相对顺序排到最前，其余候选
// 按原序跟随；候选集合与数量不变。kind 为 any 或空时原样返回。返回的候选
// 按新位置重新赋 1/(位置+1) 的序分：渲染按序分排序，若保留旧分数，分组
// 结果会在渲染时被排回原顺序。reranked、rerankScore、source 逐条保留。
func groupByArtifactKind(handle *revisionHandle, ordered []rankedHit, kind string) []rankedHit {
	if kind == artifactAny || kind == "" {
		return ordered
	}
	head := make([]rankedHit, 0, len(ordered))
	tail := make([]rankedHit, 0, len(ordered))
	for _, hit := range ordered {
		meta, ok := handle.chunks[hit.id]
		if ok && artifactKind(meta.RelPath) == kind {
			head = append(head, hit)
		} else {
			tail = append(tail, hit)
		}
	}
	grouped := append(head, tail...)
	for i := range grouped {
		grouped[i].score = 1.0 / float64(i+1)
	}
	return grouped
}

// rankByPosition 把最终 ID 序转成 rankedHit，score 取 1/(位置+1)。渲染时同
// 文件块合并后按 score 排序，合成分数严格随位置递减，跨块顺序与最终排名
// 一致且每次相同。
func rankByPosition(ids []string) []rankedHit {
	ordered := make([]rankedHit, 0, len(ids))
	for i, id := range ids {
		ordered = append(ordered, rankedHit{id: id, score: 1.0 / float64(i+1)})
	}
	return ordered
}

// coveragePercent 计算语义覆盖率文本：有向量的 chunk 数 / 存活 chunk 数，
// 百分比向下取整。没有任何 chunk 的仓库直接按 100%：一是避免除以零，二是
// 与 Manifest.SemanticComplete 的口径一致（VectorCount >= Counts.Chunks，空
// 仓库视为语义完整，不触发 semantic-coverage-partial 降级）。
func coveragePercent(manifest *index.Manifest) string {
	if manifest.Counts.Chunks == 0 {
		return "100%"
	}
	return fmt.Sprintf("%d%%", manifest.VectorCount*100/manifest.Counts.Chunks)
}

// degradedBanner 构造正文首行的降级横幅：
// "[DEGRADED] <原因,逗号分隔>; mode=<检索模式>[; semantic_coverage=<覆盖率>]"，
// 后跟空行。格式固定，调用方与测试依赖它。
func degradedBanner(reason string, mode string, coverage string) string {
	banner := "[DEGRADED] " + reason + "; mode=" + mode
	if coverage != "" {
		banner += "; semantic_coverage=" + coverage
	}
	return banner + "\n\n"
}

// degradeDeniedError 构造 deny 模式下返回的错误：保留原始错误（其中已含
// 401、429、连接被拒等分类诊断），并写明把 envName 改为 allow 即可接受
// 降级结果。用户看到错误就知道原因和下一步，不必先去翻配置。
func degradeDeniedError(stage string, cause error, envName string) error {
	return fmt.Errorf("%s: %v (degrade mode is deny; set %s=allow to accept degraded results)", stage, cause, envName)
}

// failureClass 提取 provider 调用失败的类别（reliability.CallError.Class，
// 如 rate-limit、auth、quota、transient、permanent、backoff），写进
// degraded_reason 的括号里；不是 CallError 的错误记为 error。
func failureClass(err error) string {
	callErr := &reliability.CallError{}
	if errors.As(err, &callErr) {
		return string(callErr.Class)
	}
	return "error"
}

// handleKey 是句柄表 e.handles 的键：workspaceKey 与 revision 用 NUL 连接。
// 只用 revision 做键时，两个工作区的同名 revision 会互相覆盖表项，被覆盖
// 的句柄再也关不掉。
func handleKey(workspaceKey string, revision string) string {
	return workspaceKey + "\x00" + revision
}

// acquireHandle 打开（或复用已缓存的）第一个真正可用的 revision 句柄并增
// 加引用。可用 = manifest 校验通过且 Bleve 索引与 chunk 元数据都能打开：
// manifest 完好但 Bleve 段损坏的 revision 视为损坏，沿 previous 链继续尝
// 试，否则之后每次检索都失败而状态仍报 ready。链遍历带环检测与深度上限
// （index.MaxRevisionChain），被跳过的 revision 经 noteSkippedRevisions 记
// 入状态；全部失败时返回最后一个错误，一个也没有时返回
// index.ErrNoUsableRevision。
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

// openOrReuseHandle 返回指定 revision 的句柄：句柄表里已有（包括已标记
// 退役但尚未关闭的）就取消退役标记、加引用并复用，revision 不可变，旧
// 句柄的数据仍然有效；没有则在锁外打开 Bleve 索引与 chunk 元数据，再登
// 记进表。引擎已关闭时返回错误。两个查询同时打开同一 revision 时，后登
// 记者复用先登记者的句柄并关闭自己刚打开的索引。
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
		engine: e, workspaceKey: workspaceKey, manifest: manifest, lex: lex, chunks: chunks,
		segmentDirs: segmentDirs, refs: 1,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		go lex.Close()
		return nil, errors.New("local-hybrid 引擎已关闭")
	}
	if existing, ok := e.handles[key]; ok {
		// 另一个调用在本次打开期间已登记同一 revision：复用它的句柄，
		// 本次打开的索引在后台关闭。
		existing.retired = false
		existing.refs++
		go lex.Close()
		return existing, nil
	}
	e.handles[key] = handle
	return handle, nil
}

// releaseHandle 归还一个引用。句柄已被 retireHandles 标记退役且引用归零
// 时立即关闭：Bleve 索引、chunks.jsonl 文件、向量段引用一并释放。删除表
// 项前核对表里的确是这个句柄对象：同一个键先后对应过不同句柄（旧的已
// 关闭删除、新的以同键登记）时，直接按键删除会把新句柄挤出表，之后没有
// 任何路径再关闭它。
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
		handle.releaseVectorIndexes()
	}
}

// retireHandles 在新 revision 发布后把该工作区除 activeRevision 之外的句柄
// 全部标记退役：没有引用的立即关闭，正在被查询使用的等 releaseHandle 归
// 零后关闭。previous revision 的磁盘数据仍由 GC 保留，需要回退时重新打开
// 即可；不保留其句柄是因为闲置的 previous 句柄会让 compaction 前后两套
// 完整向量同时常驻内存。第三个参数保留形参位，当前未使用。
func (e *Engine) retireHandles(workspaceKey string, activeRevision string, _ string) {
	keep := map[string]bool{
		handleKey(workspaceKey, activeRevision): true,
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
		handle.releaseVectorIndexes()
	}
}

// renderBlock 是渲染阶段的合并单元：一个候选 chunk 的完整记录加上它在
// rankedHit 里的分数与标记。同文件相邻块合并时这些字段按 mergeBlocks 的
// 规则聚合。
type renderBlock struct {
	record      chunkRecord
	score       float64
	reranked    bool
	rerankScore float64
	source      string
}

// mergedBlockMaxLines 是同文件相邻块合并后单块的行数上限，取两个代码行
// 窗口（默认 60 行，即 120 行）。不设上限时，行窗口切分的文档（40 行窗口、
// 10 行重叠）相邻窗口天然满足合并条件，两路各 60 的召回深度把同一文档的
// 多个相邻窗口都送进候选后，它们会被合成整篇文档（外部工作区实测出现过
// 434 行的单块），并以其中最高分占据高名次。取 120 行：5 个连续文档窗口
// 合成 1-100 与 91-160 两块；两个相邻代码窗口（60 行 + 去重叠 50 行 = 110
// 行）仍能合并，若取 80 行则代码窗口对合不了，重叠的 10 行会重复展示。
var mergedBlockMaxLines = 2 * chunk.DefaultProfile().WindowLines

// rerankBoundaryMarker 是正文中精排头部与未精排尾部之间的边界行。文本
// 固定，调用方可以依赖；不以 "## " 开头，按头行解析正文的调用方可以直接
// 跳过它。只在本次检索执行了精排、且正文同时含精排块与未精排块时出现。
const rerankBoundaryMarker = "-- results below were not reranked (fused order) --"

// 输出详略模式。full：前 FullResults 个候选块带正文，其余只给头行；paths：
// 全部候选只给 `## path:start-end symbol` 头行，内容由调用方按需 Read。
const (
	detailFull  = "full"
	detailPaths = "paths"
)

// parseDetail 校验 detail 参数：空值按 full；full、paths 原样接受；其他值
// 按请求类错误拒绝（daemon 返回 400），不悄悄按默认处理。
func parseDetail(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", detailFull:
		return detailFull, nil
	case detailPaths:
		return detailPaths, nil
	}
	return "", engine.AsInvalidRequest(fmt.Errorf("invalid detail %q; use full or paths", raw))
}

// renderedResult 是渲染产物：正文、结构化 hits 清单（每个合并后的块一条）
// 与展示统计。候选有多少、展示了多少、几个带正文，调用方从统计字段就能
// 读到，不必解析正文。
type renderedResult struct {
	text    string
	hits    []engine.Hit
	display engine.DisplayStats
}

// renderHits 以 detail=full 渲染并只返回正文，供测试对照固定文本使用。
func renderHits(handle *revisionHandle, hits []rankedHit, fullResults int) (string, error) {
	rendered, err := renderHitsWithInventory(handle, hits, fullResults)
	return rendered.text, err
}

// renderHitsWithInventory 以 detail=full 渲染并返回完整产物，供测试使用。
func renderHitsWithInventory(handle *revisionHandle, hits []rankedHit, fullResults int) (renderedResult, error) {
	return renderHitsDetail(handle, hits, fullResults, detailFull)
}

// pathsOnlyMarker 是正文中"带正文的头部块"与"只有头行的其余候选"之间的
// 分隔行。文本固定；不以 "## " 开头，按头行解析正文的调用方可以跳过它。
// 只在 detail=full 且候选块数超过 fullResults 时出现。
const pathsOnlyMarker = "-- remaining results listed as paths only; Read a file to see its content --"

// renderHitsDetail 把排序后的候选渲染为正文、hits 清单与展示统计。输出格
// 式固定并由 golden 测试锁定：宿主 AI 按 `## path:start-end symbol` 头行加
// 代码围栏解析结果，格式漂移会破坏它的解析。
//   - 没有候选或全部候选都不在存活集：返回 noHitsText。
//   - 按需 pread 每个候选的内容，同文件重叠或相邻的块合并（上限见
//     mergedBlockMaxLines），合并后按 score 降序。
//   - fullResults 决定几个块带正文：0 表示未指定，取
//     engine.DefaultFullResults；负值表示一个都不带；paths 模式恒为 0。
//     前 fullResults 个块输出完整围栏，其余只输出头行，两段之间插
//     pathsOnlyMarker（paths 模式不插）。
//   - 全部候选都出现在正文与 hits[] 中，没有按字节预算丢弃的块：输出体量
//     由使用者配置的 OPENACE_FULL_RESULTS 决定，不由隐含的字节上限决定；
//     要更短的回复就调小它，要更少的往返就调大。
//   - 执行过精排时在首个未精排块之前插 rerankBoundaryMarker（条件见函数
//     体内注释）。
func renderHitsDetail(handle *revisionHandle, hits []rankedHit, fullResults int, detail string) (renderedResult, error) {
	if len(hits) == 0 {
		return renderedResult{text: noHitsText}, nil
	}
	blocks := make([]renderBlock, 0, len(hits))
	for _, hit := range hits {
		if _, ok := handle.chunks[hit.id]; !ok {
			continue
		}
		record, err := handle.record(hit.id)
		if err != nil {
			return renderedResult{}, err
		}
		blocks = append(blocks, renderBlock{record: record, score: hit.score, reranked: hit.reranked, rerankScore: hit.rerankScore, source: hit.source})
	}
	if len(blocks) == 0 {
		return renderedResult{text: noHitsText}, nil
	}
	merged := mergeBlocks(blocks)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].score > merged[j].score })

	// fullResults：0 表示未指定，取默认值；负值表示一个都不带正文；paths
	// 模式无论传什么都是 0 个。
	switch {
	case detail == detailPaths:
		fullResults = 0
	case fullResults == 0:
		fullResults = engine.DefaultFullResults
	case fullResults < 0:
		fullResults = 0
	}
	inventory := make([]engine.Hit, len(merged))
	anyReranked := false
	// 精排边界标记只在"所有精排块都排在未精排块之前"时才有意义。
	// artifact_kind 分组会把未精排的目标类型候选提到精排块之前，这时不插
	// 标记（插了会误导读者），每个 hit 的 reranked 字段仍如实携带。
	cleanBoundary := true
	seenUnreranked := false
	for i, block := range merged {
		inventory[i] = engine.Hit{
			Path: block.record.RelPath, StartLine: block.record.StartLine,
			EndLine: block.record.EndLine, Symbol: block.record.Symbol, Rank: i + 1, Shown: true,
			Reranked: block.reranked, RerankScore: block.rerankScore, Source: block.source,
			Kind: artifactKind(block.record.RelPath),
		}
		anyReranked = anyReranked || block.reranked
		if block.reranked && seenUnreranked {
			cleanBoundary = false
		}
		if !block.reranked {
			seenUnreranked = true
		}
	}
	anyReranked = anyReranked && cleanBoundary
	numbered := renderLineNumbersEnabled()
	var out strings.Builder
	sawReranked, markedBoundary := false, false
	fullBlocks := 0
	for i, block := range merged {
		// 精排块的序分 1/(位置+1) 来自前 sent 个位置，恒大于任何未精排块；
		// 合并块又取成员中的最高分，因此排序后所有含精排内容的块都在未
		// 精排块之前，标记插在首个未精排块之前即可。
		if block.reranked {
			sawReranked = true
		} else if anyReranked && sawReranked && !markedBoundary {
			out.WriteString(rerankBoundaryMarker + "\n")
			markedBoundary = true
		}
		if i < fullResults {
			out.WriteString(formatBlock(block.record, numbered))
			fullBlocks++
			continue
		}
		if i == fullResults && detail != detailPaths {
			out.WriteString(pathsOnlyMarker + "\n")
		}
		out.WriteString(blockHeader(block.record) + "\n")
	}
	files := make(map[string]bool, len(merged))
	for _, block := range merged {
		files[block.record.RelPath] = true
	}
	return renderedResult{
		text: strings.TrimRight(out.String(), "\n"),
		hits: inventory,
		display: engine.DisplayStats{
			CandidateBlocks: len(merged), ShownBlocks: len(merged), FullBlocks: fullBlocks, ShownFiles: len(files),
		},
	}, nil
}

// blockHeader 生成块的头行 `## path:start-end [symbol]`，与 formatBlock 输出
// 的首行完全一致；只给头行的候选与带正文的候选用同一种头行。
func blockHeader(record chunkRecord) string {
	header := fmt.Sprintf("## %s:%d-%d", record.RelPath, record.StartLine, record.EndLine)
	if record.Symbol != "" {
		header += " " + record.Symbol
	}
	return header
}

// mergeBlocks 把同一文件里重叠或严格相邻的块合并成一块。
//   - 只在 next.StartLine ≤ current.EndLine+1 时合并。中间缺行的两块不合并：
//     合并后头行标注的行区间与实际内容会错位，path:line 引用就指错行。
//   - 合并后行数超过 mergedBlockMaxLines 时 next 另起一块，两块的重叠行各
//     自完整保留，行区间与内容仍一一对应。
//   - 合并块的 score 取成员最高分，source 跟随分数更高的一方，rerankScore
//     取最大，reranked 只要有一个成员为 true 即为 true，Symbol 取第一个非
//     空值。
//
// 返回顺序不保证，调用方自行排序。
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
			adjacent := next.record.StartLine <= current.record.EndLine+1
			mergedEnd := current.record.EndLine
			if next.record.EndLine > mergedEnd {
				mergedEnd = next.record.EndLine
			}
			if adjacent && mergedEnd-current.record.StartLine+1 <= mergedBlockMaxLines {
				if next.record.EndLine > current.record.EndLine {
					current.record.Content = current.record.Content + "\n" + tailLines(next.record, current.record.EndLine)
					current.record.EndLine = next.record.EndLine
				}
				if next.score > current.score {
					current.score = next.score
					current.source = next.source
				}
				if next.rerankScore > current.rerankScore {
					current.rerankScore = next.rerankScore
				}
				current.reranked = current.reranked || next.reranked
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

// tailLines 返回 record 内容中行号大于 afterLine 的部分：合并相邻块时，
// 后一块与前一块重叠的行只保留一份。record 从 afterLine 之后开始时原样
// 返回；record 整体都在 afterLine 之前时返回空串。
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

// EnvRenderLineNumbers 是开启代码围栏内逐行行号前缀的环境变量（取值
// 1、true、on、yes 之一为开）。行号格式同 cat -n（右对齐行号加制表符），
// 与常见 Read 工具的输出同形，调用方不必再读一次文件就能精确引用行区
// 间；LLM 对这种形状很熟悉。默认关闭，输出与历史格式逐字节一致；是否改
// 为默认开启，要先拿到调用方 A/B 对比证据。
const EnvRenderLineNumbers = "OPENACE_RENDER_LINE_NUMBERS"

// renderLineNumbersEnabled 每次渲染时重新读取 EnvRenderLineNumbers，不缓存
// 结果；测试用 t.Setenv 即可切换两种输出形态。
func renderLineNumbersEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(EnvRenderLineNumbers))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// formatBlock 渲染一个带正文的块：头行 `## path:start-end [symbol]`，然后
// 是以语言名开头的代码围栏（语言为空或 text 时围栏不带语言名），末尾空
// 一行。numbered 为 true 时围栏内每行前缀真实文件行号（cat -n 形状），行
// 号从 StartLine 起算，与头行的区间一致，调用方可直接引用行号。
func formatBlock(record chunkRecord, numbered bool) string {
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
	content := strings.TrimRight(record.Content, "\n")
	if numbered {
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%6d\t%s", record.StartLine+i, line)
		}
	} else {
		b.WriteString(content)
	}
	b.WriteString("\n```\n\n")
	return b.String()
}
