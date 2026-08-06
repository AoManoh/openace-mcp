package localengine

import (
	"fmt"
	"sort"
	"strings"

	"context"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 本文件实现 repo_map R1(D4 呈批件;框架文档"条件 dogfood 候选"):
// 快照只读的仓库地图——调用方在检索前获得仓库词汇与路径(orientation),
// 不进入 RRF、不影响检索排序。数据全部来自现役 revision 的 chunk 元数据
// (Symbol/行区间/语言),零新增索引产物、零 provider 调用;冷仓显式
// not-ready(不隐式 Sync,防"零费用工具"暗地触发付费嵌入)。

// repoMapDefaultBudget 是地图默认预算(字节;与检索输出同契约)。
const repoMapDefaultBudget = 20000

// mapFile 是单文件聚合。
type mapFile struct {
	path    string
	topDir  string
	symbols []string
	chunks  int
	maxLine int
	score   float64
}

// RepoMap 实现 engine.RepoMapper。
func (e *Engine) RepoMap(ctx context.Context, req engine.RepoMapRequest) (engine.Result, error) {
	if err := rejectProfileID(req.Workspace); err != nil {
		return engine.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return engine.Result{}, err
	}
	_, workspaceKey, err := e.resolveRoot(req.Workspace.DirectoryPath)
	if err != nil {
		return engine.Result{}, err
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		// 快照只读语义:无 revision 即显式 not-ready,指名下一步动作。
		return engine.Result{}, fmt.Errorf("index_not_ready: no published revision for this workspace yet; run codebase_retrieval or sync_workspace first (repo_map never triggers indexing or provider calls)")
	}
	defer e.releaseHandle(handle)

	focus := strings.Trim(strings.TrimSpace(req.Focus), "/")
	files := aggregateMapFiles(handle, focus)
	if len(files) == 0 {
		if focus != "" {
			return engine.Result{}, engine.AsInvalidRequest(fmt.Errorf("focus %q matches no indexed files", req.Focus))
		}
		return engine.Result{}, fmt.Errorf("index_not_ready: active revision has no files")
	}
	budget := req.MaxOutputLen
	if budget <= 0 {
		budget = repoMapDefaultBudget
	}
	text, shownFiles, truncated := renderRepoMap(handle.manifest.Revision, files, budget, focus)
	result := engine.Result{
		Engine:        EngineID,
		IndexRevision: handle.manifest.Revision,
		FileCount:     handle.manifest.Counts.Files,
		Text:          text,
	}
	result.Display = &engine.DisplayStats{
		CandidateBlocks: len(files), ShownBlocks: shownFiles, ShownFiles: shownFiles, Truncated: truncated,
	}
	return result, nil
}

// aggregateMapFiles 按文件聚合 chunk 元数据并打确定性重要度分。
func aggregateMapFiles(handle *revisionHandle, focus string) []mapFile {
	byPath := make(map[string]*mapFile)
	for _, meta := range handle.chunks {
		if focus != "" && !(meta.RelPath == focus || strings.HasPrefix(meta.RelPath, focus+"/")) {
			continue
		}
		entry, ok := byPath[meta.RelPath]
		if !ok {
			top := "."
			if i := strings.IndexByte(meta.RelPath, '/'); i > 0 {
				top = meta.RelPath[:i]
			}
			entry = &mapFile{path: meta.RelPath, topDir: top}
			byPath[meta.RelPath] = entry
		}
		entry.chunks++
		if meta.EndLine > entry.maxLine {
			entry.maxLine = meta.EndLine
		}
		if meta.Symbol != "" {
			entry.symbols = append(entry.symbols, meta.Symbol)
		}
	}
	files := make([]mapFile, 0, len(byPath))
	for _, entry := range byPath {
		// 符号去重排序(chunks map 迭代无序,先归一保证确定性)。
		sort.Strings(entry.symbols)
		entry.symbols = dedupSorted(entry.symbols)
		// 重要度 = 符号密度 × 路径负先验(test/vendored/generated 降权;
		// 吸收清单 #1 路径启发式子集,只作用于地图展示序)。
		entry.score = float64(len(entry.symbols)) * pathPrior(entry.path)
		files = append(files, *entry)
	}
	// 确定性总序:分数降序,路径字典序平局(aider #1874 教训)。
	sort.Slice(files, func(i, j int) bool {
		if files[i].score != files[j].score {
			return files[i].score > files[j].score
		}
		return files[i].path < files[j].path
	})
	return files
}

// pathPrior 是路径负先验(确定性乘子,不进检索排序)。
func pathPrior(path string) float64 {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "vendor/") || strings.Contains(lower, "node_modules/") ||
		strings.Contains(lower, "generated") || strings.Contains(lower, ".min."):
		return 0.1
	case strings.Contains(lower, "_test.") || strings.Contains(lower, "test_") ||
		strings.HasPrefix(lower, "tests/") || strings.Contains(lower, "/tests/") ||
		strings.HasPrefix(lower, "testdata/") || strings.Contains(lower, "/testdata/"):
		return 0.25
	case strings.HasPrefix(lower, "docs/") || strings.Contains(lower, "/docs/"):
		return 0.6
	}
	return 1.0
}

