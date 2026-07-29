package localengine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"context"

	"github.com/AoManoh/openace-mcp/internal/chunk"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
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

// runBuild 执行一次完整索引构建：发现→切分→词法→发布。
// 全程在 staging 内进行，任何失败/取消都丢弃 staging（暗坑 K2/K16）。
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

	// 阶段 1：发现（复用 workspace 扫描与 AssetPolicy，暗坑 K4）。
	status.setStage(engine.IndexStageScanning)
	assets, err := workspace.FileAssetSource{}.Load(ctx, root.CanonicalPath)
	if err != nil {
		return engine.Result{}, fmt.Errorf("扫描工作区: %w", err)
	}

	// 增量判定：与当前 active manifest 对比文件内容身份。
	previous, _, resolveErr := store.ResolveUsable()
	if resolveErr != nil && !isNoRevision(resolveErr) {
		return engine.Result{}, resolveErr
	}
	if previous != nil && !assetsChanged(assets, previous) {
		// 无变化且当前 revision 确实可打开时才 no-op（业务验收 P2-T08a）；
		// manifest 校验通过但 Bleve 已损坏的 revision 必须重建，
		// 不得让工作区停留在永久失败态（review B2）。
		if handle, probeErr := e.acquireHandle(workspaceKey); probeErr == nil {
			usableRevision := handle.manifest.Revision
			e.releaseHandle(handle)
			if usableRevision == previous.Revision {
				status.ready(previous, revisionCount(store, previous))
				return engine.Result{
					Engine:        EngineID,
					IndexRevision: previous.Revision,
					FileCount:     previous.Counts.Files,
				}, nil
			}
		}
	}

	var previousChunks map[string][]chunkRecord
	if previous != nil {
		previousChunks, err = loadChunkRecordsByFile(store, previous)
		if err != nil {
			// previous 数据不可读时按全量重建处理，不中断本次构建。
			previousChunks = nil
		}
	}

	// 阶段 2：切分（未变化文件直接复用上一 revision 的 chunk 记录）。
	status.setStage(engine.IndexStageChunking)
	records := make([]chunkRecord, 0, len(assets)*8)
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
		if previous != nil {
			if entry, ok := previous.Files[asset.RelPath]; ok && entry.ContentHash == asset.BlobName {
				if reuse, ok := previousChunks[asset.RelPath]; ok {
					fileRecords = reuse
				}
			}
		}
		if fileRecords == nil {
			content, ok, readErr := workspace.ReadIndexableContent(ctx, asset.AbsPath)
			if readErr != nil {
				return engine.Result{}, fmt.Errorf("读取 %s: %w", asset.RelPath, readErr)
			}
			if !ok {
				skippedFiles++
				continue
			}
			chunks, capability := e.profile.Split(chunk.File{RelPath: asset.RelPath, Content: string(content)})
			fileRecords = make([]chunkRecord, 0, len(chunks))
			for _, c := range chunks {
				fileRecords = append(fileRecords, chunkRecord{
					ID: c.ID, RelPath: c.RelPath, Language: c.Language,
					Capability: string(c.Capability), StartLine: c.StartLine, EndLine: c.EndLine,
					Symbol: c.SymbolHint, Content: c.Content, ContentHash: c.ContentHash,
				})
			}
			_ = capability
		}
		for _, record := range fileRecords {
			mergeCapability(capabilities, record.Language, record.Capability)
			totalBytes += int64(len(record.Content))
		}
		files[asset.RelPath] = index.FileEntry{ContentHash: asset.BlobName, ChunkCount: len(fileRecords)}
		records = append(records, fileRecords...)
	}

	// 阶段 3：staging 写入 chunks.jsonl 与 Bleve 索引。
	status.setStage(engine.IndexStageIndexing)
	buildID := index.NewBuildID()
	staging, err := store.BeginStaging(buildID)
	if err != nil {
		return engine.Result{}, err
	}
	discard := func() { _ = store.DiscardStaging(buildID) }

	chunksPath := filepath.Join(staging, index.ChunksFileName)
	if err := writeChunkRecords(chunksPath, records); err != nil {
		discard()
		return engine.Result{}, fmt.Errorf("写入 chunk 数据: %w", err)
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
		return engine.Result{}, fmt.Errorf("构建词法索引: %w", err)
	}
	if err := ctx.Err(); err != nil {
		discard()
		return engine.Result{}, err
	}

	// 阶段 4：manifest + 原子发布。
	status.setStage(engine.IndexStagePublishing)
	checksum, err := index.ChecksumFile(chunksPath)
	if err != nil {
		discard()
		return engine.Result{}, err
	}
	now := time.Now().UTC()
	manifest := &index.Manifest{
		SchemaVersion: index.ManifestSchemaVersion,
		Workspace: index.WorkspaceIdentity{
			CanonicalPath: root.CanonicalPath,
			PathKind:      string(root.PathKind),
			HostOS:        root.HostOS,
		},
		EngineID:            EngineID,
		EngineVersion:       EngineVersion,
		Revision:            "rev-" + buildID,
		PolicyHash:          policyHash,
		ChunkerID:           e.profile.ID,
		ChunkerVersion:      e.profile.Version,
		ChunkerCapabilities: capabilities,
		LexicalEngine:       lexical.EngineName,
		LexicalVersion:      lexical.EngineVersion,
		SegmentID:           buildID,
		Files:               files,
		Counts:              index.Counts{Files: len(files), Chunks: len(records), Bytes: totalBytes},
		ChunksChecksum:      checksum,
		CreatedAt:           now,
		ActivatedAt:         now,
	}
	if previous != nil {
		manifest.PreviousRevision = previous.Revision
	}
	if err := store.Publish(manifest, staging); err != nil {
		discard()
		return engine.Result{}, fmt.Errorf("发布索引: %w", err)
	}
	e.retireHandles(workspaceKey, manifest.Revision, manifest.PreviousRevision)
	e.gcRevisions(store, workspaceKey, manifest.Revision, manifest.PreviousRevision)
	status.setSkippedFiles(skippedFiles)
	status.ready(manifest, revisionCount(store, manifest))

	return engine.Result{
		Engine:        EngineID,
		IndexRevision: manifest.Revision,
		FileCount:     manifest.Counts.Files,
		Uploaded:      0,
		Added:         manifest.Counts.Chunks,
	}, nil
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

// loadChunkRecords 读取 revision 的全部 chunk 记录。
func loadChunkRecords(store *index.Store, manifest *index.Manifest) ([]chunkRecord, error) {
	path := filepath.Join(store.SegmentPath(manifest), index.ChunksFileName)
	f, err := os.Open(path)
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

// loadChunkRecordsByFile 按文件分组读取 chunk 记录（增量复用）。
func loadChunkRecordsByFile(store *index.Store, manifest *index.Manifest) (map[string][]chunkRecord, error) {
	records, err := loadChunkRecords(store, manifest)
	if err != nil {
		return nil, err
	}
	byFile := make(map[string][]chunkRecord)
	for _, record := range records {
		byFile[record.RelPath] = append(byFile[record.RelPath], record)
	}
	return byFile, nil
}

// gcRevisions 回收 active/previous 之外的旧 revision（review B4，D5 修订）：
// 有打开句柄（refs>0）的 revision 跳过，留给下一次 GC；GC 失败不阻塞发布。
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
		}
		_ = store.RemoveRevision(revision)
	}
}

// revisionCount 统计当前保留链上的 revision 数（状态上报）。
func revisionCount(store *index.Store, manifest *index.Manifest) int {
	count := 0
	revision := manifest.Revision
	for revision != "" {
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
