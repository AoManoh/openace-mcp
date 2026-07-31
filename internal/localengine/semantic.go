package localengine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/reliability"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// semanticOutcome 是一次构建的语义路产物。
type semanticOutcome struct {
	// enabled 表示语义路已配置（false 时其余字段无意义，K32）。
	enabled bool
	// entries/vectors 与本次 records 对齐（仅覆盖成功者），行序 = records 序。
	entries []vector.Entry
	vectors [][]float32
	// covered 是有向量的 chunk 行数（= len(entries)，暗坑 K31）。
	covered int
	// coveredByActive 是可由当前 active revision 向量满足的行数
	// （D10 发布判定基线：covered > coveredByActive 才算有改善）。
	coveredByActive int
	// newlyEmbedded 是本次经 provider 新获取的唯一内容数。
	newlyEmbedded int
	// rejected 是零向量/NaN 被拒的数量（暗坑 K35）。
	rejected int
	// lastError 是最后一次批失败的脱敏消息（状态上报）。
	lastError string
	// backedOff 表示批调用因 circuit 退避被拒（D10/K30 no-op 依据）。
	backedOff bool
}

// improved 报告语义覆盖相对 active revision 是否有实质改善。
func (s semanticOutcome) improved() bool {
	return s.enabled && s.covered > s.coveredByActive
}

// priorVectors 是构建前装载的既有向量视图（D2 复用源 + 覆盖口径基线）。
type priorVectors struct {
	// activeByHash/olderByHash 分别是 active revision 与其 previous 的
	// 向量数据（按纯 content hash 键控，revision 优先级高于 journal）。
	activeByHash map[string][]float32
	olderByHash  map[string][]float32
	// activeIDs 是 active revision 中已持久化向量的 chunk ID 集
	// （delta 构建计算未触及 chunk 覆盖时使用，暗坑 K51）。
	activeIDs map[string]bool
}

// loadPriorVectors 装载 active（hop 0）与其 previous（hop 1）全部 segment
// 的向量。GC 保留恰好这两个 revision；损坏的向量文件被跳过，由后续
// 新嵌入补齐（K25 自愈路径）。
func (e *Engine) loadPriorVectors(store *index.Store, previous *index.Manifest) priorVectors {
	prior := priorVectors{
		activeByHash: map[string][]float32{},
		olderByHash:  map[string][]float32{},
		activeIDs:    map[string]bool{},
	}
	dimension := e.embedCfg.Dimension
	manifest := previous
	for hop := 0; manifest != nil && hop < 2; hop++ {
		for _, segment := range manifest.Segments {
			if segment.VectorsChecksum == "" {
				continue
			}
			ix, err := vector.Load(store.SegmentPathFor(segment.ID), dimension,
				segment.VectorsChecksum, segment.VectorsIndexChecksum, 0)
			if err != nil {
				continue
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

// embedRecords 组装 records（全量或 delta）的向量集：先按纯 content hash
// 复用既有向量（阶段计划 D2，bit 级拷贝保证跨 revision 一致）与 journal
// 暂存（Stage 4 D4：中断构建的已付费批次），再对缺失的唯一内容批量调用
// provider——每批成功即落 journal，取消/kill 不再丢弃付费进度。provider
// 失败不阻塞构建（§10.2 lexical freshness 优先），仅 ctx 取消与本地持久
// 化故障返回错误。
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

	// 1) 复用优先级：active revision → previous revision → journal（K41）。
	reuse := make(map[string][]float32, len(prior.activeByHash))
	activeUsable := make(map[string]bool, len(prior.activeByHash))
	for hash, vec := range prior.activeByHash {
		reuse[hash] = vec
		activeUsable[hash] = true
	}
	for hash, vec := range prior.olderByHash {
		if _, ok := reuse[hash]; !ok {
			reuse[hash] = vec
		}
	}
	for hash, vec := range journal.Snapshot() {
		if _, ok := reuse[hash]; !ok {
			reuse[hash] = vec
		}
	}

	// 2) 缺失清单：唯一 content hash，按 records 首次出现序（确定性批次；
	// 同内容多处出现只嵌入一次——嵌入输入为纯 chunk 内容，D6）。
	// 持久化拒绝集（K35 修订）内的零向量内容不再送 provider，跨重启
	// 防止 watcher 周期对病理内容反复付费。
	var missingHashes []string
	var missingTexts []string
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if seen[record.ContentHash] {
			continue
		}
		seen[record.ContentHash] = true
		if _, ok := reuse[record.ContentHash]; ok {
			continue
		}
		if journal.Rejected(record.ContentHash) {
			out.rejected++
			continue
		}
		missingHashes = append(missingHashes, record.ContentHash)
		missingTexts = append(missingTexts, record.Content)
	}

	// 3) 分批嵌入：单批失败记录并继续（部分成功入盘，D10）；circuit
	// 退避即停（后续批必然拒绝，K30）；取消中止整次构建。进度按批
	// 写入状态（D8：构建期 workspace_status 可见嵌入进展）。批间按
	// MaxConcurrency 并行（T10b：provider 单请求延迟秒级且波动大，
	// 串行会把构建吞吐钉死在单请求延迟上）；每批 all-or-nothing 与
	// journal 落盘语义不变，journal 自身持锁。
	status.setEmbedProgress(len(missingHashes), 0)
	batchSize := e.embedCfg.BatchSize
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
						// 同伴 worker 已触发停止（退避/致命错误），本批的
						// 取消回声不覆盖真实原因。
						return
					}
					embedMu.Lock()
					out.lastError = sanitizeError(err)
					embedMu.Unlock()
					callErr := &reliability.CallError{}
					if errors.As(err, &callErr) && callErr.Class == reliability.ClassBackoff {
						// circuit 退避：停止投放后续批（K30），已入队
						// 批次经 workCtx 取消快速退出。
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
						// 零向量/NaN：该内容记为未覆盖并计数（K35），持久化
						// 拒绝史跨重启防重复付费。
						rejectedCount++
						batchRejected = append(batchRejected, missingHashes[span.start+i])
						continue
					}
					batchGood[missingHashes[span.start+i]] = vec
				}
				// 批成功即落 journal（D4/G2）：随后即使构建被取消/kill，
				// 这批付费向量也可被下次构建复用。
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

	// 4) 对齐 records 组装行集（同内容多行共享同一向量值）。
	for _, record := range records {
		vec, ok := reuse[record.ContentHash]
		if !ok {
			continue
		}
		out.entries = append(out.entries, vector.Entry{ID: record.ID, ContentHash: record.ContentHash})
		out.vectors = append(out.vectors, vec)
		out.covered++
		if activeUsable[record.ContentHash] {
			out.coveredByActive++
		}
	}
	return out, nil
}
