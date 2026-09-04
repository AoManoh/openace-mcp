package localengine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 本文件实现 repo_map 工具：只读当前已发布的索引 revision，输出按重要度
// 排序的文件清单和每个文件的符号名，供调用方在检索之前了解仓库里有什么、
// 在哪里。它不参与检索排序，不进入 RRF 融合与精排。数据全部来自当前
// revision 的 chunk 元数据（符号名、行区间），不新增索引产物，不调用
// provider。工作区还没有任何已发布 revision 时直接返回 index_not_ready
// 错误，不触发同步：同步会带来付费嵌入，一个对外宣称零费用的工具不应
// 该暗中触发付费。

// repoMapDefaultBudget 是 RepoMapRequest.MaxOutputLen 未指定（≤0）时
// 地图正文的字节预算。
const repoMapDefaultBudget = 20000

// mapFile 是一个文件在地图中的聚合结果：path 为索引内相对路径；topDir
// 为顶层目录名（根目录文件为 "."，指定 focus 时统一替换为 focus 前缀，
// 使全部结果归在一个目录标题下）；symbols 为去重排序后的符号名；chunks
// 为 chunk 数；maxLine 为最大结束行；score 为展示排序用的重要度。
type mapFile struct {
	path    string
	topDir  string
	symbols []string
	chunks  int
	maxLine int
	score   float64
}

// RepoMap 实现 engine.RepoMapper，按下列顺序处理请求：
//   - 工作区引用带 profile id、ctx 已取消或目录解析失败：原样返回错误。
//   - 工作区没有可打开的已发布 revision：返回 index_not_ready 错误并在
//     文本中给出下一步动作，不触发同步（理由见文件头）。
//   - 按 focus 过滤后没有文件：指定了 focus 时按请求类错误返回（前缀
//     不匹配任何已索引文件）；未指定 focus 说明当前 revision 没有文件，
//     同样返回 index_not_ready。
//   - 正常路径：MaxOutputLen ≤0 时用 repoMapDefaultBudget；结果携带
//     IndexRevision、FileCount、渲染与总耗时，Display 记录候选文件数、
//     实际展示文件数与是否因预算截断。
func (e *Engine) RepoMap(ctx context.Context, req engine.RepoMapRequest) (engine.Result, error) {
	started := time.Now()
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
		// 打不开任何已发布 revision 时不在这里触发同步，直接报
		// index_not_ready 并告诉调用方先跑检索或同步工具。
		return engine.Result{}, fmt.Errorf("index_not_ready: no published revision for this workspace yet; run codebase_retrieval or sync_workspace first (repo_map never triggers indexing or provider calls)")
	}
	defer e.releaseHandle(handle)

	focus := strings.Trim(strings.TrimSpace(req.Focus), "/")
	files := cachedMapFiles(handle, focus)
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
	renderStart := time.Now()
	text, shownFiles, truncated := renderRepoMap(handle.manifest.Revision, files, budget, focus)
	renderMs := time.Since(renderStart).Milliseconds()
	result := engine.Result{
		Engine:        EngineID,
		IndexRevision: handle.manifest.Revision,
		FileCount:     handle.manifest.Counts.Files,
		Text:          text,
		Timings: &engine.RetrievalTimings{
			RenderMs: renderMs,
			TotalMs:  time.Since(started).Milliseconds(),
		},
	}
	result.Display = &engine.DisplayStats{
		CandidateBlocks: len(files), ShownBlocks: shownFiles, ShownFiles: shownFiles, Truncated: truncated,
	}
	return result, nil
}

// cachedMapFiles 返回当前 revision 按 focus 过滤后的文件聚合列表。
//   - 全仓聚合结果按句柄缓存一次（handle.repoMapOnce）。revision 不可变，
//     缓存不会过期；大仓库每次重扫全部 chunk 元数据要秒级（Kubernetes
//     仓库实测重复聚合 2.41 秒，缓存后 302 毫秒）。
//   - focus 非空时只保留路径等于 focus 或位于 focus/ 之下的文件，并把
//     topDir 改成 focus，让渲染把它们归在同一个目录标题下。过滤只在
//     文件级进行，不重扫 chunk。
//   - 返回的是拷贝（symbols 切片也拷贝），调用方修改 topDir 或排序不会
//     污染缓存。
func cachedMapFiles(handle *revisionHandle, focus string) []mapFile {
	handle.repoMapOnce.Do(func() {
		handle.repoMapFiles = aggregateMapFiles(handle)
	})
	files := make([]mapFile, 0, len(handle.repoMapFiles))
	for _, file := range handle.repoMapFiles {
		if focus != "" && !(file.path == focus || strings.HasPrefix(file.path, focus+"/")) {
			continue
		}
		clone := file
		clone.symbols = append([]string(nil), file.symbols...)
		if focus != "" {
			clone.topDir = focus
		}
		files = append(files, clone)
	}
	return files
}

