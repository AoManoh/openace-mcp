package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/reliability"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// semanticOutcome 是一次构建的语义路产物：哪些 chunk 拿到了向量、向量从
// 哪里来、失败了什么。
type semanticOutcome struct {
	// enabled 表示本引擎配置了 embedding provider。为 false 时其余字段全为
	// 零值，调用方不读取它们；纯词法配置下的构建产物与没有语义路时一致。
	enabled bool
	// entries 与 vectors 逐行对应，只包含拿到向量的 chunk，顺序与传入的
	// records 相同；没有向量的 chunk 不出现。
	entries []vector.Entry
	vectors [][]float32
	// covered 是本次处理的 records 中拿到向量的 chunk 数，等于 len(entries)，
	// 复用来的向量与新嵌入的向量同样计入。全量构建时它就是新 revision 的
	// VectorCount；delta 构建只处理变更文件的 chunk，VectorCount 由各文件的
	// CoveredChunks 求和得到。
	covered int
	// coveredByActive 是其中向量直接来自当前 active revision 的行数。内容
	// 未变的构建只有 covered > coveredByActive（补进了新向量）才发布新
	// revision，否则不发布，避免没有变化的 revision 堆积（见 improved）。
	coveredByActive int
	// newlyEmbedded 是本次真正调用 provider 新获得向量的唯一键数。
	newlyEmbedded int
	// crossProfileReused 是从兼容的旧 chunk profile 子树复用到的唯一键数，
	// 进入同步结果供调用方核对升级后的复用效果。
	crossProfileReused int
	// rejected 是被拒绝的键数：provider 返回零向量或含 NaN 的向量，或该键
	// 已在持久化拒绝集内。零向量归一化时除零，NaN 会让相似度排序失真，
	// 这类 chunk 记为未覆盖。
	rejected int
	// lastError 是最后一次失败的脱敏错误文本，进入 workspace_status。
	lastError string
	// backedOff 表示有批调用因熔断退避（provider 连续失败后暂停向它发请
	// 求）被拒绝，此时后续批也不再投放。
	backedOff bool
}

// improved 报告本次构建是否比 active revision 多覆盖了 chunk。内容未变的
// 构建据此决定是否发布新 revision。
func (s semanticOutcome) improved() bool {
	return s.enabled && s.covered > s.coveredByActive
}

// embedTemplateVersion 是 embedDocText 模板的版本号。它同时进入索引子树
// 的 profile 段（storeProfile）与 embedKey：模板一变，旧子树保留、新子树
// 全量重建，旧模板的向量不会与新模板的混用。
const embedTemplateVersion = "a2p-v1"

// embedDocText 构造 document 嵌入的输入文本：一行英文头部说明 chunk 来自
// 哪个文件、什么语言、定义了什么符号，换行后接 chunk 内容。
//   - 加头部的原因：自然语言查询到代码的 dense 召回明显更好。评测中先导
//     实验 dense R@5 +14.1pp，放大实验（498 条查询）dense +9.6pp，经融合与
//     精排后的最终 R@5 +3.0pp（置信区间不含零），精确符号查询无退化。
//   - 头部不写行号：行号进入嵌入文本就会进入 embedKey，文件任何一处编辑
//     都会让其下方全部 chunk 的键改变、向量全部重嵌，增量构建的费用优势
//     消失。行号由 rerank 送审文本与渲染层提供。
//   - 模板文案已冻结：任何改动必须同时升级 embedTemplateVersion，否则新旧
//     文本的向量会在同一子树内混用。
func embedDocText(record chunkRecord) string {
	head := "This chunk is from " + record.RelPath + ", " + record.Language
	if record.Symbol != "" {
		head += ", defining " + record.Symbol
	}
	return head + ".\n" + record.Content
}

// embedKey 是向量复用与 journal 的键：sha256(模板版本, 相对路径, 符号,
// 语言, 内容 hash)。嵌入输入含路径与符号头，键必须覆盖同样的字段：若只
// 用内容 hash，两个内容相同、路径不同的 chunk（如多个空的 __init__.py）
// 会复用第一个文件的带头向量，向量里的路径信息与实际文件不符。代价：
// 文件重命名或目录移动后，内容未变的 chunk 也要重新嵌入付费。行号不进
// 键，行号漂移不触发重嵌。
func embedKey(record chunkRecord) string {
	h := sha256.Sum256([]byte(embedTemplateVersion + "\x00" + record.RelPath + "\x00" +
		record.Symbol + "\x00" + record.Language + "\x00" + record.ContentHash))
	return hex.EncodeToString(h[:])
}

