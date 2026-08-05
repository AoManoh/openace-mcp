package localengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/vector"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// 本文件是 Batch API 驱动工具的引擎侧钩子(2026-08-05 主线;上游裁决:
// 2026-08-03 batch verdict §2——工具形态"上传/建作业/轮询/下载/回灌
// journal",不进产品面、不改引擎契约)。与 DumpChunkRecords 同类:仅
// openace-bench 消费,不进 MCP 工具面。键与送审文本由引擎单一实现
// (embedKey/embedDocText)导出,工具侧零重组,杜绝复制漂移。

// EmbedJob 是一条待嵌任务:custom_id=Key(embedKey),送审文本与在线
// 路径逐字节一致(a2p 模板)。
type EmbedJob struct {
	Key  string
	Text string
}

// EmbedPlan 是计划摘要(费用预估与对账的事实源)。
type EmbedPlan struct {
	// TotalChunks 是存活 chunk 总数(embedKey 去重前)。
	TotalChunks int
	// UniqueKeys 是唯一 embedKey 数。
	UniqueKeys int
	// Reusable 是已有向量可复用数(active/previous revision ∪ journal)。
	Reusable int
	// Rejected 是持久化拒绝集内的键数(病理内容,不再送 provider)。
	Rejected int
	// Pending 是本次导出的待嵌任务数(= 回调次数)。
	Pending int
	// Dimension/Model/StoreProfile 供批量作业参数与子树对账。
	Dimension    int
	Model        string
	StoreProfile string
}

// ImportReport 是回灌结果摘要。
type ImportReport struct {
	// Appended 是新写入 journal 的向量数。
	Appended int
	// Existing 是 journal 已有同键而跳过的条数。
	Existing int
	// BadVector 是归一失败(零向量/NaN)而跳过的条数——不入持久化拒绝
	// 集:拒绝集语义是"内容病理"(K35),批量结果异常可能是传输/服务面
	// 问题,写入会阻断后续实时路径重嵌。
	BadVector int
	// WrongDim 是维度不符而跳过的条数。
	WrongDim int
}

// errSemanticRequired:批量工具依赖 embedding profile 定位 storeProfile
// 子树,semantic off 时无从谈起,显式拒绝防静默空转白灌。
var errSemanticRequired = errors.New("batch 工具需要已配置的 embedding provider(storeProfile 子树由 embedding profile 决定)")

