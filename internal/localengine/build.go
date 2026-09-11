package localengine

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"context"

	"github.com/AoManoh/openace-mcp/internal/chunk"
	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/vector"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// chunkRecord 是 segment 内 chunks.jsonl 的行格式。ContentHash 是纯内容
// hash、不含行号：它参与向量复用键 embedKey，文件内行号漂移不触发重新
// 嵌入付费。
type chunkRecord struct {
	ID          string `json:"id"`
	RelPath     string `json:"path"`
	Language    string `json:"language"`
	Capability  string `json:"capability"`
	StartLine   int    `json:"start"`
	EndLine     int    `json:"end"`
	Symbol      string `json:"symbol,omitempty"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
}

// compaction（把全部 segment 合并为一个、物理丢弃死 chunk 的全量构建）的
// 触发阈值：segment 数或死 chunk 占比任一达到阈值，下一次有内容变更的
// 构建走全量路径。已有向量按键复用，合并本身不调用 provider；构建中
// 新增或仍缺向量的内容继续按正常嵌入流程处理。
// 阈值是固定常数，不提供环境变量。
const (
	compactSegmentThreshold = 8
	compactGarbageRatio     = 0.5
)

// garbageRatio 计算 revision 中死 chunk 占全部 segment chunk 的比例。死 chunk
// 指被同一文件的新版本覆盖的旧 chunk，以及所属文件已进 tombstone（记录
// 已删除或改名文件路径的列表，查询时过滤其旧 chunk）的 chunk。
func garbageRatio(manifest *index.Manifest) float64 {
	total := 0
	for _, segment := range manifest.Segments {
		total += segment.Counts.Chunks
	}
	if total == 0 {
		return 0
	}
	dead := total - manifest.Counts.Chunks
	if dead <= 0 {
		return 0
	}
	return float64(dead) / float64(total)
}

// projectedGarbageRatio 预估在 previous 之上做 delta 发布后的死 chunk 占比：
// 死量 = previous 已有的死 chunk + 本次被删除文件（含被新的忽略规则排除的
// 文件）的全部 chunk，分母仍是既有 segment 的总 chunk 数。
//
// 按发布后的结果而不是按 previous 判断的原因：一次 .openaceignore 变更让
// 57% 已索引文件出局，该次构建没有任何新增 chunk，按 previous 算垃圾比为
// 0 于是走 delta，发布出旧单段加 33,009 条 tombstone 的 revision（垃圾
// 57.1%）却不触发 compaction；查询按段取前 N 再过滤死 chunk 且不回补，
// 有效召回深度随之缩水，直到某次内容变更才碰巧合并。纯删除构建不新增
// 段，本预估即精确结果；removed 为空时与 garbageRatio(previous) 相等，
// 原有判断不变。变更文件的旧版 chunk 数在切分前还没有算出，一律不计入，
// 与原有算法一致。
func projectedGarbageRatio(previous *index.Manifest, removed []string) float64 {
	total := 0
	for _, segment := range previous.Segments {
		total += segment.Counts.Chunks
	}
	if total == 0 {
		return 0
	}
	live := previous.Counts.Chunks
	for _, path := range removed {
		live -= previous.Files[path].ChunkCount
	}
	dead := total - live
	if dead <= 0 {
		return 0
	}
	return float64(dead) / float64(total)
}

// runBuild 执行一次索引构建，在两条路径间选择：delta（只为变更文件产出
// 新 segment，成本与变更量成正比）与 full（首建、损坏修复、语义补齐与
// compaction 的全量构建）。未发布的数据写入 staging 临时目录，失败或取消
// 时丢弃。首建可能先发布词法中间版本，该版本与已追加的 journal 不随
// 后续嵌入失败撤销，下次同步可继续复用。
//   - 先拿跨进程写锁：构建、GC、journal（已付费但尚未进入 revision 的向量
//     暂存文件）都是写路径，必须独占。
//   - 扫描：复用 workspace 包的文件选择与忽略规则，不另建第二套扫描器。
//   - 内容未变时探测词法索引是否可用（needsLexicalRebuild）。词法可用且
//     没有向量修复请求时：语义已满足（或未配置）就直接返回现有 revision，
//     不发布；provider 熔断器（连续失败后暂停向它发请求）在退避期也直接
//     返回，覆盖缺口留在状态里。
//   - 选路：delta 只用于 manifest v2 前身之上的内容变更，且没有词法或向量
//     修复请求、segment 数与预估垃圾比都低于 compaction 阈值；其余全走
//     全量路径，成因经 fullBuildReason 写进 Result.BuildMode。
func (e *Engine) runBuild(ctx context.Context, root pathutil.WorkspaceRoot, workspaceKey string) (result engine.Result, err error) {
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return engine.Result{}, err
	}
	status := e.statusFor(root, workspaceKey)
	status.begin()
	defer func() {
		if err != nil {
			status.fail(err)
		}
	}()
	// 构建结束时清空 tree-sitter 的 arena 池：池内 arena 是强引用，GC 不
	// 回收，批量切分结束后释放；查询路径不用解析器，不受影响。
	defer chunk.DrainParserPools()

	// 跨进程写锁：构建、GC、journal 都是写路径，必须独占；查询只读不经
	// 此处。锁在引擎生命周期内持有并跨构建复用。
	if _, err := e.acquireWriteLock(workspaceKey, store); err != nil {
		return engine.Result{}, err
	}

	// 阶段 1：扫描。复用 workspace 包的文件选择与忽略规则，不另建第二套
	// 扫描器，敏感文件不会绕过 AssetPolicy 进入索引。
	status.setStage(engine.IndexStageScanning)
	// 扫描期按进度回填文件数，大仓首扫时状态不再长期显示 files=0。
	assets, scanStats, err := workspace.FileAssetSource{Cache: e.statCacheFor(workspaceKey), Progress: status.setScannedFiles}.LoadWithStats(ctx, root.CanonicalPath)
	if err != nil {
		return engine.Result{}, fmt.Errorf("扫描工作区: %w", err)
	}
	// 因无读权限或超过大小上限被跳过的文件数进入状态。
	status.setPermissionSkipped(scanStats.PermissionSkippedFiles)
	status.setOversizeSkipped(scanStats.OversizeSkippedFiles)
	status.setScannedFiles(len(assets))

	previous, _, resolveErr := store.ResolveUsable()
	if resolveErr != nil && !isNoRevision(resolveErr) {
		return engine.Result{}, resolveErr
	}
	contentChanged := previous == nil || assetsChanged(assets, previous)
	repairRequested := e.consumeVectorRepair(workspaceKey)
	// needsLexicalRebuild：内容未变，但词法索引打不开，或实际能打开的
	// revision 落后于 manifest 认可的 revision（manifest 完好、Bleve 段
	// 损坏），需要全量重建来修复；否则之后每次检索都失败而状态仍报 ready。
	needsLexicalRebuild := false
	if previous != nil && !contentChanged {
		if handle, probeErr := e.acquireHandle(workspaceKey); probeErr == nil {
			sameRevision := handle.manifest.Revision == previous.Revision
			e.releaseHandle(handle)
			if !sameRevision {
				needsLexicalRebuild = true
			} else if !repairRequested {
				// 无变化且词法可用：语义已满足（或未配置）即不发布，返回现有
				// revision；provider 熔断器在退避期同样直接返回，覆盖缺口留在
				// 状态里，避免 watcher 每个周期都触发一次注定失败的重建。
				semanticSatisfied := !e.semanticEnabled() || previous.SemanticComplete()
				circuitBackoff := e.semanticEnabled() && e.embedClient.CircuitSnapshot().State == "backoff"
				if semanticSatisfied || circuitBackoff {
					status.ready(previous, revisionCount(store, previous))
					return engine.Result{
						Engine:        EngineID,
						IndexRevision: previous.Revision,
						FileCount:     previous.Counts.Files,
					}, nil
				}
			}
		} else {
			needsLexicalRebuild = true
		}
	}

	// 选路：delta 只用于 manifest v2 前身之上的内容变更；首建、词法或向量
	// 修复、语义补齐与 compaction 走全量路径。垃圾比按本次 delta 发布后的
	// 结果预估（含被删除文件的 chunk），纯删除构建越阈同样合并；只看
	// previous 会让它一直不触发（见 projectedGarbageRatio）。
	garbage := 0.0
	if previous != nil {
		_, removed, _ := diffDeltaAssets(previous, assets)
		garbage = projectedGarbageRatio(previous, removed)
	}
	useDelta := previous != nil && contentChanged &&
		previous.SchemaVersion == index.ManifestSchemaV2 &&
		!needsLexicalRebuild && !repairRequested &&
		len(previous.Segments) < compactSegmentThreshold &&
		garbage < compactGarbageRatio
	if useDelta {
		return e.buildDelta(ctx, store, status, root, workspaceKey, previous, assets)
	}
	return e.buildFull(ctx, store, status, root, workspaceKey, previous, assets, contentChanged, needsLexicalRebuild,
		fullBuildReason(previous, contentChanged, needsLexicalRebuild, repairRequested, garbage))
}

// publishLexicalInterim 在首次构建的嵌入开始前发布一个只有词法索引的中间
// revision，与最终发布共用 staging、manifest 与原子发布机制。向量文件为
// 空集，这是合法状态：向量索引载入后 Count=0，查询的向量路按覆盖为空
// 直接跳过，不会登记修复。任何一步失败都返回 nil 且不影响主构建：中间
// 发布只是让仓库更早可检索，最终发布才是正确性路径。发布前复验写锁，
// 失锁即放弃。
func (e *Engine) publishLexicalInterim(ctx context.Context, store *index.Store, status *wsStatus, root pathutil.WorkspaceRoot, workspaceKey string, records []chunkRecord, files map[string]index.FileEntry, capabilities map[string]string, totalBytes int64) *index.Manifest {
	if err := ctx.Err(); err != nil {
		return nil
	}
	interim := semanticOutcome{enabled: true}
	artifacts, discard, err := e.buildSegmentStaging(ctx, store, records, interim)
	if err != nil {
		return nil
	}
	manifest := e.newManifestSkeleton(root, "rev-"+artifacts.buildID, nil)
	manifest.ChunkerCapabilities = capabilities
	interimFiles := make(map[string]index.FileEntry, len(files))
	for path, entry := range files {
		entry.SegmentID = artifacts.buildID
		interimFiles[path] = entry
	}
	manifest.Files = interimFiles
	manifest.Counts = index.Counts{Files: len(interimFiles), Chunks: len(records), Bytes: totalBytes}
	manifest.Segments = []index.SegmentRef{{
		ID:                   artifacts.buildID,
		ChunksChecksum:       artifacts.chunksChecksum,
		VectorsChecksum:      artifacts.vectorsChecksum,
		VectorsIndexChecksum: artifacts.vectorsIndexChecksum,
		Counts:               manifest.Counts,
	}}
	// 发布前复验写锁：失锁说明所有权已被其他进程接管，放弃中间发布。
	if lock, err := e.acquireWriteLock(workspaceKey, store); err != nil {
		discard()
		return nil
	} else if err := lock.Verify(); err != nil {
		discard()
		return nil
	}
	if err := store.Publish(manifest, artifacts.staging); err != nil {
		discard()
		return nil
	}
	e.retireHandles(workspaceKey, manifest.Revision, "")
	status.publishedInterim(manifest, revisionCount(store, manifest))
	return manifest
}

// fullBuildReason 给出全量路径的成因标签，随 Result.BuildMode 返回给调用
// 方。没有成因时，delta 报表里的异常数字会被解读成升级触发了全量重嵌，
// 调用方只能从进度条反猜。判定顺序与 runBuild 的 useDelta 条件一致；
// garbage 是 runBuild 已算出的结果预估垃圾比，纯删除越阈同样标为
// compaction-garbage。
func fullBuildReason(previous *index.Manifest, contentChanged bool, needsLexicalRebuild bool, repairRequested bool, garbage float64) string {
	switch {
	case previous == nil:
		return "full:first-build"
	case needsLexicalRebuild:
		return "full:lexical-selfheal"
	case repairRequested:
		return "full:vector-repair"
	case previous.SchemaVersion != index.ManifestSchemaV2:
		return "full:schema-upgrade"
	case contentChanged && len(previous.Segments) >= compactSegmentThreshold:
		return "full:compaction-segments"
	case contentChanged && garbage >= compactGarbageRatio:
		return "full:compaction-garbage"
	case !contentChanged:
		return "full:semantic-fill"
	}
	return "full"
}

// chunkAsset 读取并切分单个文件。skipped=true 表示内容检查拒绝（二进制、
// 非 UTF-8、超过大小上限）或文件在扫描后已消失：按变更中的文件跳过并
// 计数，留给下一次同步处理，不中断整次构建。
func (e *Engine) chunkAsset(ctx context.Context, asset workspace.ContextAsset) (fileRecords []chunkRecord, skipped bool, err error) {
	content, ok, readErr := workspace.ReadIndexableContent(ctx, asset.AbsPath)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("读取 %s: %w", asset.RelPath, readErr)
	}
	if !ok {
		return nil, true, nil
	}
	// 每种语言的切分能力由各条 record 经 mergeCapability 合并得出，Split
	// 的文件级能力返回值不再使用。
	chunks, _ := e.profile.Split(chunk.File{RelPath: asset.RelPath, Content: string(content)})
	fileRecords = make([]chunkRecord, 0, len(chunks))
	for _, c := range chunks {
		fileRecords = append(fileRecords, chunkRecord{
			ID: c.ID, RelPath: c.RelPath, Language: c.Language,
			Capability: string(c.Capability), StartLine: c.StartLine, EndLine: c.EndLine,
			Symbol: c.SymbolHint, Content: c.Content, ContentHash: c.ContentHash,
		})
	}
	return fileRecords, false, nil
}

// segmentArtifacts 是一次 staging 构建的产物标识（buildID 兼作 segment ID
// 与 revision 后缀）、staging 路径与各文件的校验和。
type segmentArtifacts struct {
	buildID              string
	staging              string
	chunksChecksum       string
	vectorsChecksum      string
	vectorsIndexChecksum string
}

// buildSegmentStaging 在 staging 目录内产出一个 segment：chunks.jsonl、
// Bleve 索引，配置了 embedding 时再写向量文件。任一步失败或 ctx 取消都
// 丢弃 staging；成功时返回 discard 供调用方在发布失败时清理。
func (e *Engine) buildSegmentStaging(ctx context.Context, store *index.Store, records []chunkRecord, seman semanticOutcome) (segmentArtifacts, func(), error) {
	buildID := index.NewBuildID()
	staging, err := store.BeginStaging(buildID)
	if err != nil {
		return segmentArtifacts{}, nil, err
	}
	discard := func() { _ = store.DiscardStaging(buildID) }

	chunksPath := filepath.Join(staging, index.ChunksFileName)
	if err := writeChunkRecords(chunksPath, records); err != nil {
		discard()
		return segmentArtifacts{}, nil, fmt.Errorf("写入 chunk 数据: %w", err)
	}
	docs := make([]lexical.Doc, 0, len(records))
	for _, record := range records {
		docs = append(docs, lexical.Doc{
			ID: record.ID, Path: record.RelPath, Symbol: record.Symbol,
			Language: record.Language, Content: record.Content,
		})
	}
	if err := lexical.Build(ctx, filepath.Join(staging, index.LexicalDirName), docs); err != nil {
		discard()
		return segmentArtifacts{}, nil, fmt.Errorf("构建词法索引: %w", err)
	}
	artifacts := segmentArtifacts{buildID: buildID, staging: staging}
	// 配置了 embedding 时向量文件一定写入，空集也合法：空仓库或零覆盖的
	// revision 同样可以发布。
	if seman.enabled {
		artifacts.vectorsChecksum, artifacts.vectorsIndexChecksum, err =
			vector.Write(staging, e.embedCfg.Dimension, seman.entries, seman.vectors)
		if err != nil {
			discard()
			return segmentArtifacts{}, nil, fmt.Errorf("写入向量数据: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		discard()
		return segmentArtifacts{}, nil, err
	}
	artifacts.chunksChecksum, err = index.ChecksumFile(chunksPath)
	if err != nil {
		discard()
		return segmentArtifacts{}, nil, err
	}
	return artifacts, discard, nil
}

// newManifestSkeleton 组装 manifest v2 的公共字段：工作区身份、引擎与切分
// 器版本、词法引擎版本、时间戳；有 previous 时记录前一 revision；配置了
// embedding 时记录 provider、模型、维度、dtype 与 ProfileHash。
func (e *Engine) newManifestSkeleton(root pathutil.WorkspaceRoot, revision string, previous *index.Manifest) *index.Manifest {
	now := time.Now().UTC()
	manifest := &index.Manifest{
		SchemaVersion: index.ManifestSchemaV2,
		Workspace: index.WorkspaceIdentity{
			CanonicalPath: root.CanonicalPath,
			PathKind:      string(root.PathKind),
			HostOS:        root.HostOS,
		},
		EngineID:       EngineID,
		EngineVersion:  EngineVersion,
		Revision:       revision,
		PolicyHash:     policyHash,
		ChunkerID:      e.profile.ID,
		ChunkerVersion: e.profile.Version,
		LexicalEngine:  lexical.EngineName,
		LexicalVersion: lexical.EngineVersion,
		CreatedAt:      now,
		ActivatedAt:    now,
	}
	if previous != nil {
		manifest.PreviousRevision = previous.Revision
	}
	if e.semanticEnabled() {
		manifest.EmbeddingProvider = e.embedCfg.ProviderType
		manifest.EmbeddingModel = e.embedCfg.Model
		manifest.EmbeddingDimension = e.embedCfg.Dimension
		manifest.EmbeddingDtype = embedding.Dtype
		manifest.EmbeddingProfileHash = e.embedCfg.ProfileHash()
	}
	return manifest
}

// finishPublish 发布 manifest 并收尾：
//   - 发布前复验写锁：失锁说明所有权已被其他进程接管，本次构建作废并
//     丢弃 staging。
//   - 发布后把旧 revision 的句柄标记退役，回收 active 与 previous 之外的
//     旧 revision。
//   - 已随 revision 入盘的向量从 journal 清除：向量事实源是 revision，
//     journal 只在已付费未发布的窗口存在。清除失败不影响发布，条目留到
//     下次发布再清。
//   - Result.Added 是本轮实际写入 segment 的 chunk 数；BuildMode 写明构建
//     形态与成因；配置了 embedding 时携带覆盖率，覆盖不完整给出降级原因
//     semantic-coverage-partial（与查询路径同名），同步成功也不隐藏缺口。
func (e *Engine) finishPublish(store *index.Store, status *wsStatus, workspaceKey string, manifest *index.Manifest, staging string, discard func(), seman semanticOutcome, skippedFiles int, added int, buildMode string) (engine.Result, error) {
	// 发布前复验写锁：失锁说明所有权已被其他进程接管，本次构建作废。
	if lock, err := e.acquireWriteLock(workspaceKey, store); err != nil {
		if discard != nil {
			discard()
		}
		return engine.Result{}, err
	} else if err := lock.Verify(); err != nil {
		if discard != nil {
			discard()
		}
		return engine.Result{}, err
	}
	if err := store.Publish(manifest, staging); err != nil {
		if discard != nil {
			discard()
		}
		return engine.Result{}, fmt.Errorf("发布索引: %w", err)
	}
	e.retireHandles(workspaceKey, manifest.Revision, manifest.PreviousRevision)
	e.gcRevisions(store, status, workspaceKey, manifest.Revision, manifest.PreviousRevision)
	// 已随 revision 入盘的向量从 journal 清除；失败不影响发布，条目留到
	// 下次发布再清。
	if seman.enabled && len(seman.entries) > 0 {
		if journal, journalErr := e.journalFor(workspaceKey, store); journalErr == nil {
			published := make(map[string]bool, len(seman.entries))
			for _, entry := range seman.entries {
				published[entry.ContentHash] = true
			}
			_ = journal.CompactAfterPublish(published)
		}
	}
	status.setSkippedFiles(skippedFiles)
	status.setSemanticOutcome(seman.rejected, seman.lastError)
	status.ready(manifest, revisionCount(store, manifest))
	// Added 是本轮实际写入 segment 的 chunk 数。改动前统一取
	// manifest.Counts.Chunks，delta 构建被报成 revision 总量：1975 个文件的
	// 仓库做一次小改动显示 Added=19028，被解读成升级触发了全量重嵌。
	// BuildMode 写明构建形态与成因。
	result := engine.Result{
		Engine:             EngineID,
		IndexRevision:      manifest.Revision,
		FileCount:          manifest.Counts.Files,
		Uploaded:           0,
		Added:              added,
		BuildMode:          buildMode,
		CrossProfileReused: seman.crossProfileReused,
	}
	// 同步成功也不隐藏语义覆盖缺口：结果携带覆盖率，覆盖不完整给出与
	// 查询路径同名的降级原因，Summary 里可见。
	if e.semanticEnabled() {
		result.SemanticCoverage = coveragePercent(manifest)
		if !manifest.SemanticComplete() {
			result.DegradedReason = "semantic-coverage-partial"
		}
	}
	return result, nil
}

// coveredPerFile 统计一个文件有向量的 chunk 数。查找键是 embedKey，与
// seman.entries 的键一致；覆盖率的分子分母都按存活 chunk 计，复用来的
// 向量与新嵌入的同样计入。
func coveredPerFile(fileRecords []chunkRecord, coveredHash map[string]bool) int {
	covered := 0
	for _, record := range fileRecords {
		if coveredHash[embedKey(record)] {
			covered++
		}
	}
	return covered
}

// buildFull 是全量构建：首建、词法或向量损坏修复、语义补齐与 compaction
// 都走这里，产出单 segment 的 revision。未变文件的 chunk 记录与向量全部
// 本地复用，provider 只为真正缺向量的内容调用。
//   - 切分：与 previous 内容身份一致的文件直接复用其 chunk 记录，其余重新
//     切分。内容检查拒绝或切出 0 chunk 的文件计入 skippedFiles 且不进
//     manifest。
//   - 首建且配置了 embedding：嵌入开始前先发布只有词法索引的中间
//     revision，嵌入结束后按实际取得的向量发布；覆盖不足时显式报告缺口。
//   - 装载既有向量：active 与 previous revision 的向量优先；只在首建、语义
//     未完整或 active 段物理不全时并入同工作区旧 chunk profile 子树的
//     向量。
//   - 内容未变、词法可用且向量没有实质增加时不发布新 revision，返回现有
//     revision，缺口留在状态里，避免堆积没有变化的 revision。
//   - 其余情况写 staging、组装单段 manifest v2、原子发布。
func (e *Engine) buildFull(ctx context.Context, store *index.Store, status *wsStatus, root pathutil.WorkspaceRoot, workspaceKey string, previous *index.Manifest, assets workspace.AssetSet, contentChanged bool, needsLexicalRebuild bool, buildMode string) (engine.Result, error) {
	var previousChunks map[string][]chunkRecord
	if previous != nil {
		var err error
		previousChunks, err = loadLiveChunkRecordsByFile(store, previous)
		if err != nil {
			// previous 的 chunk 记录读不出来时全部重新切分，不中断本次构建。
			previousChunks = nil
		}
	}

	// 阶段 2：切分。未变化的文件直接复用上一 revision 的 chunk 记录。
	status.setStage(engine.IndexStageChunking)
	records := make([]chunkRecord, 0, len(assets)*8)
	recordsByFile := make(map[string][]chunkRecord, len(assets))
	files := make(map[string]index.FileEntry, len(assets))
	capabilities := map[string]string{}
	// totalBytes 是已索引 chunk 内容的字节数，复用与新切分的文件按同一方式
	// 累计。
	var totalBytes int64
	// skippedFiles 统计扫描通过但被内容检查拒绝（二进制、非 UTF-8、超过
	// 大小上限）或切出 0 chunk 的文件，原样进入状态上报。
	skippedFiles := 0
	for _, asset := range assets {
		if err := ctx.Err(); err != nil {
			return engine.Result{}, err
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
			var err error
			fileRecords, skipped, err = e.chunkAsset(ctx, asset)
			if err != nil {
				return engine.Result{}, err
			}
			// 切出 0 chunk 的文件与内容检查拒绝的同样跳过。全量单段路径
			// 没有旧 chunk 复活的风险，但 ChunkCount 为 0 的 Files 条目是脏
			// 数据，会被之后 delta 与 compaction 的 ContentHash 复用条件
			// 误命中。
			if skipped || len(fileRecords) == 0 {
				skippedFiles++
				continue
			}
		}
		var fileBytes int64
		for _, record := range fileRecords {
			mergeCapability(capabilities, record.Language, record.Capability)
			fileBytes += int64(len(record.Content))
		}
		totalBytes += fileBytes
		files[asset.RelPath] = index.FileEntry{
			ContentHash: asset.BlobName, ChunkCount: len(fileRecords), Bytes: fileBytes,
		}
		recordsByFile[asset.RelPath] = fileRecords
		records = append(records, fileRecords...)
	}

	// 首建的词法中间发布。启用语义的仓库若等嵌入结束才发布，在此之前
	// 不可检索，大仓 30 分钟不可用。只在首建（previous==nil）触发：嵌入
	// 开始前先发布只有词法索引的 revision（空向量文件合法，覆盖率 0%，
	// 查询按既有降级语义标记），嵌入结束后按实际向量覆盖发布，缺口显式报告。
	// 词法 revision 成为其 previous，GC 链不变。中间发布失败不影响主构建；
	// 显式 Sync 仍阻塞到最终发布；嵌入中途崩溃时词法 revision 继续可
	// 服务，下次同步按语义补齐路径完成。
	if previous == nil && e.semanticEnabled() && len(records) > 0 && e.lexicalFirst {
		if lexManifest := e.publishLexicalInterim(ctx, store, status, root, workspaceKey, records, files, capabilities, totalBytes); lexManifest != nil {
			previous = lexManifest
		}
	}

	// 阶段 2.5：语义路（为 chunk 取得向量）。未配置 embedding 时 embedRecords
	// 直接返回空产物。
	var prior priorVectors
	// 复用的向量经 mmap 页或堆数据被 prior 的哈希表引用，直到新段写盘
	// 完成；构建返回时统一释放，包括提前返回与错误路径。
	defer func() { prior.release() }()
	if e.semanticEnabled() {
		if previous != nil {
			prior = e.loadPriorVectors(store, previous, nil)
		}
		// 同工作区旧 chunk profile 子树的向量是最低优先级来源，active 与
		// previous 始终优先。语义完整的 revision 不再载入兄弟子树的大向量
		// 集，否则 compaction 期间常驻内存翻倍；只在首建、语义未完整或
		// active 段物理不全时并入。
		if previous == nil || !previous.SemanticComplete() || prior.activeLoadedSegments != prior.activeExpectedSegments {
			e.mergeSiblingProfileVectors(store, root, &prior)
		}
	}
	seman, err := e.embedRecords(ctx, store, workspaceKey, prior, records, status)
	if err != nil {
		return engine.Result{}, err
	}

	// 内容未变、词法可用且向量没有实质增加时不发布新 revision，返回现有
	// revision，缺口留在状态里，避免堆积没有变化的 revision。
	if previous != nil && !contentChanged && !needsLexicalRebuild && !seman.improved() {
		status.setSemanticOutcome(seman.rejected, seman.lastError)
		status.ready(previous, revisionCount(store, previous))
		return engine.Result{
			Engine:        EngineID,
			IndexRevision: previous.Revision,
			FileCount:     previous.Counts.Files,
		}, nil
	}

	// 阶段 3：写 staging。
	status.setStage(engine.IndexStageIndexing)
	artifacts, discard, err := e.buildSegmentStaging(ctx, store, records, seman)
	if err != nil {
		return engine.Result{}, err
	}

	// 阶段 4：组装单段 manifest v2 并原子发布。
	status.setStage(engine.IndexStagePublishing)
	coveredHash := make(map[string]bool, len(seman.entries))
	for _, entry := range seman.entries {
		coveredHash[entry.ContentHash] = true
	}
	for path, entry := range files {
		entry.SegmentID = artifacts.buildID
		if seman.enabled {
			entry.CoveredChunks = coveredPerFile(recordsByFile[path], coveredHash)
		}
		files[path] = entry
	}
	manifest := e.newManifestSkeleton(root, "rev-"+artifacts.buildID, previous)
	manifest.ChunkerCapabilities = capabilities
	manifest.Files = files
	manifest.Counts = index.Counts{Files: len(files), Chunks: len(records), Bytes: totalBytes}
	manifest.Segments = []index.SegmentRef{{
		ID:                   artifacts.buildID,
		ChunksChecksum:       artifacts.chunksChecksum,
		VectorsChecksum:      artifacts.vectorsChecksum,
		VectorsIndexChecksum: artifacts.vectorsIndexChecksum,
		Counts:               manifest.Counts,
		VectorCount:          seman.covered,
	}}
	if seman.enabled {
		manifest.VectorCount = seman.covered
	}
	return e.finishPublish(store, status, workspaceKey, manifest, artifacts.staging, discard, seman, skippedFiles, len(records), buildMode)
}

// buildDelta 是增量构建：只为变更文件切分、嵌入并产出一个 delta segment，
// 删除或改名的文件进 tombstone；未触及文件的 chunk 与向量零工作量。
//   - 变更集全部切出 0 chunk、没有删除、且这些路径本就不在 manifest 里
//     时不发布：manifest 内容不会变化，发布只会制造 revision 更替。
//   - 既有向量按行选读，只读出变更记录需要的键。
//   - 内容检查拒绝的变更文件从存活集移除并进 tombstone，旧版本也不再可
//     检索。
//   - manifest 沿用 previous 的 segment 列表，有记录时追加本次 delta 段；
//     只有删除时 staging 为空，做 manifest-only 发布。
func (e *Engine) buildDelta(ctx context.Context, store *index.Store, status *wsStatus, root pathutil.WorkspaceRoot, workspaceKey string, previous *index.Manifest, assets workspace.AssetSet) (engine.Result, error) {
	status.setStage(engine.IndexStageChunking)
	changed, removed, currentPaths := diffDeltaAssets(previous, assets)
	delta, err := e.chunkChangedAssets(ctx, changed)
	if err != nil {
		return engine.Result{}, err
	}
	records := delta.records
	// 空 delta 不发布：变更集全部切出 0 chunk、没有删除、且这些路径本就
	// 不在 manifest 里时，manifest 内容不会变化，发布只会制造 revision
	// 更替（GC 压力、journal 增长、反复重建）。扫描层已按同一条件剔除
	// 0 chunk 的内容，这里防止将来回归。
	if len(records) == 0 && len(removed) == 0 {
		touchesManifest := false
		for _, asset := range changed {
			if _, ok := previous.Files[asset.RelPath]; ok {
				touchesManifest = true
				break
			}
		}
		if !touchesManifest {
			status.ready(previous, revisionCount(store, previous))
			return engine.Result{
				Engine:        EngineID,
				IndexRevision: previous.Revision,
				FileCount:     previous.Counts.Files,
			}, nil
		}
	}

	// 语义路只嵌入 delta 记录，复用按 embedKey。既有向量按行选读：delta
	// 实际复用的行数约等于变更 chunk 数，不为此整读全部 prior 段（needed
	// 是变更记录的 embedKey 集）。
	var prior priorVectors
	defer func() { prior.release() }()
	if e.semanticEnabled() {
		needed := make(map[string]bool, len(records))
		for _, record := range records {
			needed[embedKey(record)] = true
		}
		prior = e.loadPriorVectors(store, previous, needed)
	}
	seman, err := e.embedRecords(ctx, store, workspaceKey, prior, records, status)
	if err != nil {
		return engine.Result{}, err
	}
	coveredHash := make(map[string]bool, len(seman.entries))
	for _, entry := range seman.entries {
		coveredHash[entry.ContentHash] = true
	}

	// 组装 manifest v2 的 Files、Tombstones 与 Segments。
	status.setStage(engine.IndexStageIndexing)
	files := make(map[string]index.FileEntry, len(previous.Files)+len(changed))
	for path, entry := range previous.Files {
		files[path] = entry
	}
	for _, path := range removed {
		delete(files, path)
	}
	capabilities := make(map[string]string, len(previous.ChunkerCapabilities))
	for language, capability := range previous.ChunkerCapabilities {
		capabilities[language] = capability
	}

	var artifacts segmentArtifacts
	var discard func()
	hasSegment := len(records) > 0
	if hasSegment {
		artifacts, discard, err = e.buildSegmentStaging(ctx, store, records, seman)
		if err != nil {
			return engine.Result{}, err
		}
	}
	for _, asset := range changed {
		if delta.skippedPaths[asset.RelPath] {
			// 内容检查拒绝的变更文件从存活集移除，旧版本也不再可检索。
			delete(files, asset.RelPath)
			continue
		}
		fileRecords := delta.byFile[asset.RelPath]
		var fileBytes int64
		for _, record := range fileRecords {
			mergeCapability(capabilities, record.Language, record.Capability)
			fileBytes += int64(len(record.Content))
		}
		entry := index.FileEntry{
			ContentHash: asset.BlobName, ChunkCount: len(fileRecords), Bytes: fileBytes,
			SegmentID: artifacts.buildID,
		}
		if seman.enabled {
			entry.CoveredChunks = coveredPerFile(fileRecords, coveredHash)
		}
		files[asset.RelPath] = entry
	}

	tombstones := deltaTombstones(previous, currentPaths, removed, delta.skippedPaths)
	counts, vectorCount := sumLiveCounts(files)

	status.setStage(engine.IndexStagePublishing)
	revisionID := index.NewBuildID()
	manifest := e.newManifestSkeleton(root, "rev-"+revisionID, previous)
	manifest.ChunkerCapabilities = capabilities
	manifest.Files = files
	manifest.Tombstones = tombstones
	manifest.Counts = counts
	manifest.Segments = append([]index.SegmentRef{}, previous.Segments...)
	if hasSegment {
		manifest.Segments = append(manifest.Segments, index.SegmentRef{
			ID:                   artifacts.buildID,
			ChunksChecksum:       artifacts.chunksChecksum,
			VectorsChecksum:      artifacts.vectorsChecksum,
			VectorsIndexChecksum: artifacts.vectorsIndexChecksum,
			Counts:               index.Counts{Files: len(delta.byFile), Chunks: len(records)},
			VectorCount:          len(seman.entries),
		})
	}
	if e.semanticEnabled() {
		manifest.VectorCount = vectorCount
	}
	staging := ""
	if hasSegment {
		staging = artifacts.staging
	}
	return e.finishPublish(store, status, workspaceKey, manifest, staging, discard, seman, delta.skippedFiles, len(records), "delta")
}

// diffDeltaAssets 对比 previous manifest 与当前资产集，返回变更文件（新增
// 或内容身份变化）、被删除的路径与当前路径集。
func diffDeltaAssets(previous *index.Manifest, assets workspace.AssetSet) ([]workspace.ContextAsset, []string, map[string]bool) {
	currentPaths := make(map[string]bool, len(assets))
	var changed []workspace.ContextAsset
	for _, asset := range assets {
		currentPaths[asset.RelPath] = true
		entry, ok := previous.Files[asset.RelPath]
		if !ok || entry.ContentHash != asset.BlobName {
			changed = append(changed, asset)
		}
	}
	var removed []string
	for path := range previous.Files {
		if !currentPaths[path] {
			removed = append(removed, path)
		}
	}
	return changed, removed, currentPaths
}

// deltaChunks 是 buildDelta 切分阶段的产物：全部记录、按文件分组的记录、
// 跳过的文件数与路径集。
type deltaChunks struct {
	records      []chunkRecord
	byFile       map[string][]chunkRecord
	skippedFiles int
	skippedPaths map[string]bool
}

// chunkChangedAssets 切分变更文件。切出 0 chunk 的变更文件（纯空白、仅
// 注释等）与内容检查拒绝的文件同样处理：从 Files 摘除并进 tombstone。
// 否则旧 segment 里该文件的 chunk 没有新版本覆盖，继续可检索，而
// compaction 按 ContentHash 复用会把这份过期内容固化下来。
func (e *Engine) chunkChangedAssets(ctx context.Context, changed []workspace.ContextAsset) (deltaChunks, error) {
	delta := deltaChunks{
		records:      make([]chunkRecord, 0, len(changed)*8),
		byFile:       make(map[string][]chunkRecord, len(changed)),
		skippedPaths: make(map[string]bool),
	}
	for _, asset := range changed {
		if err := ctx.Err(); err != nil {
			return deltaChunks{}, err
		}
		fileRecords, skipped, err := e.chunkAsset(ctx, asset)
		if err != nil {
			return deltaChunks{}, err
		}
		if skipped || len(fileRecords) == 0 {
			delta.skippedFiles++
			delta.skippedPaths[asset.RelPath] = true
			continue
		}
		delta.byFile[asset.RelPath] = fileRecords
		delta.records = append(delta.records, fileRecords...)
	}
	return delta, nil
}

// deltaTombstones 计算增量后的 tombstone 集：previous 的 tombstone 中仍不在
// 当前路径集（或本次被拒绝）的路径，加本次删除的路径，加本次被内容检查
// 拒绝的变更文件。重新出现的文件从 tombstone 中移出。排序输出，同样的
// 输入产出同样的 manifest。
func deltaTombstones(previous *index.Manifest, currentPaths map[string]bool, removed []string, skippedPaths map[string]bool) []string {
	tombstoneSet := make(map[string]bool, len(previous.Tombstones)+len(removed))
	for _, path := range previous.Tombstones {
		if !currentPaths[path] || skippedPaths[path] {
			tombstoneSet[path] = true
		}
	}
	for _, path := range removed {
		tombstoneSet[path] = true
	}
	for path := range skippedPaths {
		tombstoneSet[path] = true
	}
	tombstones := make([]string, 0, len(tombstoneSet))
	for path := range tombstoneSet {
		tombstones = append(tombstones, path)
	}
	sort.Strings(tombstones)
	return tombstones
}

// sumLiveCounts 按存活文件累加文件数、chunk 数、字节数与有向量的 chunk
// 数。覆盖率的分子分母都只计存活 chunk，已删除文件的向量不计入，
// 否则覆盖率虚高。
func sumLiveCounts(files map[string]index.FileEntry) (index.Counts, int) {
	counts := index.Counts{Files: len(files)}
	vectorCount := 0
	for _, entry := range files {
		counts.Chunks += entry.ChunkCount
		counts.Bytes += entry.Bytes
		vectorCount += entry.CoveredChunks
	}
	return counts, vectorCount
}

// assetsChanged 判断文件集合或任一文件的内容身份是否与 manifest 不同。
func assetsChanged(assets workspace.AssetSet, manifest *index.Manifest) bool {
	if len(assets) != len(manifest.Files) {
		return true
	}
	for _, asset := range assets {
		entry, ok := manifest.Files[asset.RelPath]
		if !ok || entry.ContentHash != asset.BlobName {
			return true
		}
	}
	return false
}

// mergeCapability 合并一种语言的切分能力：全部 chunk 都是 ast 才记 ast，
// 出现不同取值即记 mixed，状态里能看出有文件回退到了行窗口切分。
func mergeCapability(capabilities map[string]string, language string, capability string) {
	current, ok := capabilities[language]
	if !ok {
		capabilities[language] = capability
		return
	}
	if current != capability {
		capabilities[language] = "mixed"
	}
}

// writeChunkRecords 把 chunk 记录写为 JSONL，写完 fsync 再关闭。文件以
// O_EXCL 创建，staging 目录内不覆盖既有文件。
func writeChunkRecords(path string, records []chunkRecord) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := bufio.NewWriterSize(f, 1<<20)
	encoder := json.NewEncoder(writer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			f.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readSegmentChunkRecords 读取单个 segment 的全部 chunk 记录；单行缓冲上限
// 8 MiB，损坏行按错误返回。
func readSegmentChunkRecords(segmentDir string) ([]chunkRecord, error) {
	f, err := os.Open(filepath.Join(segmentDir, index.ChunksFileName))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []chunkRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record chunkRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("chunk 数据损坏: %w", err)
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

// loadLiveChunkRecordsByFile 读取 revision 的存活 chunk 记录并按文件分组：
// 按 segment 顺序后者覆盖前者（同一文件以最新 segment 的版本为准），再以
// manifest.Files 为存活文件的最终依据，tombstone 文件与被覆盖的旧版本都
// 不出现。
func loadLiveChunkRecordsByFile(store *index.Store, manifest *index.Manifest) (map[string][]chunkRecord, error) {
	byFile := make(map[string][]chunkRecord, len(manifest.Files))
	for _, segment := range manifest.Segments {
		records, err := readSegmentChunkRecords(store.SegmentPathFor(segment.ID))
		if err != nil {
			return nil, err
		}
		segmentByFile := make(map[string][]chunkRecord)
		for _, record := range records {
			segmentByFile[record.RelPath] = append(segmentByFile[record.RelPath], record)
		}
		for path, group := range segmentByFile {
			byFile[path] = group
		}
	}
	for path := range byFile {
		if _, live := manifest.Files[path]; !live {
			delete(byFile, path)
		}
	}
	return byFile, nil
}

// gcRevisions 回收 active 与 previous 之外的旧 revision。仍有打开句柄
// （refs>0）的 revision 跳过，留给下一次 GC；删除失败不影响发布，失败数
// 记入状态的 gc_failed：Windows 上被占用的映射或句柄会阻止删除，静默
// 积累就是磁盘泄漏。多个 revision 共享的 segment 由 store.RemoveRevision
// 按引用判断是否删除。
func (e *Engine) gcRevisions(store *index.Store, status *wsStatus, workspaceKey string, activeRevision string, previousRevision string) {
	revisions, err := store.ListRevisions()
	if err != nil {
		return
	}
	failed := 0
	for _, revision := range revisions {
		if revision == activeRevision || revision == previousRevision {
			continue
		}
		key := handleKey(workspaceKey, revision)
		e.mu.Lock()
		handle, ok := e.handles[key]
		if ok && handle.refs > 0 {
			e.mu.Unlock()
			continue
		}
		if ok {
			delete(e.handles, key)
		}
		e.mu.Unlock()
		if ok {
			_ = handle.lex.Close()
			handle.closeContentFiles()
			handle.releaseVectorIndexes()
		}
		if err := store.RemoveRevision(revision); err != nil {
			failed++
		}
	}
	if status != nil {
		status.setGCFailures(failed)
	}
}

// revisionCount 统计当前保留链上的 revision 数，供状态上报。链遍历带环
// 检测与深度上限（index.MaxRevisionChain），被外部改坏成环的 manifest 不会
// 让遍历挂起。
func revisionCount(store *index.Store, manifest *index.Manifest) int {
	count := 0
	visited := make(map[string]bool)
	revision := manifest.Revision
	for revision != "" && !visited[revision] && count < index.MaxRevisionChain {
		visited[revision] = true
		count++
		m, err := store.LoadManifest(revision)
		if err != nil {
			break
		}
		revision = m.PreviousRevision
	}
	return count
}

func isNoRevision(err error) bool {
	return err == index.ErrNoUsableRevision
}
