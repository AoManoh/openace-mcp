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

// chunkRecord 是 chunks.jsonl 的行格式；content_hash 供后续阶段
// 按内容键控派生产物缓存（阶段计划 D4/K5）。
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

// compaction 触发阈值（Stage 4 D3，受测常数）：delta 段数或垃圾 chunk
// 占比任一超限时，下一次构建走全量合并路径（零 provider 调用）。
const (
	compactSegmentThreshold = 8
	compactGarbageRatio     = 0.5
)

// garbageRatio 计算 revision 中死 chunk（被 supersede/tombstone 的旧版本）
// 占全部 segment chunk 的比例。
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

// runBuild 执行一次索引构建：按 D1/D3 在 delta（变更量成本，G1）与
// full（首建/自愈/compaction）两条路径间裁决。全程在 staging 内进行，
// 任何失败/取消都丢弃 staging（暗坑 K2/K16）。
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
	// 构建收尾排空 tree-sitter arena 池(M4c):池内 arena 是强引用,
	// GC 不回收;批量切分结束后排水,查询路径不涉及。
	defer chunk.DrainParserPools()

	// 跨进程写锁（D6/K45）：构建/GC/journal 是写路径，必须独占；
	// 查询只读不经此处。锁在 Engine 生命周期内持有并跨构建复用。
	if _, err := e.acquireWriteLock(workspaceKey, store); err != nil {
		return engine.Result{}, err
	}

	// 阶段 1：发现（复用 workspace 扫描与 AssetPolicy，暗坑 K4）。
	status.setStage(engine.IndexStageScanning)
	assets, scanStats, err := workspace.FileAssetSource{Cache: e.statCacheFor(workspaceKey)}.LoadWithStats(ctx, root.CanonicalPath)
	if err != nil {
		return engine.Result{}, fmt.Errorf("扫描工作区: %w", err)
	}
	// 权限跳过如实上报(M1 配套状态面,K6 上抛口径)。
	status.setPermissionSkipped(scanStats.PermissionSkippedFiles)
	status.setScannedFiles(len(assets))

	previous, _, resolveErr := store.ResolveUsable()
	if resolveErr != nil && !isNoRevision(resolveErr) {
		return engine.Result{}, resolveErr
	}
	contentChanged := previous == nil || assetsChanged(assets, previous)
	repairRequested := e.consumeVectorRepair(workspaceKey)
	// needsLexicalRebuild：内容未变但词法索引不可用/落后（review B2 自愈）。
	needsLexicalRebuild := false
	if previous != nil && !contentChanged {
		if handle, probeErr := e.acquireHandle(workspaceKey); probeErr == nil {
			sameRevision := handle.manifest.Revision == previous.Revision
			e.releaseHandle(handle)
			if !sameRevision {
				needsLexicalRebuild = true
			} else if !repairRequested {
				// 无变化且词法可用：语义满足（或 semantic off）即 no-op；
				// provider 退避中同样 no-op，防重建风暴（D10/K30），
				// 覆盖缺口如实留在状态里。
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

	// 模式裁决（D1/D3）：delta 仅用于 v2 前身之上的内容变更；首建、
	// 自愈（词法/向量）、语义补齐（fill）与 compaction 走全量路径。
	useDelta := previous != nil && contentChanged &&
		previous.SchemaVersion == index.ManifestSchemaV2 &&
		!needsLexicalRebuild && !repairRequested &&
		len(previous.Segments) < compactSegmentThreshold &&
		garbageRatio(previous) < compactGarbageRatio
	if useDelta {
		return e.buildDelta(ctx, store, status, root, workspaceKey, previous, assets)
	}
	return e.buildFull(ctx, store, status, root, workspaceKey, previous, assets, contentChanged, needsLexicalRebuild,
		fullBuildReason(previous, contentChanged, needsLexicalRebuild, repairRequested))
}

// publishLexicalInterim 发布冷仓词法中间 revision:与最终发布同一
// staging/manifest/原子发布机制,向量文件为空集(合法,K10;
// vectorIndexes 载入 Count=0,dense 路自然"覆盖为空"短路,不触发
// repair)。失败返回 nil 且不阻塞主构建(中间发布是可用性增益,
// 最终发布仍是正确性路径)。
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
	// 发布前复验写锁(K46 同款):失锁即放弃中间发布。
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

// fullBuildReason 给出全量路径的成因标签(灰度反馈四 §6.1:构建原因
// 不可见时,delta 报表异常会被解读成"升级触发全量重嵌"级别的虚惊;
// 原因随 Result.BuildMode 外显,调用方无需从进度条反猜)。判定顺序与
// runBuild 的 useDelta 条件评估语义一致。
func fullBuildReason(previous *index.Manifest, contentChanged bool, needsLexicalRebuild bool, repairRequested bool) string {
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
	case contentChanged && garbageRatio(previous) >= compactGarbageRatio:
		return "full:compaction-garbage"
	case !contentChanged:
		return "full:semantic-fill"
	}
	return "full"
}

// chunkAsset 读取并切分单个文件；skipped 表示内容门禁拒绝（暗坑 K6）
// 或文件在扫描后消失/替换（S24：按"变更中"跳过并计数，本文件留给
// 下一次 sync 收敛，不中断整次构建）。
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
	// 语言能力由逐条 record 合并得出（mergeCapability），文件级
	// 返回值不再单独使用（review S21）。
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

// segmentArtifacts 是一次 staging 构建产物的校验和集合。
type segmentArtifacts struct {
	buildID              string
	staging              string
	chunksChecksum       string
	vectorsChecksum      string
	vectorsIndexChecksum string
}

// buildSegmentStaging 在 staging 内产出一个 segment：chunks.jsonl +
// Bleve 索引 +（semantic on 时）向量文件。
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
	// 向量文件写入 staging（semantic on 时恒写入，空集也合法，K10 同族）。
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

// newManifestSkeleton 组装 v2 manifest 公共字段。
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

// finishPublish 发布 manifest 并完成句柄退役、revision GC、journal 压实
// 与状态收敛。
func (e *Engine) finishPublish(store *index.Store, status *wsStatus, workspaceKey string, manifest *index.Manifest, staging string, discard func(), seman semanticOutcome, skippedFiles int, added int, buildMode string) (engine.Result, error) {
	// 发布前复验写锁（K46）：失锁说明所有权已被接管，本次构建作废。
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
	e.gcRevisions(store, workspaceKey, manifest.Revision, manifest.PreviousRevision)
	// 已随 revision 入盘的向量从 journal 清除（D4/K41：发布即 GC；
	// 失败不阻塞——上限截断兜底，下次发布重试）。
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
	// Added=本轮实际写入 segment 的 chunk 数(灰度反馈四 §6.1:此前
	// 统一取 manifest.Counts.Chunks,delta 构建被误报成 revision 总量,
	// 现场 1975 文件小改动显示 "Added=19028",被解读为升级触发全量
	// 重嵌的虚惊);BuildMode 显式外显构建形态与成因。
	result := engine.Result{
		Engine:             EngineID,
		IndexRevision:      manifest.Revision,
		FileCount:          manifest.Counts.Files,
		Uploaded:           0,
		Added:              added,
		BuildMode:          buildMode,
		CrossProfileReused: seman.crossProfileReused,
	}
	// P8(review 2026-08-06):sync 成功面不吞语义覆盖缺口——Result 携带
	// 覆盖率;覆盖不完整即给降级原因(与查询路径同 token),Summary 可见。
	// "禁止静默降级"红线在 sync 工具面闭合。
	if e.semanticEnabled() {
		result.SemanticCoverage = coveragePercent(manifest)
		if !manifest.SemanticComplete() {
			result.DegradedReason = "semantic-coverage-partial"
		}
	}
	return result, nil
}

// coveredPerFile 统计每文件被向量覆盖的 chunk 数（覆盖口径 K31/K51;
// 键 = embedKey,与 seman.entries 的键语义一致,方案① R1）。
func coveredPerFile(fileRecords []chunkRecord, coveredHash map[string]bool) int {
	covered := 0
	for _, record := range fileRecords {
		if coveredHash[embedKey(record)] {
			covered++
		}
	}
	return covered
}

// buildFull 全量构建：首建、词法/向量自愈、语义补齐（fill）与
// compaction（D3：合并后单段，chunk 与向量全部本地复用，零 provider
// 调用于未变更内容）。
func (e *Engine) buildFull(ctx context.Context, store *index.Store, status *wsStatus, root pathutil.WorkspaceRoot, workspaceKey string, previous *index.Manifest, assets workspace.AssetSet, contentChanged bool, needsLexicalRebuild bool, buildMode string) (engine.Result, error) {
	var previousChunks map[string][]chunkRecord
	if previous != nil {
		var err error
		previousChunks, err = loadLiveChunkRecordsByFile(store, previous)
		if err != nil {
			// previous 数据不可读时按全量重建处理，不中断本次构建。
			previousChunks = nil
		}
	}

	// 阶段 2：切分（未变化文件直接复用上一 revision 的 chunk 记录）。
	status.setStage(engine.IndexStageChunking)
	records := make([]chunkRecord, 0, len(assets)*8)
	recordsByFile := make(map[string][]chunkRecord, len(assets))
	files := make(map[string]index.FileEntry, len(assets))
	capabilities := map[string]string{}
	// totalBytes 口径：已索引 chunk 内容字节数（复用与新切分文件一致计算）。
	var totalBytes int64
	// skippedFiles 统计扫描通过但内容门禁拒绝的文件（二进制/非 UTF-8/超限），
	// 如实进入状态上报（暗坑 K6）。
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
			// 0 chunk 文件与门禁拒绝同路(H3 口径):全量单段路径无
			// 复活风险,但 Files{ChunkCount:0} 属脏数据且会被后续
			// delta/compaction 的 ContentHash 复用条件误命中。
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

	// 阶段 2.4:lexical-first 冷仓中间发布(框架 18.1/S4,灰度最痛:
	// 启用语义的冷仓在 embedding 结束前完全不可检索,大仓 30 分钟
	// 不可用)。仅冷仓首建(previous==nil)触发:embedding 开始前先
	// 发布词法 revision(空向量文件合法,K10;覆盖如实 0%,查询按
	// 既有降级语义显式标记),嵌入完成后照常发布语义完整 revision
	// (词法 revision 转为其 previous,GC 链一致)。中间发布失败不
	// 阻塞主构建(词法先行是增益,不是正确性依赖);显式 Sync 语义
	// 不变(仍阻塞到最终发布)。嵌入中途崩溃时词法 revision 保持可
	// 服务,下次 sync 走既有 semantic-fill 收敛。
	if previous == nil && e.semanticEnabled() && len(records) > 0 && e.lexicalFirst {
		if lexManifest := e.publishLexicalInterim(ctx, store, status, root, workspaceKey, records, files, capabilities, totalBytes); lexManifest != nil {
			previous = lexManifest
		}
	}

	// 阶段 2.5：语义路（semantic off 时零开销直通，K32）。
	var prior priorVectors
	if e.semanticEnabled() {
		if previous != nil {
			prior = e.loadPriorVectors(store, previous)
		}
		// 兼容旧 chunk profile 子树是最低优先级 prior；当前 active/
		// previous 始终优先。完整现役 revision 无需再载入兄弟大向量集
		// (避免 compaction 平白翻倍 RSS);仅冷仓/覆盖缺口路径发现。
		if previous == nil || !previous.SemanticComplete() || prior.activeLoadedRows < prior.activeExpectedRows {
			e.mergeSiblingProfileVectors(store, root, &prior)
		}
	}
	seman, err := e.embedRecords(ctx, store, workspaceKey, prior, records, status)
	if err != nil {
		return engine.Result{}, err
	}

	// D10 发布判定：内容未变、词法可用且向量无实质改善时不发布新
	// revision（防 no-op 发布膨胀）；返回现 revision，缺口如实在状态。
	if previous != nil && !contentChanged && !needsLexicalRebuild && !seman.improved() {
		status.setSemanticOutcome(seman.rejected, seman.lastError)
		status.ready(previous, revisionCount(store, previous))
		return engine.Result{
			Engine:        EngineID,
			IndexRevision: previous.Revision,
			FileCount:     previous.Counts.Files,
		}, nil
	}

	// 阶段 3：staging 写入。
	status.setStage(engine.IndexStageIndexing)
	artifacts, discard, err := e.buildSegmentStaging(ctx, store, records, seman)
	if err != nil {
		return engine.Result{}, err
	}

	// 阶段 4：manifest（v2 单段）+ 原子发布。
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

// buildDelta 增量构建（D1/G1）：只为变更文件产出 delta segment，删除/
// 改名进 tombstone；未触及文件的 chunk 与向量零工作量。
func (e *Engine) buildDelta(ctx context.Context, store *index.Store, status *wsStatus, root pathutil.WorkspaceRoot, workspaceKey string, previous *index.Manifest, assets workspace.AssetSet) (engine.Result, error) {
	status.setStage(engine.IndexStageChunking)
	changed, removed, currentPaths := diffDeltaAssets(previous, assets)
	delta, err := e.chunkChangedAssets(ctx, changed)
	if err != nil {
		return engine.Result{}, err
	}
	records := delta.records

	// 语义路：只嵌入 delta 记录（复用按纯 content hash，D2）。
	var prior priorVectors
	if e.semanticEnabled() {
		prior = e.loadPriorVectors(store, previous)
	}
	seman, err := e.embedRecords(ctx, store, workspaceKey, prior, records, status)
	if err != nil {
		return engine.Result{}, err
	}
	coveredHash := make(map[string]bool, len(seman.entries))
	for _, entry := range seman.entries {
		coveredHash[entry.ContentHash] = true
	}

	// manifest v2 组装：Files/Tombstones/Segments。
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
			// 内容门禁拒绝的变更文件从 live 集移除（旧版本亦不可再检索）。
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

// diffDeltaAssets 对比 previous manifest 与当前资产集:变更文件(新增或
// 内容身份变化)、删除路径与现存路径集。
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

// deltaChunks 是 buildDelta 切分阶段的产物。
type deltaChunks struct {
	records      []chunkRecord
	byFile       map[string][]chunkRecord
	skippedFiles int
	skippedPaths map[string]bool
}

// chunkChangedAssets 切分变更文件。0 chunk 的变更文件(纯空白/仅注释等)
// 按删除语义处理,与内容门禁拒绝同路:从 Files 摘除 + 入 tombstone。
// 否则旧段 chunk 无人覆盖继续可检索,且 compaction 按 ContentHash 复用
// 将污染固化(H3,诊断报告 2026-08-03;K39/K44 口径)。
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

// deltaTombstones 计算增量后的墓碑集:(prev ∪ removed ∪ 被拒绝的变更
// 文件) − 现存路径(K44),排序输出。
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

// sumLiveCounts 按存活文件求和计数与向量覆盖(K31/K51 口径)。
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

// assetsChanged 判断文件集合或内容身份是否变化。
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

// mergeCapability 合并语言能力：全部 ast 才是 ast，混合如实上报 mixed。
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

// writeChunkRecords 把 chunk 记录写为 JSONL 并落盘同步。
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

// readSegmentChunkRecords 读取单个 segment 的全部 chunk 记录。
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

// loadLiveChunkRecordsByFile 读取 revision 的存活 chunk 记录（多 segment
// newest-wins + tombstone 过滤，D1/K44）：按段序后者覆盖前者，再以
// manifest.Files 作为存活文件的最终裁决。
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

// gcRevisions 回收 active/previous 之外的旧 revision（review B4，D5 修订）：
// 有打开句柄（refs>0）的 revision 跳过，留给下一次 GC；GC 失败不阻塞发布。
// segment 级共享由 store.RemoveRevision 的引用计数保证（Stage 4 K42）。
func (e *Engine) gcRevisions(store *index.Store, workspaceKey string, activeRevision string, previousRevision string) {
	revisions, err := store.ListRevisions()
	if err != nil {
		return
	}
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
		}
		_ = store.RemoveRevision(revision)
	}
}

// revisionCount 统计当前保留链上的 revision 数（状态上报）。
// 链遍历带环检测与深度上限（review S3，与 ResolveUsable 同护栏）。
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
