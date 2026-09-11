package localengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/vector"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// 本文件是离线批量嵌入工具在引擎侧的入口。工具流程是：导出待嵌任务、
// 提交 provider 批作业、轮询、下载结果、把向量导入 journal（已付费但尚未
// 进入 revision 的向量暂存文件）。全部待嵌键已覆盖且内容未变化时，之后的
// Sync 可直接复用这些向量；仍有缺口时会调用 provider 补齐。这些方法只由
// 评测程序 openace-bench 调用，不进 MCP 工具面，也不
// 改变引擎契约。任务键与送审文本由引擎的 embedKey 与 embedDocText 生成，
// 工具侧不自行拼装，两处实现不会分叉。

// EmbedJob 是一条待嵌任务：Key 是 embedKey，作为批作业里的 custom_id；
// Text 与在线构建送给 provider 的文本逐字节一致。
type EmbedJob struct {
	Key  string
	Text string
}

// EmbedPlan 是一次计划的摘要，供费用预估与结果核对。
type EmbedPlan struct {
	// TotalChunks 是存活 chunk 总数，按 embedKey 去重之前。
	TotalChunks int
	// UniqueKeys 是唯一 embedKey 数。
	UniqueKeys int
	// Reusable 是已有向量可复用的键数，来源为 active 与 previous revision、
	// 兼容的旧 chunk profile 子树、journal，与在线构建 embedRecords 的
	// 复用判定一致。
	Reusable int
	// CrossProfileReusable 是 Reusable 中来自旧 chunk profile 子树的份额，
	// 与 engine.Result.CrossProfileReused 同一算法；当前子树为空却零缺口
	// 的计划由此可以解释。
	CrossProfileReusable int
	// Rejected 是持久化拒绝集内的键数：provider 曾对这些内容返回零向量或
	// NaN，不再送 provider。
	Rejected int
	// Pending 是本次导出的待嵌任务数，等于回调次数。
	Pending int
	// Dimension、Model、StoreProfile 供批作业参数与索引子树核对。
	Dimension    int
	Model        string
	StoreProfile string
}

// ImportReport 是一次导入的结果摘要。
type ImportReport struct {
	// Appended 是通过检查并纳入本次追加的向量数。成功返回时均已写入
	// journal；出错返回时可能包含当前批中尚未持久化的向量。
	Appended int
	// Existing 是 journal 已有同键而跳过的条数。
	Existing int
	// BadVector 是归一化失败（零向量或 NaN）而跳过的条数。这些键不进持久化
	// 拒绝集：拒绝集表示内容本身有问题，而批量结果异常可能出在传输或
	// 服务端，写进拒绝集会让之后的实时路径也不再为它们嵌入。
	BadVector int
	// WrongDim 是维度与配置不符而跳过的条数。
	WrongDim int
	// UnknownKey 是不属于当前工作区合法 embedKey 集而被拒的条数。错误
	// 归属的向量一旦进入 journal，该 chunk 的正确嵌入会一直被跳过（journal
	// 命中即不再送 provider），垃圾键也会一直留在 journal 里占用容量。
	// 合法集由 legalEmbedKeys 按当前工作区扫描与切分算出。
	UnknownKey int
}

// ErrSemanticRequired 表示批量工具在未配置 embedding provider 时被拒绝：
// 索引子树由 embedding 身份决定，没有它就没有可写入的目标，静默运行只会
// 白做。导出给 openace-bench 的 -sync-only 预检用：收到该错误说明是纯词法
// 配置，同步不会产生 provider 费用，直接放行。
var ErrSemanticRequired = errors.New("batch 工具需要已配置的 embedding provider(storeProfile 子树由 embedding profile 决定)")

