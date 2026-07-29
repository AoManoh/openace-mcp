package localengine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
)

// noHitsText 与既有 MCP 文案保持一致。
const noHitsText = "No relevant code sections were found."

// revisionHandle 是一个已打开 revision 的只读句柄（refcount 管理，暗坑 K3/K11）。
type revisionHandle struct {
	workspaceKey string
	manifest     *index.Manifest
	lex          *lexical.Index
	chunks       map[string]chunkRecord
	refs         int
	retired      bool
}

// Search 实现 engine.Service：按需同步后在 active revision 上执行词法检索。
func (e *Engine) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	if err := rejectProfileID(req.Workspace); err != nil {
		return engine.Result{}, err
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return engine.Result{}, errors.New("查询内容为空")
	}
	// 检索前确保索引就绪（与 legacy Syncer 的 retrieval 语义一致）。
	syncResult, err := e.syncWorkspace(ctx, req.Workspace)
	if err != nil {
		return engine.Result{}, err
	}
	_, workspaceKey, err := e.resolveRoot(req.Workspace.DirectoryPath)
	if err != nil {
		return engine.Result{}, err
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		return engine.Result{}, err
	}
	defer e.releaseHandle(handle)

	hits, err := handle.lex.Search(ctx, query, defaultTopK)
	if err != nil {
		return engine.Result{}, fmt.Errorf("词法检索: %w", err)
	}
	text := renderHits(handle, hits, req.MaxOutputLen)
	result := syncResult
	result.Text = text
	result.Engine = EngineID
	result.IndexRevision = handle.manifest.Revision
	return result, nil
}

// handleKey 是句柄表键：workspaceKey + revision 复合，避免跨工作区
// revision 碰撞与同名键位覆盖（review B3）。
func handleKey(workspaceKey string, revision string) string {
	return workspaceKey + "\x00" + revision
}

// acquireHandle 打开（或复用）首个真正可打开的 revision 句柄并增加引用。
// manifest 校验通过但 Bleve 打开失败的 revision 视为损坏，沿 previous 链
// 继续回退（review B2），全部失败时返回最后错误。
func (e *Engine) acquireHandle(workspaceKey string) (*revisionHandle, error) {
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return nil, err
	}
	manifest, skipped, err := store.ResolveUsable()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for manifest != nil {
		handle, openErr := e.openOrReuseHandle(store, workspaceKey, manifest)
		if openErr == nil {
			if len(skipped) > 0 {
				e.noteSkippedRevisions(workspaceKey, skipped)
			}
			return handle, nil
		}
		lastErr = openErr
		skipped = append(skipped, manifest.Revision)
		previousRevision := manifest.PreviousRevision
		manifest = nil
		if previousRevision != "" {
			if previous, loadErr := store.LoadManifest(previousRevision); loadErr == nil {
				if verifyErr := store.VerifyManifest(previous); verifyErr == nil {
					manifest = previous
				}
			}
		}
	}
	e.noteSkippedRevisions(workspaceKey, skipped)
	if lastErr == nil {
		lastErr = index.ErrNoUsableRevision
	}
	return nil, lastErr
}

// openOrReuseHandle 复用缓存句柄（含已退役者：revision 不可变，数据仍有效，
// 重新启用即可），否则打开新句柄并注册。
func (e *Engine) openOrReuseHandle(store *index.Store, workspaceKey string, manifest *index.Manifest) (*revisionHandle, error) {
	key := handleKey(workspaceKey, manifest.Revision)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("local-hybrid 引擎已关闭")
	}
	if handle, ok := e.handles[key]; ok {
		handle.retired = false
		handle.refs++
		e.mu.Unlock()
		return handle, nil
	}
	e.mu.Unlock()

	lex, err := lexical.Open(filepath.Join(store.SegmentPath(manifest), index.LexicalDirName))
	if err != nil {
		return nil, fmt.Errorf("打开词法索引（revision %s）: %w", manifest.Revision, err)
	}
	records, err := loadChunkRecords(store, manifest)
	if err != nil {
		lex.Close()
		return nil, err
	}
	chunks := make(map[string]chunkRecord, len(records))
	for _, record := range records {
		chunks[record.ID] = record
	}
	handle := &revisionHandle{workspaceKey: workspaceKey, manifest: manifest, lex: lex, chunks: chunks, refs: 1}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		go lex.Close()
		return nil, errors.New("local-hybrid 引擎已关闭")
	}
	if existing, ok := e.handles[key]; ok {
		// 并发打开竞争：复用已注册句柄，丢弃本次打开。
		existing.retired = false
		existing.refs++
		go lex.Close()
		return existing, nil
	}
	e.handles[key] = handle
	return handle, nil
}