// priorVectors 是构建开始前装载的既有向量：既是复用来源，也是覆盖率对比
// 的基线。
type priorVectors struct {
	// activeByHash 与 olderByHash 分别是 active revision 与其 previous
	// revision 的向量，键为 vector.Entry.ContentHash 字段的值（当前子树里
	// 即 embedKey）。复用优先级：active 高于 previous，两者都高于 journal。
	activeByHash map[string][]float32
	olderByHash  map[string][]float32
	// crossProfileByHash 来自同一 workspace、同一 embedding 身份与模板版本
	// 的旧 chunk profile 子树，由 mergeSiblingProfileVectors 注入；优先级
	// 低于 active/previous，只读复用。
	crossProfileByHash map[string][]float32
	// activeLoadedRows/activeLoadedSegments 是 active revision 实际通过校验
	// 装载的行数与段数，activeExpectedRows/activeExpectedSegments 是其
	// manifest 宣称的数量；两者不等说明 manifest 完整但向量文件损坏或缺
	// 失。loadedRows 是两个 revision 合计装载（按行选读时为实际读出）的
	// 行数。
	activeLoadedRows       int
	activeExpectedRows     int
	activeLoadedSegments   int
	activeExpectedSegments int
	loadedRows             int
	// activeIDs 是 active revision 中已持久化向量的 chunk ID 集合，两种
	// 装载策略下都按全段填充。
	activeIDs map[string]bool
	// indexes 持有底层数据：整段装载时是 *vector.Index，各 byHash 表的值
	// 是其数据的子切片，可能指向 mmap 页；按行选读时是 *vector.RowReader，
	// 表值是独立拷贝。构建方用完 prior 必须调用 release，否则映射与文件
	// 句柄一直保留到进程退出。
	indexes []io.Closer
}

// release 关闭全部底层向量索引，可重复调用。调用后各 byHash 表中来自
// 整段装载的值切片全部失效，不得再读。
func (p *priorVectors) release() {
	for _, ix := range p.indexes {
		_ = ix.Close()
	}
	p.indexes = nil
}

// loadPriorVectors 装载 active revision（hop 0）与其 previous revision
// （hop 1）全部 segment 的向量；revision GC 恰好保留这两个 revision。
// 向量文件损坏的 segment 被跳过，缺口由本次新嵌入补齐。
//
// needed 决定装载策略：
//   - nil：整段装载，做数据文件的全文件 checksum 校验。用于全量构建、
//     compaction 与兄弟子树复用这些几乎全部行都要复用的场景。
//   - 非 nil：按行选读，只读出键命中 needed 的行。delta 构建实际复用的
//     行数约等于变更 chunk 数，为几十行整读 GiB 级的段不划算。按行路径
//     的完整性检查是 idx 文件 checksum 加每行范数探针，不做数据文件的
//     全文件 checksum，弱化之处记录在 vector.RowReader。
//
// 两种策略下 activeLoadedRows/activeLoadedSegments 口径一致：段结构校验
// 通过即计入全段可寻址行，表示完整性，不表示读出的行数。用户配置了向量
// 内存预算（vectorMaxResident>0）时，累计行数达到预算即停止装载后续段。
func (e *Engine) loadPriorVectors(store *index.Store, previous *index.Manifest, needed map[string]bool) priorVectors {
	prior := priorVectors{
		activeByHash:       map[string][]float32{},
		olderByHash:        map[string][]float32{},
		crossProfileByHash: map[string][]float32{},
		activeIDs:          map[string]bool{},
	}
	dimension := e.embedCfg.Dimension
	manifest := previous
	seenSegments := make(map[string]bool)
	loadedRows := 0
	limit := e.vectorMaxResident // 0 表示不限（默认）；>0 来自 OPENACE_VECTOR_MEMORY_BUDGET 折算的行数
	if previous != nil {
		prior.activeExpectedRows = previous.VectorCount
		for _, segment := range previous.Segments {
			if segment.VectorsChecksum != "" {
				prior.activeExpectedSegments++
			}
		}
	}
	for hop := 0; manifest != nil && hop < 2; hop++ {
		for _, segment := range manifest.Segments {
			if segment.VectorsChecksum == "" || seenSegments[segment.ID] {
				continue
			}
			seenSegments[segment.ID] = true
			remaining := 0
			if limit > 0 {
				remaining = limit - loadedRows
				if remaining <= 0 {
					break
				}
			}
			if needed != nil {
				loadedRows = e.loadPriorRowsSelective(&prior, store, segment, dimension, hop, needed, limit, loadedRows)
				continue
			}
			ix, err := vector.Load(store.SegmentPathFor(segment.ID), dimension,
				segment.VectorsChecksum, segment.VectorsIndexChecksum, remaining)
			if err != nil {
				continue
			}
			prior.indexes = append(prior.indexes, ix)
			loadedRows += ix.Count()
			prior.loadedRows = loadedRows
			if hop == 0 {
				prior.activeLoadedRows += ix.Count()
				prior.activeLoadedSegments++
			}
			for i, entry := range ix.Entries() {
				if hop == 0 {
					prior.activeIDs[entry.ID] = true
					if _, ok := prior.activeByHash[entry.ContentHash]; !ok {
						prior.activeByHash[entry.ContentHash] = ix.Row(i)
					}
				} else if _, ok := prior.olderByHash[entry.ContentHash]; !ok {
					prior.olderByHash[entry.ContentHash] = ix.Row(i)
				}
			}
		}
		if manifest.PreviousRevision == "" || (limit > 0 && loadedRows >= limit) {
			break
		}
		older, err := store.LoadManifest(manifest.PreviousRevision)
		if err != nil {
			break
		}
		manifest = older
	}
	return prior
}