// PlanEmbedJobs 枚举工作区当前状态下的待嵌任务，对每条 pending 任务回调
// fn。扫描、切分与 previous 的 chunk 记录复用走在线构建的同一批函数
// （chunkAsset、loadLiveChunkRecordsByFile），复用来源的判定顺序（active
// 与 previous revision、兼容的旧 chunk profile 子树、journal、拒绝集）与
// embedRecords 一致；不调用 provider，不发布 revision。持跨进程写锁执行，
// 并发构建改写 journal 会让计划与实际不符。
func (e *Engine) PlanEmbedJobs(ctx context.Context, ref engine.WorkspaceRef, fn func(EmbedJob) error) (EmbedPlan, error) {
	if err := rejectProfileID(ref); err != nil {
		return EmbedPlan{}, err
	}
	if !e.semanticEnabled() {
		return EmbedPlan{}, ErrSemanticRequired
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
	previous, records, err := e.planLiveRecords(ctx, workspaceKey, root, store)
	if err != nil {
		return EmbedPlan{}, err
	}
	plan.TotalChunks = len(records)
	if err := e.tallyEmbedPlan(ctx, &plan, store, root, workspaceKey, previous, records, fn); err != nil {
		return EmbedPlan{}, err
	}
	return plan, nil
}

// planLiveRecords 枚举工作区当前状态的存活 chunk 记录，步骤与 runBuild 的
// 扫描与切分阶段相同：扫描工作区，内容身份未变的文件复用 previous 的
// chunk 记录，其余文件重新切分；内容检查拒绝或切出 0 chunk 的文件跳过。
// 不调用 provider。
func (e *Engine) planLiveRecords(ctx context.Context, workspaceKey string, root pathutil.WorkspaceRoot, store *index.Store) (*index.Manifest, []chunkRecord, error) {
	assets, _, err := workspace.FileAssetSource{Cache: e.statCacheFor(workspaceKey)}.LoadWithStats(ctx, root.CanonicalPath)
	if err != nil {
		return nil, nil, fmt.Errorf("扫描工作区: %w", err)
	}
	previous, _, resolveErr := store.ResolveUsable()
	if resolveErr != nil && !isNoRevision(resolveErr) {
		return nil, nil, resolveErr
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
			return nil, nil, err
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
				return nil, nil, err
			}
			if skipped || len(fileRecords) == 0 {
				continue
			}
		}
		records = append(records, fileRecords...)
	}
	return previous, records, nil
}

// tallyEmbedPlan 按 embedKey 去重后把每个键归类计数：依次查 active 与
// previous revision 的向量、兼容的旧 chunk profile 子树的向量、journal、
// 持久化拒绝集，顺序与 embedRecords 一致；都没命中的键是 pending，回调 fn
// 导出待嵌任务。
func (e *Engine) tallyEmbedPlan(ctx context.Context, plan *EmbedPlan, store *index.Store, root pathutil.WorkspaceRoot, workspaceKey string, previous *index.Manifest, records []chunkRecord, fn func(EmbedJob) error) error {
	var prior priorVectors
	defer func() { prior.release() }()
	if previous != nil {
		prior = e.loadPriorVectors(store, previous, nil)
	}
	// 并入旧 chunk profile 子树向量的条件必须与 buildFull 逐字一致：首建、
	// 语义未完整或 active 段物理不全时并入，语义完整的 revision 不并入。
	// 改动前计划只看当前子树：二进制的 chunk profile 已升到 v8 而缓存由 v7
	// 建成时，v8 子树为空，计划报 57,650 个 chunk 缺向量，-sync-only 预检
	// 据此拒绝执行；而同样状态下在线构建经 mergeSiblingProfileVectors 从
	// v7 子树按键复用了全部向量，真实缺口为 0。计划少算复用来源就是假的
	// 缺口，用户会放弃零费路径或以为需要重新付费。候选发现与身份匹配
	// 复用 profile_reuse.go 的实现；并入的索引归 prior 持有，随 defer 统一
	// 释放。
	if previous == nil || !previous.SemanticComplete() || prior.activeLoadedSegments != prior.activeExpectedSegments {
		e.mergeSiblingProfileVectors(store, root, &prior)
	}
	journal, err := e.journalFor(workspaceKey, store)
	if err != nil {
		return fmt.Errorf("打开 embedding journal: %w", err)
	}
	journalKeys := journal.Snapshot()
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
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
		// 旧子树向量的优先级低于 active 与 previous、高于 journal，与
		// embedRecords 填充复用表的顺序一致；CrossProfileReusable 只计
		// 前两级没有命中的键。
		if _, ok := prior.crossProfileByHash[key]; ok {
			plan.Reusable++
			plan.CrossProfileReusable++
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
			return err
		}
	}
	return nil
}