// PlanEmbedJobs 枚举工作区当前形态下的待嵌任务:与在线构建同源的扫描
// 与切分(含 previous chunk 复用),同源的复用池判定(active/previous
// revision → journal → 拒绝集),零 provider 调用、零 revision 变更。
// 持跨进程写锁执行,防并发构建改写 journal 造成计划失真。
func (e *Engine) PlanEmbedJobs(ctx context.Context, ref engine.WorkspaceRef, fn func(EmbedJob) error) (EmbedPlan, error) {
	if err := rejectProfileID(ref); err != nil {
		return EmbedPlan{}, err
	}
	if !e.semanticEnabled() {
		return EmbedPlan{}, errSemanticRequired
	}
	plan := EmbedPlan{
		Dimension:    e.embedCfg.Dimension,
		Model:        e.embedCfg.Model,
		StoreProfile: e.storeProfile,
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return EmbedPlan{}, err
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return EmbedPlan{}, err
	}
	if _, err := e.acquireWriteLock(workspaceKey, store); err != nil {
		return EmbedPlan{}, err
	}

	// 与 runBuild 阶段 1/2 同源:扫描 → (previous 复用 | 重切分)。
	assets, _, err := workspace.FileAssetSource{Cache: e.statCacheFor(workspaceKey)}.LoadWithStats(ctx, root.CanonicalPath)
	if err != nil {
		return EmbedPlan{}, fmt.Errorf("扫描工作区: %w", err)
	}
	previous, _, resolveErr := store.ResolveUsable()
	if resolveErr != nil && !isNoRevision(resolveErr) {
		return EmbedPlan{}, resolveErr
	}
	var previousChunks map[string][]chunkRecord
	if previous != nil {
		if loaded, err := loadLiveChunkRecordsByFile(store, previous); err == nil {
			previousChunks = loaded
		}
	}
	var records []chunkRecord
	for _, asset := range assets {
		if err := ctx.Err(); err != nil {
			return EmbedPlan{}, err
		}
		var fileRecords []chunkRecord
		reused := false
		if previous != nil {
			if entry, ok := previous.Files[asset.RelPath]; ok && entry.ContentHash == asset.BlobName {
				if reuse, ok := previousChunks[asset.RelPath]; ok {
					fileRecords = reuse
					reused = true
				}
			}
		}
		if !reused {
			var skipped bool
			fileRecords, skipped, err = e.chunkAsset(ctx, asset)
			if err != nil {
				return EmbedPlan{}, err
			}
			if skipped || len(fileRecords) == 0 {
				continue
			}
		}
		records = append(records, fileRecords...)
	}
	plan.TotalChunks = len(records)

	// 复用池与拒绝集(embedRecords 步骤 1/2 同源判定)。
	var prior priorVectors
	if previous != nil {
		prior = e.loadPriorVectors(store, previous)
	}
	journal, err := e.journalFor(workspaceKey, store)
	if err != nil {
		return EmbedPlan{}, fmt.Errorf("打开 embedding journal: %w", err)
	}
	journalKeys := journal.Snapshot()
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return EmbedPlan{}, err
		}
		key := embedKey(record)
		if seen[key] {
			continue
		}
		seen[key] = true
		plan.UniqueKeys++
		if _, ok := prior.activeByHash[key]; ok {
			plan.Reusable++
			continue
		}
		if _, ok := prior.olderByHash[key]; ok {
			plan.Reusable++
			continue
		}
		if _, ok := journalKeys[key]; ok {
			plan.Reusable++
			continue
		}
		if journal.Rejected(key) {
			plan.Rejected++
			continue
		}
		plan.Pending++
		if err := fn(EmbedJob{Key: key, Text: embedDocText(record)}); err != nil {
			return EmbedPlan{}, err
		}
	}
	return plan, nil
}

// ImportEmbeddings 把离线批量结果回灌 journal:逐条校验维度并 L2 归一
// (与在线路径同口径,vector.Write 拒绝未归一输入),journal 已有键跳过。
// next 返回 ok=false 表示流结束。回灌后一次正常 Sync 即零 provider 调用
// 收编发布,发布随即 CompactAfterPublish 清理 journal。
func (e *Engine) ImportEmbeddings(ctx context.Context, ref engine.WorkspaceRef, next func() (key string, vec []float32, ok bool, err error)) (ImportReport, error) {
	if err := rejectProfileID(ref); err != nil {
		return ImportReport{}, err
	}
	if !e.semanticEnabled() {
		return ImportReport{}, errSemanticRequired
	}
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return ImportReport{}, err
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return ImportReport{}, err
	}
	if _, err := e.acquireWriteLock(workspaceKey, store); err != nil {
		return ImportReport{}, err
	}
	journal, err := e.journalFor(workspaceKey, store)
	if err != nil {
		return ImportReport{}, fmt.Errorf("打开 embedding journal: %w", err)
	}

	report := ImportReport{}
	existing := journal.Snapshot()
	const flushEvery = 512
	batch := make(map[string][]float32, flushEvery)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := journal.Append(batch); err != nil {
			return fmt.Errorf("journal 落盘: %w", err)
		}
		for key := range batch {
			existing[key] = nil
			delete(batch, key)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		key, vec, ok, err := next()
		if err != nil {
			return report, err
		}
		if !ok {
			break
		}
		if _, dup := existing[key]; dup {
			report.Existing++
			continue
		}
		if _, dup := batch[key]; dup {
			report.Existing++
			continue
		}
		if len(vec) != e.embedCfg.Dimension {
			report.WrongDim++
			continue
		}
		if err := vector.Normalize(vec); err != nil {
			report.BadVector++
			continue
		}
		batch[key] = vec
		report.Appended++
		if len(batch) >= flushEvery {
			if err := flush(); err != nil {
				return report, err
			}
		}
	}
	if err := flush(); err != nil {
		return report, err
	}
	return report, nil
}