// loadPriorRowsSelective 是 loadPriorVectors 在 needed 非 nil 时对单个
// segment 的处理：打开按行读取器，把段内全部 chunk ID 计入 activeIDs
// （仅 hop 0），只对键命中 needed 且目标表尚无该键的行读取向量。返回累计
// 读出的行数。段打不开或某行读出损坏时跳过，与整段装载遇到损坏的处理
// 一致，缺口由新嵌入补齐；累计行数达到内存预算即停止。
func (e *Engine) loadPriorRowsSelective(prior *priorVectors, store *index.Store, segment index.SegmentRef, dimension int, hop int, needed map[string]bool, limit int, loadedRows int) int {
	reader, err := vector.OpenRowReader(store.SegmentPathFor(segment.ID), dimension, segment.VectorsIndexChecksum)
	if err != nil {
		return loadedRows
	}
	prior.indexes = append(prior.indexes, reader)
	entries := reader.Entries()
	if hop == 0 {
		prior.activeLoadedRows += len(entries)
		prior.activeLoadedSegments++
	}
	for i, entry := range entries {
		if hop == 0 {
			prior.activeIDs[entry.ID] = true
		}
		if !needed[entry.ContentHash] {
			continue
		}
		target := prior.activeByHash
		if hop != 0 {
			target = prior.olderByHash
		}
		if _, ok := target[entry.ContentHash]; ok {
			continue
		}
		if limit > 0 && loadedRows >= limit {
			break
		}
		row, err := reader.ReadRow(i)
		if err != nil {
			continue
		}
		target[entry.ContentHash] = row
		loadedRows++
		prior.loadedRows = loadedRows
	}
	return loadedRows
}