// aggregateMapFiles 把当前 revision 的全部存活 chunk 元数据按文件聚合，
// 计算每个文件的重要度，并按重要度降序、路径字典序返回。
func aggregateMapFiles(handle *revisionHandle) []mapFile {
	byPath := make(map[string]*mapFile)
	for _, meta := range handle.chunks {
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
		// handle.chunks 是 map，遍历顺序每次不同；先排序去重，保证同一
		// revision 每次得到相同的符号列表。
		sort.Strings(entry.symbols)
		entry.symbols = dedupSorted(entry.symbols)
		// 重要度 = 去重后符号数 × pathPrior 路径乘子（测试、vendored、生成
		// 物、文档降权）。该分数只决定地图的展示顺序，不进入检索排序。
		entry.score = float64(len(entry.symbols)) * pathPrior(entry.path)
		files = append(files, *entry)
	}
	// 总序必须确定：分数降序，同分按路径字典序。aider 项目曾因同分候选
	// 的先后每次不同而输出不同的地图（其公开 issue #1874），调用方的缓存
	// 与判断随之变化；显式的次级排序键避免这一点。
	sort.Slice(files, func(i, j int) bool {
		if files[i].score != files[j].score {
			return files[i].score > files[j].score
		}
		return files[i].path < files[j].path
	})
	return files
}

// pathPrior 返回路径的重要度乘子：vendor/、node_modules/、含 generated
// 或 .min. 的路径为 0.1；测试文件与测试目录为 0.25；docs/ 目录为 0.6；
// 其余为 1.0。同一路径恒定同一乘子；只用于地图展示序，不进入检索排序。
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

// renderRepoMap 在字节预算内渲染地图，返回正文、实际展示的文件数与是否
// 截断。
//   - 目录顺序：files 已按重要度降序，每个顶层目录按其最高分文件首次
//     出现的位置排序（dirOrder），目录内文件保持分数降序。
//   - 选择按轮次轮转：第 r 轮依 dirOrder 从每个目录取其第 r 个文件。第 0
//     轮保证每个顶层目录至少有一个文件进入地图，之后才轮到各目录的第
//     二、第三个文件。若单纯按总分填充，一个符号密集的大目录会占满预算，
//     其他顶层目录一个文件也进不了地图，调用方看不到仓库的整体结构。
//   - 预算：一个文件的边际成本是它那一行的长度，所属目录首次被选中时
//     再加目录标题行的长度；累计超过 budget 时停止选择并置 truncated。
//   - 渲染与选择分开：正文按 dirOrder 逐目录输出标题和该目录选中的文
//     件。若按轮转顺序边选边输出，第二轮起的条目会落在错误的目录标题
//     之下（试用时实际出现过）。
//   - 截断时末尾追加一行提示，写明已展示数与总数，并提示调大预算或用
//     focus 缩小到子树。
func renderRepoMap(revision string, files []mapFile, budget int, focus string) (string, int, bool) {
	perDir := make(map[string][]mapFile)
	var dirOrder []string
	for _, f := range files {
		if _, ok := perDir[f.topDir]; !ok {
			dirOrder = append(dirOrder, f.topDir)
		}
		perDir[f.topDir] = append(perDir[f.topDir], f)
	}
	// 下面先只做选择（轮转 + 预算记账），选完再按目录分组输出；两步
	// 合并会把后续轮次的文件写到错误的目录标题下。
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

// formatMapFile 把一个文件渲染为一行：两个空格缩进的 path:1-maxLine，
// 其后最多列 8 个符号名，超出部分写成 ", +N more"，末尾带换行。
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
