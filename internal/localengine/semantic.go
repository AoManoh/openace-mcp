package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

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
	// crossProfileReused 是从兼容的兄弟索引子树（旧 chunk profile 或旧词法
	// 评分模型的子树，revision 向量与 journal 条目都算）复用到的唯一键数，
	// 进入同步结果供调用方核对升级后的复用效果。
	crossProfileReused int
	// migrationBlock 非空时，同一 embedding 身份旧子树里的已付费状态无法
	// 确认或尚未回收（见 siblingPaidStateFor），本次没有为缺口发任何文档
	// 嵌入请求；值是原因与恢复步骤，同时写进 lastError。
	migrationBlock string
	// rejected 是被拒绝的键数：provider 返回零向量或含 NaN 的向量，或该键
	// 已在持久化拒绝集内。零向量归一化时除零，NaN 会让相似度排序失真，
	// 这类 chunk 记为未覆盖。
	rejected int
	// rejectedKeys 是 rejected 计数对应的键集合，构建按文件统计
	// FileEntry.RejectedChunks 时使用。
	rejectedKeys map[string]bool
	// lastError 是最后一次失败的脱敏错误文本，进入 workspace_status。
	lastError string
}

// markRejected 记下一个被拒绝的键并计数。
func (s *semanticOutcome) markRejected(key string) {
	if s.rejectedKeys == nil {
		s.rejectedKeys = make(map[string]bool)
	}
	s.rejectedKeys[key] = true
	s.rejected++
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
	// 的兄弟子树（旧 chunk profile 或旧词法评分模型）的 revision，由
	// mergeSiblingProfileVectors 注入；优先级低于 active/previous，只读复用。
	crossProfileByHash map[string][]float32
	// activeLoadedSegments 是 active revision 实际通过校验装载的段数，
	// activeExpectedSegments 是其 manifest 宣称的段数；两者不等说明 manifest
	// 完整但向量文件损坏或缺失，构建与嵌入计划据此决定是否并入旧切分版本
	// 子树的向量。activeLoadedRows 是 active revision 装载的行数，只供测试核对
	// 装载策略。
	activeLoadedRows       int
	activeLoadedSegments   int
	activeExpectedSegments int
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
// 通过即计入全段可寻址行，表示完整性，不表示读出的行数。
func (e *Engine) loadPriorVectors(store *index.Store, previous *index.Manifest, needed map[string]bool) priorVectors {
	prior := priorVectors{
		activeByHash:       map[string][]float32{},
		olderByHash:        map[string][]float32{},
		crossProfileByHash: map[string][]float32{},
	}
	dimension := e.embedCfg.Dimension
	manifest := previous
	seenSegments := make(map[string]bool)
	if previous != nil {
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
			if needed != nil {
				e.loadPriorRowsSelective(&prior, store, segment, dimension, hop, needed)
				continue
			}
			ix, err := vector.Load(store.SegmentPathFor(segment.ID), dimension,
				segment.VectorsChecksum, segment.VectorsIndexChecksum)
			if err != nil {
				continue
			}
			prior.indexes = append(prior.indexes, ix)
			if hop == 0 {
				prior.activeLoadedRows += ix.Count()
				prior.activeLoadedSegments++
			}
			for i, entry := range ix.Entries() {
				if hop == 0 {
					if _, ok := prior.activeByHash[entry.ContentHash]; !ok {
						prior.activeByHash[entry.ContentHash] = ix.Row(i)
					}
				} else if _, ok := prior.olderByHash[entry.ContentHash]; !ok {
					prior.olderByHash[entry.ContentHash] = ix.Row(i)
				}
			}
		}
		if manifest.PreviousRevision == "" {
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
// segment 的处理：打开按行读取器，只对键命中 needed 且目标表尚无该键的行
// 读取向量。段打不开或某行读出损坏时跳过，与整段装载遇到损坏的处理一致，
// 缺口由新嵌入补齐。
func (e *Engine) loadPriorRowsSelective(prior *priorVectors, store *index.Store, segment index.SegmentRef, dimension int, hop int, needed map[string]bool) {
	reader, err := vector.OpenRowReader(store.SegmentPathFor(segment.ID), dimension, segment.VectorsIndexChecksum)
	if err != nil {
		return
	}
	prior.indexes = append(prior.indexes, reader)
	entries := reader.Entries()
	if hop == 0 {
		prior.activeLoadedRows += len(entries)
		prior.activeLoadedSegments++
	}
	for i, entry := range entries {
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
		if _, ok := prior.activeByHash[entry.ContentHash]; ok {
			continue
		}
		row, err := reader.ReadRow(i)
		if err != nil {
			continue
		}
		target[entry.ContentHash] = row
	}
}

// embedRecords 为 records（全量或 delta 构建产出的 chunk 记录）组装向量集。
//   - 先按 embedKey 复用既有向量，来源按优先级依次为 active revision、
//     previous revision、兼容的旧 profile 子树、journal（上次构建中断前已
//     付费落盘的批次），取第一个命中。复用是逐位拷贝，同一 chunk 跨
//     revision 的向量完全一致。
//   - 当前子树拒绝集里的键不送 provider。仍有缺口时先经 siblingPaidStateFor
//     只读检查同一 embedding 身份旧子树的已付费状态：其 journal 里的向量
//     直接复用，其拒绝集里的键不送审并记进当前子树的拒绝集；旧状态无法
//     确认或尚未回收时，所有缺口都不送 provider，按未覆盖发布并在
//     lastError 与 migrationBlock 说明原因。
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
			out.markRejected(key)
			continue
		}
		missingHashes = append(missingHashes, key)
		missingTexts = append(missingTexts, embedDocText(record))
	}
	if len(missingHashes) > 0 {
		processed, err := e.recoverBulkState(ctx, journal, status, reuse, &out)
		if err != nil {
			return out, err
		}
		if len(processed) > 0 {
			var keptHashes, keptTexts []string
			for i, key := range missingHashes {
				if !processed[key] {
					keptHashes = append(keptHashes, key)
					keptTexts = append(keptTexts, missingTexts[i])
				}
			}
			missingHashes, missingTexts = keptHashes, keptTexts
		}
	}

	// 送审前只读检查同一 embedding 身份旧子树里与这些缺口有关的已付费状态
	// （换子树后它们仍留在旧子树）：journal 里已付费的向量直接复用；旧拒绝
	// 集里的键不送审，并写进当前子树的拒绝集；旧状态无法确认或尚未回收时，
	// 不向 provider 发任何文档嵌入请求，复用到的向量照常组装发布，缺口按
	// 未覆盖如实上报，原因与恢复步骤写进 lastError。
	if len(missingHashes) > 0 {
		paid := e.siblingPaidStateFor(store, missingHashes)
		var inherited []string
		keptHashes := make([]string, 0, len(missingHashes))
		keptTexts := make([]string, 0, len(missingTexts))
		for i, key := range missingHashes {
			if vec, ok := paid.vectors[key]; ok {
				reuse[key] = vec
				reusedRows++
				out.crossProfileReused++
				continue
			}
			if paid.rejected[key] {
				inherited = append(inherited, key)
				out.markRejected(key)
				continue
			}
			keptHashes = append(keptHashes, key)
			keptTexts = append(keptTexts, missingTexts[i])
		}
		missingHashes, missingTexts = keptHashes, keptTexts
		if err := journal.MarkRejected(inherited); err != nil {
			return out, fmt.Errorf("journal 拒绝集落盘: %w", err)
		}
		if len(missingHashes) > 0 && paid.blockReason != "" {
			out.lastError = paid.blockReason
			out.migrationBlock = paid.blockReason
			status.setEmbedProgress(len(missingHashes), 0)
			return e.assembleSemanticOutcome(records, reuse, activeUsable, out), nil
		}
	}

	// 3) 分批嵌入。单批失败只记录并继续，成功的批照常入盘。熔断进入退避
	// （provider 连续失败后暂停向它发请求）时停止投放后续批，后续批都会
	// 被同样拒绝。ctx 取消中止整次构建。进度按批写入状态，构建期的
	// workspace_status 能看到嵌入进展。批次投放跟随客户端的动态窗口：
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
		// 不钳位时下面切分批次的步长为 0，循环不会结束。
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
	var (
		embedMu  sync.Mutex
		embedded int
		fatalErr error
		wg       sync.WaitGroup
	)
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	// 嵌入阶段前后吞吐治理器累计计数之差进状态（last_embed_*）：构建慢时
	// 据此区分请求多、被限速还是单个请求慢。
	governorBefore := e.embedClient.GovernorSnapshot()
	recordFatal := func(err error) {
		embedMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		embedMu.Unlock()
		cancelWork()
	}
	processSpan := func(span batchSpan, batch *embedding.IndexBatch, retryWaiting func(bool)) {
		if workCtx.Err() != nil {
			return
		}
		vectors, err := batch.Embed(workCtx, retryWaiting)
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
				// workCtx 让已入队的批次立刻退出，不再逐批发出注定被
				// 拒绝的请求。停止后续批与记录覆盖缺口都由这次取消和
				// lastError 承担，不另设标记字段。
				cancelWork()
			}
			return
		}
		batchGood := make(map[string][]float32, len(vectors))
		var batchRejected []string
		for i, vec := range vectors {
			if err := vector.Normalize(vec); err != nil {
				// provider 返回零向量或含 NaN：归一化会除零，NaN 会让
				// 排序失真，该内容记为未覆盖并计数；同时写入持久化拒
				// 绝集，重启后也不再为它付费。
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
		for _, key := range batchRejected {
			out.markRejected(key)
		}
		for hash, vec := range batchGood {
			reuse[hash] = vec
			out.newlyEmbedded++
		}
		embedded += span.end - span.start
		status.setEmbedProgress(len(missingHashes)-embedded, embedded)
		embedMu.Unlock()
	}
	type batchEvent struct {
		id            int
		waiting, done bool
	}
	events := make(chan batchEvent)
	waiting := make(map[int]bool)
	next, running, waitingCount := 0, 0, 0
	resourceTick := time.NewTicker(100 * time.Millisecond)
	defer resourceTick.Stop()
	cancelled := workCtx.Done()
	for next < len(batches) || running > 0 {
		changed := e.embedClient.IndexWindowChanges()
		// 当前窗口之外只实体化一个待准入批。重试睡眠不计入正在执行的
		// 批次数，但仍通过客户端的逻辑批次内存检查控制新建 goroutine。
		for workCtx.Err() == nil && next < len(batches) && running-waitingCount <= e.embedClient.GovernorSnapshot().Window {
			id, span := next, batches[next]
			batch, ok := e.embedClient.TryIndexBatch(missingTexts[span.start:span.end])
			if !ok {
				break
			}
			next++
			running++
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer batch.Close()
				defer func() { events <- batchEvent{id: id, done: true} }()
				processSpan(span, batch, func(isWaiting bool) {
					select {
					case events <- batchEvent{id: id, waiting: isWaiting}:
					case <-workCtx.Done():
					}
				})
			}()
		}
		if running == 0 && (next == len(batches) || workCtx.Err() != nil) {
			break
		}
		select {
		case event := <-events:
			if event.done {
				running--
				if waiting[event.id] {
					waitingCount--
				}
				delete(waiting, event.id)
			} else if waiting[event.id] != event.waiting {
				waiting[event.id] = event.waiting
				if event.waiting {
					waitingCount++
				} else {
					waitingCount--
				}
			}
		case <-changed:
		case <-resourceTick.C:
		case <-cancelled:
			cancelled = nil
		}
	}
	wg.Wait()
	status.setEmbedRequestStats(governorBefore, e.embedClient.GovernorSnapshot())
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