// renderRepoMap 预算内渲染:顶层目录轮转保底(每目录先出最强文件,
// 防单目录刷屏——反馈二 §3.4 预算错配教训),随后按总分回填;目录内
// 展示按分数序,目录间按首文件分数序。
func renderRepoMap(revision string, files []mapFile, budget int, focus string) (string, int, bool) {
	perDir := make(map[string][]mapFile)
	var dirOrder []string
	for _, f := range files {
		if _, ok := perDir[f.topDir]; !ok {
			dirOrder = append(dirOrder, f.topDir)
		}
		perDir[f.topDir] = append(perDir[f.topDir], f)
	}
	// 选择与渲染解耦(实测修复:轮转序线性输出会把跨目录条目挤进
	// 错误的目录 header 下)。选择=轮转+预算(每轮每目录取一个,
	// 保证顶层目录保底;边际成本含该目录 header 首次开销);渲染=
	// 按目录分组,目录序=首选序,目录内按分数序。
	title := fmt.Sprintf("# repo map: %s (%d files", revision, totalFiles(perDir))
	if focus != "" {
		title += fmt.Sprintf(", focus=%s", focus)
	}
	title += ")\n"
	dirHeader := func(dir string) string {
		group := perDir[dir]
		symbols := 0
		for _, f := range group {
			symbols += len(f.symbols)
		}
		return fmt.Sprintf("%s/ (%d files, %d symbols)\n", dir, len(group), symbols)
	}
	used := len(title)
	selected := make(map[string][]mapFile)
	dirCharged := make(map[string]bool)
	shown := 0
	truncated := false
	round := 0
	for !truncated {
		advanced := false
		for _, dir := range dirOrder {
			group := perDir[dir]
			if round >= len(group) {
				continue
			}
			advanced = true
			marginal := len(formatMapFile(group[round]))
			if !dirCharged[dir] {
				marginal += len(dirHeader(dir))
			}
			if used+marginal > budget {
				truncated = true
				break
			}
			used += marginal
			dirCharged[dir] = true
			selected[dir] = append(selected[dir], group[round])
			shown++
		}
		if !advanced {
			break
		}
		round++
	}

	var out strings.Builder
	out.WriteString(title)
	for _, dir := range dirOrder {
		group := selected[dir]
		if len(group) == 0 {
			continue
		}
		out.WriteString(dirHeader(dir))
		for _, f := range group {
			out.WriteString(formatMapFile(f))
		}
	}
	if truncated {
		out.WriteString(fmt.Sprintf("[map truncated: %d of %d files shown; raise max_output_length or use focus for a subtree]\n", shown, len(files)))
	}
	return strings.TrimRight(out.String(), "\n"), shown, truncated
}

func totalFiles(perDir map[string][]mapFile) int {
	n := 0
	for _, group := range perDir {
		n += len(group)
	}
	return n
}

// formatMapFile 单文件一行:path:1-maxLine 符号清单(上限 8,可续)。
func formatMapFile(f mapFile) string {
	line := fmt.Sprintf("  %s:1-%d", f.path, f.maxLine)
	if len(f.symbols) > 0 {
		shown := f.symbols
		more := ""
		if len(shown) > 8 {
			more = fmt.Sprintf(", +%d more", len(shown)-8)
			shown = shown[:8]
		}
		line += " " + strings.Join(shown, ", ") + more
	}
	return line + "\n"
}

func dedupSorted(items []string) []string {
	out := items[:0]
	var last string
	for i, item := range items {
		if i == 0 || item != last {
			out = append(out, item)
		}
		last = item
	}
	return out
}
