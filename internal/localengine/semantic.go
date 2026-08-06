package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// crossProfileReused 是实际命中兼容旧 profile 子树的唯一键数。
	crossProfileReused int
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

// embedTemplateVersion 是 document 嵌入模板版本(方案①,2026-08-02 批准):
// 进 storeProfile(版本变化 = 平行子树全量重建)与 embedKey。
const embedTemplateVersion = "a2p-v1"

// embedDocText 构造 document 嵌入输入(A2' 模板,冻结):NL 化 path/
// language/symbol 头 + 内容。证据:E3 spike +14.1pp dense R@5(零反向)、
// 放大实验 n=498 dense +9.6pp、端到端 fusion+rerank +3.0pp(CI 排零)、
// exact 探针零退化、voyage-context-3 对照臂 -7.7pp 被否——手工模板胜出。
// R2:行号显式不进模板(行号进键会使任意编辑级联失效下方全部 chunk 的
// 嵌入复用,摧毁增量经济性;行号信息由 rerank 头与渲染层承担)。
func embedDocText(record chunkRecord) string {
	head := "This chunk is from " + record.RelPath + ", " + record.Language
	if record.Symbol != "" {
		head += ", defining " + record.Symbol
	}
	return head + ".\n" + record.Content
}

// embedKey 是嵌入复用/journal 的键(R1):模板使嵌入输入 = f(path, symbol,
// language, content),键随之升级——纯 content hash 键会让同内容异路径
// chunk 静默复用首个文件的带头向量(路径串味)。代价如实声明:重命名
// (含目录移动)后同内容 chunk 需重嵌(D2 "rename 零重付" 条款经方案①
// 批准修订);行号漂移不改键(R2)。
func embedKey(record chunkRecord) string {
	h := sha256.Sum256([]byte(embedTemplateVersion + "\x00" + record.RelPath + "\x00" +
		record.Symbol + "\x00" + record.Language + "\x00" + record.ContentHash))
	return hex.EncodeToString(h[:])
}

// priorVectors 是构建前装载的既有向量视图（D2 复用源 + 覆盖口径基线）。
type priorVectors struct {
	// activeByHash/olderByHash 分别是 active revision 与其 previous 的
	// 向量数据（按纯 content hash 键控，revision 优先级高于 journal）。
	activeByHash map[string][]float32
	olderByHash  map[string][]float32
	// crossProfileByHash 来自同 workspace、同 embedding identity/template
	// 的旧 chunk profile 子树。优先级低于当前 active/previous,只读复用。
	crossProfileByHash map[string][]float32
	// activeIDs 是 active revision 中已持久化向量的 chunk ID 集
	// （delta 构建计算未触及 chunk 覆盖时使用，暗坑 K51）。
	activeIDs map[string]bool
}

// loadPriorVectors 装载 active（hop 0）与其 previous（hop 1）全部 segment
// 的向量。GC 保留恰好这两个 revision；损坏的向量文件被跳过，由后续
// 新嵌入补齐（K25 自愈路径）。
func (e *Engine) loadPriorVectors(store *index.Store, previous *index.Manifest) priorVectors {
	prior := priorVectors{
		activeByHash:       map[string][]float32{},
		olderByHash:        map[string][]float32{},
		crossProfileByHash: map[string][]float32{},
		activeIDs:          map[string]bool{},
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

	// 1) 复用优先级：active revision → previous revision → 兼容旧
	// profile 子树 → journal。跨 profile 只在 embedding identity/template
	// 精确一致时由发现层注入。
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

	// 2) 缺失清单：唯一 embedKey，按 records 首次出现序（确定性批次；
	// R1:键 = f(模板版本, path, symbol, language, contentHash)——嵌入
	// 输入带路径头后,同内容异路径必须各自成键,防止带头向量串路径）。
	// 持久化拒绝集（K35 修订）内的零向量内容不再送 provider，跨重启
	// 防止 watcher 周期对病理内容反复付费。
	var missingHashes []string
	var missingTexts []string
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		key := embedKey(record)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := reuse[key]; ok {
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

	// 3) 分批嵌入：单批失败记录并继续（部分成功入盘，D10）；circuit
	// 退避即停（后续批必然拒绝，K30）；取消中止整次构建。进度按批
	// 写入状态（D8：构建期 workspace_status 可见嵌入进展）。批间按
	// MaxConcurrency 并行（T10b：provider 单请求延迟秒级且波动大，
	// 串行会把构建吞吐钉死在单请求延迟上）；每批 all-or-nothing 与
	// journal 落盘语义不变，journal 自身持锁。
	status.setEmbedProgress(len(missingHashes), 0)
	batchSize := e.embedCfg.BatchSize
	if batchSize < 1 {
		// 程序化构造 Options 传 0 的防呆(L5):env 路径有下限校验,
		// 此处钳位防批次切分死循环;与下方 workers 钳位同一口径。
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

	// 4) 对齐 records 组装行集（同 embedKey 多行共享同一向量值;Entry
	// 的 ContentHash 字段自本版本起承载 embedKey——子树按模板版本平行
	// 隔离,单一子树内键语义恒一致）。
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
	return out, nil
}