// embedRecords 为 records（全量或 delta 构建产出的 chunk 记录）组装向量集。
//   - 先按 embedKey 复用既有向量，来源按优先级依次为 active revision、
//     previous revision、兼容的旧 profile 子树、journal（上次构建中断前已
//     付费落盘的批次），取第一个命中。复用是逐位拷贝，同一 chunk 跨
//     revision 的向量完全一致。
//   - 再对仍缺失的唯一键调用 provider。每批成功立刻写入 journal，之后即使
//     构建被取消或进程被杀，这批付费向量下次构建仍可复用。
//   - provider 失败只记录不中断：词法索引的新鲜度优先于语义覆盖，本次拿到
//     的向量照常入盘，缺口如实进入覆盖率。只有 ctx 取消与本地落盘失败才
//     返回错误。
//   - 语义路未配置或 records 为空时直接返回只带 enabled 标志的空产物。
func (e *Engine) embedRecords(ctx context.Context, store *index.Store, workspaceKey string, prior priorVectors, records []chunkRecord, status *wsStatus) (semanticOutcome, error) {
	out := semanticOutcome{enabled: e.semanticEnabled()}
	if !out.enabled || len(records) == 0 {
		return out, nil
	}
	journal, err := e.journalFor(workspaceKey, store)
	if err != nil {
		return out, fmt.Errorf("打开 embedding journal: %w", err)
	}
	status.setStage(engine.IndexStageEmbedding)

	// 1) 复用表按优先级填充：active revision、previous revision、兼容的旧
	// profile 子树、journal，已有的键不被低优先级来源覆盖。旧 profile 子树
	// 的向量只在 embedding 身份与模板版本完全一致时才会被
	// mergeSiblingProfileVectors 注入到 prior。
	reuse := make(map[string][]float32, len(prior.activeByHash))
	activeUsable := make(map[string]bool, len(prior.activeByHash))
	crossUsable := make(map[string]bool, len(prior.crossProfileByHash))
	for hash, vec := range prior.activeByHash {
		reuse[hash] = vec
		activeUsable[hash] = true
	}
	for hash, vec := range prior.olderByHash {
		if _, ok := reuse[hash]; !ok {
			reuse[hash] = vec
		}
	}
	for hash, vec := range prior.crossProfileByHash {
		if _, ok := reuse[hash]; !ok {
			reuse[hash] = vec
			crossUsable[hash] = true
		}
	}
	for hash, vec := range journal.Snapshot() {
		if _, ok := reuse[hash]; !ok {
			reuse[hash] = vec
		}
	}

	// 2) 缺失清单：按 records 首次出现的顺序收集唯一 embedKey，同一输入每次
	// 切出相同的批次。已在持久化拒绝集（provider 曾对该内容返回零向量或
	// NaN）里的键不再送 provider：拒绝集跨重启保留，否则 watcher 每个周期
	// 都会为同一段病理内容重复付费。
	var missingHashes []string
	var missingTexts []string
	reusedRows := 0
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		key := embedKey(record)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := reuse[key]; ok {
			reusedRows++
			if crossUsable[key] {
				out.crossProfileReused++
			}
			continue
		}
		if journal.Rejected(key) {
			out.rejected++
			continue
		}
		missingHashes = append(missingHashes, key)
		missingTexts = append(missingTexts, embedDocText(record))
	}

	// 付费前的预算拦截，仅在用户配置了 OPENACE_VECTOR_MEMORY_BUDGET 时生效
	// （默认不限）。发布后常驻向量行数的投影 = 已复用行数 + 待新嵌行数；
	// 超出预算就在任何 provider 调用之前停止：已复用部分照常入盘发布，缺口
	// 行保持未覆盖并写明原因。若先付费、发布后才在查询期发现装不下，钱
	// 已花掉而向量用不上。delta 构建时物理段的行数可能高于此投影（被删除
	// chunk 的死行在 compaction 阈值前仍保留），这部分由查询侧装载时的累计
	// 预算校验处理。
	if limit := e.vectorMaxResident; limit > 0 && len(missingHashes) > 0 {
		if projected := reusedRows + len(missingHashes); projected > limit {
			out.lastError = fmt.Sprintf("vector memory budget exceeded before embedding: projected %d rows > budget %d rows (%s); missing chunks left uncovered explicitly", projected, limit, EnvVectorMemoryBudget)
			status.setEmbedProgress(len(missingHashes), 0)
			return e.assembleSemanticOutcome(records, reuse, activeUsable, out), nil
		}
	}

	// 3) 分批嵌入。单批失败只记录并继续，成功的批照常入盘。熔断进入退避
	// （provider 连续失败后暂停向它发请求）时停止投放后续批，后续批都会
	// 被同样拒绝。ctx 取消中止整次构建。进度按批写入状态，构建期的
	// workspace_status 能看到嵌入进展。批之间按 MaxConcurrency 并行：
	// provider 单次请求延迟在秒级且波动大，串行时整次构建的吞吐等于单请
	// 求延迟。每批要么全部成功要么整批失败，journal 自身持锁，并行不改变
	// 落盘语义。
	status.setEmbedProgress(len(missingHashes), 0)
	// 用户开启了批接口且缺失数达到阈值时，改走 provider 的批作业路径（费用
	// 低 33%，服务端排队窗口最长 12 小时；崩溃安全与失败处理见
	// semantic_bulk.go）。回收的向量写入同一 journal 与 reuse，之后与同步
	// 路径共用 assembleSemanticOutcome，产物等价。
	if len(missingHashes) > 0 && e.bulkEligible(len(missingHashes)) {
		if err := e.bulkEmbedMissing(ctx, journal, status, missingHashes, missingTexts, reuse, &out); err != nil {
			return out, err
		}
		return e.assembleSemanticOutcome(records, reuse, activeUsable, out), nil
	}
	batchSize := e.embedCfg.BatchSize
	if batchSize < 1 {
		// 环境变量路径已校验 BatchSize ≥1，但程序化构造 Options 可能传 0；
		// 不钳位时下面切分批次的步长为 0，循环不会结束。workers 同样钳位。
		batchSize = 1
	}
	type batchSpan struct{ start, end int }
	var batches []batchSpan
	for start := 0; start < len(missingHashes); start += batchSize {
		end := start + batchSize
		if end > len(missingHashes) {
			end = len(missingHashes)
		}
		batches = append(batches, batchSpan{start: start, end: end})
	}
	workers := e.embedCfg.MaxConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(batches) {
		workers = len(batches)
	}
	var (
		embedMu   sync.Mutex
		embedded  int
		fatalErr  error
		workQueue = make(chan batchSpan)
		wg        sync.WaitGroup
	)
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	recordFatal := func(err error) {
		embedMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		embedMu.Unlock()
		cancelWork()
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for span := range workQueue {
				if workCtx.Err() != nil {
					return
				}
				vectors, err := e.embedClient.EmbedBatch(workCtx, missingTexts[span.start:span.end], embedding.InputDocument)
				if err != nil {
					if ctx.Err() != nil {
						recordFatal(ctx.Err())
						return
					}
					if workCtx.Err() != nil {
						// 另一个 worker 已经触发停止（退避或致命错误），本批是
						// 因 workCtx 被取消而失败，这个取消错误不应覆盖真实
						// 原因。
						return
					}
					embedMu.Lock()
					out.lastError = sanitizeError(err)
					embedMu.Unlock()
					callErr := &reliability.CallError{}
					if errors.As(err, &callErr) && callErr.Class == reliability.ClassBackoff {
						// 熔断已进入退避，后续每一批都会被同样拒绝；取消
						// workCtx 让已入队的批次立刻退出，不再逐批撞退避。
						embedMu.Lock()
						out.backedOff = true
						embedMu.Unlock()
						cancelWork()
					}
					continue
				}
				batchGood := make(map[string][]float32, len(vectors))
				var batchRejected []string
				rejectedCount := 0
				for i, vec := range vectors {
					if err := vector.Normalize(vec); err != nil {
						// provider 返回零向量或含 NaN：归一化会除零，NaN 会让
						// 排序失真，该内容记为未覆盖并计数；同时写入持久化拒
						// 绝集，重启后也不再为它付费。
						rejectedCount++
						batchRejected = append(batchRejected, missingHashes[span.start+i])
						continue
					}
					batchGood[missingHashes[span.start+i]] = vec
				}
				// 每批成功立刻写入 journal：之后即使构建被取消或进程被杀，
				// 这批已付费的向量下次构建仍能复用。
				if err := journal.Append(batchGood); err != nil {
					recordFatal(fmt.Errorf("journal 落盘: %w", err))
					return
				}
				if err := journal.MarkRejected(batchRejected); err != nil {
					recordFatal(fmt.Errorf("journal 拒绝集落盘: %w", err))
					return
				}
				embedMu.Lock()
				out.rejected += rejectedCount
				for hash, vec := range batchGood {
					reuse[hash] = vec
					out.newlyEmbedded++
				}
				embedded += span.end - span.start
				status.setEmbedProgress(len(missingHashes)-embedded, embedded)
				embedMu.Unlock()
			}
		}()
	}
	for _, span := range batches {
		if workCtx.Err() != nil {
			break
		}
		select {
		case workQueue <- span:
		case <-workCtx.Done():
		}
	}
	close(workQueue)
	wg.Wait()
	if fatalErr != nil {
		return out, fatalErr
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}

	return e.assembleSemanticOutcome(records, reuse, activeUsable, out), nil
}

// assembleSemanticOutcome 把 reuse 中的向量按 records 顺序组装成
// entries/vectors，并统计 covered 与 coveredByActive；同步与批作业两条
// 路径共用。embedKey 相同的多条记录共享同一个向量值。vector.Entry 的
// ContentHash 字段存放的是 embedKey：字段名沿用旧版，索引子树按模板
// 版本隔离，同一子树内该字段的含义恒定。
func (e *Engine) assembleSemanticOutcome(records []chunkRecord, reuse map[string][]float32, activeUsable map[string]bool, out semanticOutcome) semanticOutcome {
	for _, record := range records {
		key := embedKey(record)
		vec, ok := reuse[key]
		if !ok {
			continue
		}
		out.entries = append(out.entries, vector.Entry{ID: record.ID, ContentHash: key})
		out.vectors = append(out.vectors, vec)
		out.covered++
		if activeUsable[key] {
			out.coveredByActive++
		}
	}
	return out
}
