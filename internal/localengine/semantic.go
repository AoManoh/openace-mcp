package localengine

import (
	"context"
	"errors"

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

// embedRecords 组装本次 revision 的向量集：先按纯 content hash 复用
// active/previous 两跳的已有向量（阶段计划 D2，bit 级拷贝保证跨 revision
// 一致），再对缺失的唯一内容批量调用 provider。provider 失败不阻塞构建
// （§10.2 lexical freshness 优先），仅 ctx 取消返回错误。
func (e *Engine) embedRecords(ctx context.Context, store *index.Store, previous *index.Manifest, records []chunkRecord, status *wsStatus) (semanticOutcome, error) {
	out := semanticOutcome{enabled: e.semanticEnabled()}
	if !out.enabled || len(records) == 0 {
		return out, nil
	}
	status.setStage(engine.IndexStageEmbedding)
	dimension := e.embedCfg.Dimension

	// 1) 复用：active（hop 0）与其 previous（hop 1）。GC 保留恰好这两个
	// revision；损坏的向量文件被跳过，由后续新嵌入补齐（K25 自愈路径）。
	reuse := make(map[string][]float32)
	activeUsable := make(map[string]bool)
	manifest := previous
	for hop := 0; manifest != nil && hop < 2; hop++ {
		if manifest.HasVectors() {
			ix, err := vector.Load(store.SegmentPath(manifest), dimension,
				manifest.VectorsChecksum, manifest.VectorsIndexChecksum, 0)
			if err == nil {
				for i, entry := range ix.Entries() {
					if _, ok := reuse[entry.ContentHash]; !ok {
						reuse[entry.ContentHash] = ix.Row(i)
						if hop == 0 {
							activeUsable[entry.ContentHash] = true
						}
					}
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

	// 2) 缺失清单：唯一 content hash，按 records 首次出现序（确定性批次；
	// 同内容多处出现只嵌入一次——嵌入输入为纯 chunk 内容，D6）。
	// 本进程已知的零向量内容（K35 拒绝史）不再送 provider，防止 watcher
	// 周期对病理内容反复付费。
	var missingHashes []string
	var missingTexts []string
	seen := make(map[string]bool, len(records))
	e.mu.Lock()
	knownZero := make(map[string]bool, len(e.zeroVecHashes))
	for hash := range e.zeroVecHashes {
		knownZero[hash] = true
	}
	e.mu.Unlock()
	for _, record := range records {
		if seen[record.ContentHash] {
			continue
		}
		seen[record.ContentHash] = true
		if _, ok := reuse[record.ContentHash]; ok {
			continue
		}
		if knownZero[record.ContentHash] {
			out.rejected++
			continue
		}
		missingHashes = append(missingHashes, record.ContentHash)
		missingTexts = append(missingTexts, record.Content)
	}

	// 3) 分批嵌入：单批失败记录并继续（部分成功入盘，D10）；circuit
	// 退避即停（后续批必然拒绝，K30）；取消中止整次构建。
	batchSize := e.embedCfg.BatchSize
	for start := 0; start < len(missingHashes); start += batchSize {
		end := start + batchSize
		if end > len(missingHashes) {
			end = len(missingHashes)
		}
		vectors, err := e.embedClient.EmbedBatch(ctx, missingTexts[start:end], embedding.InputDocument)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.lastError = sanitizeError(err)
			callErr := &reliability.CallError{}
			if errors.As(err, &callErr) && callErr.Class == reliability.ClassBackoff {
				out.backedOff = true
				break
			}
			continue
		}
		for i, vec := range vectors {
			if err := vector.Normalize(vec); err != nil {
				// 零向量/NaN：该内容记为未覆盖并计数（K35），且进程内
				// 不再重试（防重复付费）。
				out.rejected++
				e.mu.Lock()
				e.zeroVecHashes[missingHashes[start+i]] = true
				e.mu.Unlock()
				continue
			}
			reuse[missingHashes[start+i]] = vec
			out.newlyEmbedded++
		}
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