// legalEmbedKeys 枚举当前工作区全部合法的 embedKey，供 ImportEmbeddings 校验
// 键归属。枚举方式与 planLiveRecords 相同：扫描工作区，内容未变的文件复用
// 上一 revision 的 chunk 记录，变更文件现切；扫描不带 stat 缓存。
func (e *Engine) legalEmbedKeys(ctx context.Context, root pathutil.WorkspaceRoot, store *index.Store) (map[string]bool, error) {
	assets, err := workspace.FileAssetSource{}.Load(ctx, root.CanonicalPath)
	if err != nil {
		return nil, fmt.Errorf("扫描工作区: %w", err)
	}
	previous, _, resolveErr := store.ResolveUsable()
	if resolveErr != nil && !isNoRevision(resolveErr) {
		return nil, resolveErr
	}
	previousChunks := map[string][]chunkRecord{}
	if previous != nil {
		if loaded, err := loadLiveChunkRecordsByFile(store, previous); err == nil {
			previousChunks = loaded
		}
	}
	legal := make(map[string]bool, len(assets)*8)
	for _, asset := range assets {
		if err := ctx.Err(); err != nil {
			return nil, err
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
				return nil, err
			}
			if skipped {
				continue
			}
		}
		for _, record := range fileRecords {
			legal[embedKey(record)] = true
		}
	}
	return legal, nil
}

// ImportEmbeddings 把离线批量嵌入的结果写入 journal。next 每次返回一条
// （键、向量），ok=false 表示流结束。每条依次检查：键不在当前工作区的
// 合法 embedKey 集内则拒绝；journal 或本批已有同键则跳过；维度与配置不符
// 则跳过；L2 归一化失败（零向量或 NaN）则跳过，归一化与在线路径一致，
// vector.Write 拒绝未归一化的输入。通过的向量每 512 条批量追加进 journal。
// 导入后 Sync 复用有效向量；只有全部待嵌键已覆盖且内容未变化时，才不需
// 调用 provider。发布后 CompactAfterPublish 清理已入盘的条目。出错时
// 返回已统计的计数，其中 Appended 可能包含尚未持久化的当前批。
func (e *Engine) ImportEmbeddings(ctx context.Context, ref engine.WorkspaceRef, next func() (key string, vec []float32, ok bool, err error)) (ImportReport, error) {
	if err := rejectProfileID(ref); err != nil {
		return ImportReport{}, err
	}
	if !e.semanticEnabled() {
		return ImportReport{}, ErrSemanticRequired
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
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
	legal, err := e.legalEmbedKeys(ctx, root, store)
	if err != nil {
		return ImportReport{}, err
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
		if !legal[key] {
			report.UnknownKey++
			continue
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

// EmbeddingIdentity 返回当前 embedding 身份的三元组：ProfileHash、模型、
// 维度。openace-bench 导入前用它核对结果文件头部记录的身份，另一套模型
// 或维度产出的向量不会被写进本子树的 journal。
func (e *Engine) EmbeddingIdentity() (profileHash string, model string, dimension int) {
	return e.embedCfg.ProfileHash(), e.embedCfg.Model, e.embedCfg.Dimension
}