// releaseHandle 归还引用；已退役且无引用的句柄立即关闭。
// 删除表项前校验身份，防止误删同键新句柄（review B3）。
func (e *Engine) releaseHandle(handle *revisionHandle) {
	key := handleKey(handle.workspaceKey, handle.manifest.Revision)
	e.mu.Lock()
	handle.refs--
	shouldClose := handle.retired && handle.refs <= 0
	if shouldClose && e.handles[key] == handle {
		delete(e.handles, key)
	}
	e.mu.Unlock()
	if shouldClose {
		_ = handle.lex.Close()
	}
}

// retireHandles 在新 revision 发布后退役不再保留的句柄
// （只保留 active 与 previous；使用中的句柄延迟到引用归零关闭）。
func (e *Engine) retireHandles(workspaceKey string, activeRevision string, previousRevision string) {
	keep := map[string]bool{
		handleKey(workspaceKey, activeRevision):   true,
		handleKey(workspaceKey, previousRevision): true,
	}
	var closable []*revisionHandle
	e.mu.Lock()
	for key, handle := range e.handles {
		if handle.workspaceKey != workspaceKey || keep[key] {
			continue
		}
		handle.retired = true
		if handle.refs <= 0 {
			delete(e.handles, key)
			closable = append(closable, handle)
		}
	}
	e.mu.Unlock()
	for _, handle := range closable {
		_ = handle.lex.Close()
	}
}

// renderBlock 是渲染前的合并单元。
type renderBlock struct {
	record chunkRecord
	score  float64
}

// renderHits 把命中渲染为稳定文本格式（golden 锁定，暗坑 K13）：
// 同文件重叠/相邻 chunk 合并，按最高分排序，MaxOutputLen 预算截断。
func renderHits(handle *revisionHandle, hits []lexical.Hit, maxOutputLen int) string {
	if len(hits) == 0 {
		return noHitsText
	}
	blocks := make([]renderBlock, 0, len(hits))
	for _, hit := range hits {
		record, ok := handle.chunks[hit.ID]
		if !ok {
			continue
		}
		blocks = append(blocks, renderBlock{record: record, score: hit.Score})
	}
	if len(blocks) == 0 {
		return noHitsText
	}
	merged := mergeBlocks(blocks)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].score > merged[j].score })

	var out strings.Builder
	budget := maxOutputLen
	if budget <= 0 {
		budget = 20000
	}
	truncated := false
	for i, block := range merged {
		section := formatBlock(block.record)
		if i > 0 && out.Len()+len(section) > budget {
			truncated = true
			break
		}
		out.WriteString(section)
	}
	if truncated {
		out.WriteString("\n[output truncated by max_output_length]\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// mergeBlocks 只合并同文件真正重叠或严格相邻的块（next.Start ≤ current.End+1），
// 取最高分。禁止跨间隙合并：缺行会让 header 行区间与内容错位（review B1）。
func mergeBlocks(blocks []renderBlock) []renderBlock {
	byFile := make(map[string][]renderBlock)
	for _, block := range blocks {
		byFile[block.record.RelPath] = append(byFile[block.record.RelPath], block)
	}
	var merged []renderBlock
	for _, group := range byFile {
		sort.Slice(group, func(i, j int) bool { return group[i].record.StartLine < group[j].record.StartLine })
		current := group[0]
		for _, next := range group[1:] {
			if next.record.StartLine <= current.record.EndLine+1 {
				if next.record.EndLine > current.record.EndLine {
					current.record.Content = current.record.Content + "\n" + tailLines(next.record, current.record.EndLine)
					current.record.EndLine = next.record.EndLine
				}
				if next.score > current.score {
					current.score = next.score
				}
				if current.record.Symbol == "" {
					current.record.Symbol = next.record.Symbol
				}
				continue
			}
			merged = append(merged, current)
			current = next
		}
		merged = append(merged, current)
	}
	return merged
}

// tailLines 取 next 中位于 afterLine 之后的部分，避免合并时内容重复。
func tailLines(record chunkRecord, afterLine int) string {
	skip := afterLine - record.StartLine + 1
	if skip <= 0 {
		return record.Content
	}
	lines := strings.Split(record.Content, "\n")
	if skip >= len(lines) {
		return ""
	}
	return strings.Join(lines[skip:], "\n")
}

// formatBlock 渲染单个块：`## path:start-end [symbol]` + 代码围栏。
func formatBlock(record chunkRecord) string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(record.RelPath)
	b.WriteString(":")
	fmt.Fprintf(&b, "%d-%d", record.StartLine, record.EndLine)
	if record.Symbol != "" {
		b.WriteString(" ")
		b.WriteString(record.Symbol)
	}
	b.WriteString("\n```")
	if record.Language != "" && record.Language != "text" {
		b.WriteString(record.Language)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(record.Content, "\n"))
	b.WriteString("\n```\n\n")
	return b.String()
}
