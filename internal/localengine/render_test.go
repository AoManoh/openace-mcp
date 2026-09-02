package localengine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
)

func readFileForTest(path string) ([]byte, error)     { return os.ReadFile(path) }
func writeFileForTest(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// newRenderHandle 把记录物化为真实 segment 文件并走 pread 路径构建句柄
// （D5 之后渲染内容按需读取，测试覆盖真实取回链路）。
func newRenderHandle(t *testing.T, records ...chunkRecord) *revisionHandle {
	t.Helper()
	dir := t.TempDir()
	if err := writeChunkRecords(filepath.Join(dir, index.ChunksFileName), records); err != nil {
		t.Fatal(err)
	}
	files := map[string]index.FileEntry{}
	for _, record := range records {
		entry := files[record.RelPath]
		entry.ChunkCount++
		files[record.RelPath] = entry
	}
	metas, err := loadLiveChunkMetas(&index.Manifest{Files: files}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	handle := &revisionHandle{chunks: metas, segmentDirs: []string{dir}}
	t.Cleanup(handle.closeContentFiles)
	return handle
}

func mustRender(t *testing.T, handle *revisionHandle, hits []rankedHit, budget int) string {
	t.Helper()
	got, err := renderHits(handle, hits, budget)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRenderGolden 锁定渲染格式（暗坑 K13）：宿主 AI 依赖该文本形状，
// 任何格式变化都必须显式修改本 golden 并在阶段记录中声明。
func TestRenderGolden(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "c1", RelPath: "internal/app/login.go", Language: "go", StartLine: 10, EndLine: 12, Symbol: "HandleLogin", Content: "func HandleLogin() error {\n\treturn nil\n}"},
		chunkRecord{ID: "c2", RelPath: "docs/guide.md", Language: "markdown", StartLine: 1, EndLine: 2, Content: "# Guide\nlogin flow"},
	)
	hits := []rankedHit{{id: "c1", score: 2.0}, {id: "c2", score: 1.0}}
	got := mustRender(t, handle, hits, 0)
	want := "## internal/app/login.go:10-12 HandleLogin\n" +
		"```go\n" +
		"func HandleLogin() error {\n\treturn nil\n}\n" +
		"```\n\n" +
		"## docs/guide.md:1-2\n" +
		"```markdown\n" +
		"# Guide\nlogin flow\n" +
		"```"
	if got != want {
		t.Fatalf("渲染格式偏离 golden:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestRenderLineNumbersGolden 锁定 D1 Read-parity 试验面格式:开关开启时
// 围栏内逐行携带真实文件行号(cat -n 形状,从 StartLine 起算),header
// 与默认格式一致;默认关闭时与历史逐字节一致(上一 golden 锁定)。
func TestRenderLineNumbersGolden(t *testing.T) {
	t.Setenv(EnvRenderLineNumbers, "1")
	handle := newRenderHandle(t,
		chunkRecord{ID: "c1", RelPath: "internal/app/login.go", Language: "go", StartLine: 10, EndLine: 12, Symbol: "HandleLogin", Content: "func HandleLogin() error {\n\treturn nil\n}"},
	)
	got := mustRender(t, handle, []rankedHit{{id: "c1", score: 2.0}}, 0)
	want := "## internal/app/login.go:10-12 HandleLogin\n" +
		"```go\n" +
		"    10\tfunc HandleLogin() error {\n" +
		"    11\t\treturn nil\n" +
		"    12\t}\n" +
		"```"
	if got != want {
		t.Fatalf("行号渲染格式偏离 golden:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestRenderLineNumbersMergedBlocksStayContinuous 相邻块合并后行号连续。
func TestRenderLineNumbersMergedBlocksStayContinuous(t *testing.T) {
	t.Setenv(EnvRenderLineNumbers, "on")
	handle := newRenderHandle(t,
		chunkRecord{ID: "a", RelPath: "m.go", Language: "go", StartLine: 1, EndLine: 3, Content: "l1\nl2\nl3"},
		chunkRecord{ID: "b", RelPath: "m.go", Language: "go", StartLine: 4, EndLine: 5, Content: "l4\nl5"},
	)
	got := mustRender(t, handle, []rankedHit{{id: "a", score: 1.5}, {id: "b", score: 1.0}}, 0)
	want := "## m.go:1-5\n```go\n" +
		"     1\tl1\n     2\tl2\n     3\tl3\n     4\tl4\n     5\tl5\n```"
	if got != want {
		t.Fatalf("合并块行号错误:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestRenderMergesAdjacentChunks 同文件相邻块合并且内容不重复。
func TestRenderMergesAdjacentChunks(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a", RelPath: "m.go", Language: "go", StartLine: 1, EndLine: 3, Content: "l1\nl2\nl3"},
		chunkRecord{ID: "b", RelPath: "m.go", Language: "go", StartLine: 4, EndLine: 5, Content: "l4\nl5"},
	)
	got := mustRender(t, handle, []rankedHit{{id: "a", score: 1.5}, {id: "b", score: 1.0}}, 0)
	want := "## m.go:1-5\n```go\nl1\nl2\nl3\nl4\nl5\n```"
	if got != want {
		t.Fatalf("相邻块合并错误:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestRenderBudgetTruncation 预算截断保留完整块并声明截断。
func TestRenderBudgetTruncation(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 1, Content: "alpha"},
		chunkRecord{ID: "b", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Content: "beta"},
	)
	got := mustRender(t, handle, []rankedHit{{id: "a", score: 2}, {id: "b", score: 1}}, 30)
	// b.go 的内容不得出现;其路径引用可经 omitted 清单合法携带(18.2)。
	if !containsAll(got, "a.go", "[output truncated by max_output_length: 1 of 2 result blocks shown") || contains(got, "beta") {
		t.Fatalf("预算截断行为错误: %q", got)
	}
}

// TestRenderBudgetPrioritizesFileCoverage(灰度反馈四 §6.2):预算不足时
// 先保证每个命中文件至少一个片段,再回填同文件更多片段——此前纯分序
// 填充让单文件多片段吃光预算,其余点名文件整体消失(现场 33 块只回
// 2 块且同文件)。
func TestRenderBudgetPrioritizesFileCoverage(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a1", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 1, Content: "alpha one"},
		chunkRecord{ID: "a2", RelPath: "a.go", Language: "go", StartLine: 10, EndLine: 10, Content: "alpha two"},
		chunkRecord{ID: "b1", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Content: "beta one"},
		chunkRecord{ID: "c1", RelPath: "c.go", Language: "go", StartLine: 1, EndLine: 1, Content: "gamma one"},
	)
	// a.go 两块分数最高;预算只够 3 块——旧行为回 a1+a2+b1(c.go 消失),
	// 新行为回 a1+b1+c1(每文件先保一块)。
	hits := []rankedHit{{id: "a1", score: 4}, {id: "a2", score: 3}, {id: "b1", score: 2}, {id: "c1", score: 1}}
	got := mustRender(t, handle, hits, 100)
	if !containsAll(got, "a.go", "b.go", "c.go") {
		t.Fatalf("每个命中文件应至少一个片段: %q", got)
	}
	if contains(got, "alpha two") {
		t.Fatalf("同文件第二片段应让位于未展示文件: %q", got)
	}
	if !contains(got, "[output truncated by max_output_length: 3 of 4 result blocks shown") {
		t.Fatalf("截断标记应如实计数: %q", got)
	}
	// 预算充足时全量返回,行为与历史一致。
	full := mustRender(t, handle, hits, 0)
	if !containsAll(full, "alpha one", "alpha two", "beta one", "gamma one") || contains(full, "[output truncated") {
		t.Fatalf("预算充足应全量: %q", full)
	}
}

// TestRenderProducesHitInventory(框架 18.2/S2):渲染同时产出结构化
// hits 清单(path/行区间/symbol/rank/shown)与展示统计——"候选存在但
// 调用方看不到"从此机器可读(cross-file 缺口 19/39 卡 rank6-10、灰度
// 33 块只回 2 块,同源问题)。
func TestRenderProducesHitInventory(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a1", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 3, Symbol: "Alpha", Content: "alpha one\nl2\nl3"},
		chunkRecord{ID: "b1", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Symbol: "Beta", Content: "beta one"},
		chunkRecord{ID: "c1", RelPath: "c.go", Language: "go", StartLine: 5, EndLine: 5, Content: "gamma one"},
	)
	hits := []rankedHit{{id: "a1", score: 3}, {id: "b1", score: 2}, {id: "c1", score: 1}}
	// 预算只够 1 块:inventory 仍覆盖全部候选,shown 如实。
	rendered, err := renderHitsWithInventory(handle, hits, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.hits) != 3 {
		t.Fatalf("inventory 应覆盖全部合并候选: %+v", rendered.hits)
	}
	first := rendered.hits[0]
	if first.Path != "a.go" || first.StartLine != 1 || first.EndLine != 3 || first.Symbol != "Alpha" || first.Rank != 1 || !first.Shown {
		t.Fatalf("首条 hit 元数据错误: %+v", first)
	}
	shown := 0
	for _, h := range rendered.hits {
		if h.Shown {
			shown++
		}
	}
	if shown != rendered.display.ShownBlocks || !rendered.display.Truncated {
		t.Fatalf("展示统计与 inventory 不一致: shown=%d display=%+v", shown, rendered.display)
	}
	if rendered.display.CandidateBlocks != 3 || rendered.display.ShownFiles != shown {
		t.Fatalf("统计字段错误: %+v", rendered.display)
	}
	// 截断时正文尾部列出未展示文件(弱 caller 无结构化访问也能续取)。
	if !strings.Contains(rendered.text, "omitted files:") || !strings.Contains(rendered.text, "b.go:1-1") {
		t.Fatalf("截断应附未展示文件清单: %q", rendered.text)
	}
}

// TestRenderPathsDetailMode(用户候选:路径+行号优先返回,agent 自行
// Read):detail=paths 时正文只有 header 行(零代码围栏),预算约束
// 依旧;inventory/统计照常。
func TestRenderPathsDetailMode(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a1", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 3, Symbol: "Alpha", Content: "alpha one\nl2\nl3"},
		chunkRecord{ID: "b1", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Symbol: "Beta", Content: "beta one"},
	)
	hits := []rankedHit{{id: "a1", score: 3}, {id: "b1", score: 2}}
	rendered, err := renderHitsDetail(handle, hits, 0, detailPaths)
	if err != nil {
		t.Fatal(err)
	}
	want := "## a.go:1-3 Alpha\n## b.go:1-1 Beta"
	if rendered.text != want {
		t.Fatalf("paths 模式正文应只有 header:\n--- want ---\n%s\n--- got ---\n%s", want, rendered.text)
	}
	if len(rendered.hits) != 2 || !rendered.hits[0].Shown || !rendered.hits[1].Shown {
		t.Fatalf("paths 模式 inventory 应全 shown: %+v", rendered.hits)
	}
}

// TestContentPreadCorruptionSurfaces 是暗坑 K47：打开后段文件被篡改导致
// 偏移错位/解码失败时，渲染显式报错而不是返回错误内容。
func TestContentPreadCorruptionSurfaces(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 1, Content: "alpha content here"},
	)
	// 篡改：整体前移文件内容，破坏既有偏移。
	path := filepath.Join(handle.segmentDirs[0], index.ChunksFileName)
	raw, err := readFileForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileForTest(path, append([]byte("garbage-prefix"), raw...)); err != nil {
		t.Fatal(err)
	}
	if _, err := renderHits(handle, []rankedHit{{id: "a", score: 1}}, 0); err == nil {
		t.Fatalf("篡改后渲染应显式报错（K47）")
	}
}

func contains(s string, sub string) bool { return len(s) >= len(sub) && index0(s, sub) >= 0 }

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func index0(s string, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestRenderMergeCapsSpan(外部反馈 2026-09-02 F-01):同文件相邻行窗口
// 链此前无上限合并,整篇文档可合成一块(实录 434 行)并以最高分占据高
// 名次槽位,吞掉默认输出预算的三分之一。合并后单块行数上限为两个代码
// 窗口(2×WindowLines=120):5 个 40 行文档窗口(重叠 10 行)应合成
// 1-100 与 91-160 两块,每块行区间与内容行数仍一致。
func TestRenderMergeCapsSpan(t *testing.T) {
	var records []chunkRecord
	var hits []rankedHit
	for i := 0; i < 5; i++ {
		start := 1 + i*30
		end := start + 39
		lines := make([]string, 0, 40)
		for n := start; n <= end; n++ {
			lines = append(lines, "line "+strconv.Itoa(n))
		}
		id := "w" + strconv.Itoa(i)
		records = append(records, chunkRecord{ID: id, RelPath: "doc.md", Language: "markdown", StartLine: start, EndLine: end, Content: strings.Join(lines, "\n")})
		hits = append(hits, rankedHit{id: id, score: 1.0 / float64(i+1)})
	}
	handle := newRenderHandle(t, records...)
	rendered, err := renderHitsDetail(handle, hits, 0, detailPaths)
	if err != nil {
		t.Fatal(err)
	}
	want := "## doc.md:1-100\n## doc.md:91-160"
	if rendered.text != want {
		t.Fatalf("合并块应受 120 行上限约束:\n--- want ---\n%s\n--- got ---\n%s", want, rendered.text)
	}
	full, err := renderHitsDetail(handle, hits, 0, "full")
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"## doc.md:1-100\n", "## doc.md:91-160\n"} {
		if !strings.Contains(full.text, header) {
			t.Fatalf("full 模式缺少合并块 %q:\n%s", header, full.text)
		}
	}
	// 每块内容行数与 header 区间一致(合并不得产生缺行或重复行)。
	first := full.text[strings.Index(full.text, "## doc.md:1-100"):strings.Index(full.text, "## doc.md:91-160")]
	if got := strings.Count(first, "\nline "); got != 100 {
		t.Fatalf("1-100 块应恰含 100 行内容,实际 %d", got)
	}
}

// TestRenderMarksRerankBoundary(外部反馈 2026-09-02 F-03):精排只覆盖
// 前 rerankHeadLimit 个候选,窗口外候选按融合原序附回。此前正文对二者
// 同形输出,调用方无法分辨第 51 位起的质量断层。现在 hits[] 逐条携带
// reranked/rerank_score/source,正文在最后一个精排块之后插入一行边界
// 标记(paths 与 full 两种模式)。
func TestRenderMarksRerankBoundary(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "a", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 1, Symbol: "A", Content: "alpha"},
		chunkRecord{ID: "b", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Symbol: "B", Content: "beta"},
		chunkRecord{ID: "c", RelPath: "c.go", Language: "go", StartLine: 1, EndLine: 1, Symbol: "C", Content: "gamma"},
	)
	hits := []rankedHit{
		{id: "a", score: 1, reranked: true, rerankScore: 0.91, source: "both"},
		{id: "b", score: 0.5, reranked: true, rerankScore: 0.42, source: "dense"},
		{id: "c", score: 0.33, source: "lexical"},
	}
	paths, err := renderHitsDetail(handle, hits, 0, detailPaths)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := "## a.go:1-1 A\n## b.go:1-1 B\n" + rerankBoundaryMarker + "\n## c.go:1-1 C"
	if paths.text != wantPaths {
		t.Fatalf("paths 模式应在精排边界插标记:\n--- want ---\n%s\n--- got ---\n%s", wantPaths, paths.text)
	}
	if !paths.hits[0].Reranked || paths.hits[0].RerankScore != 0.91 || paths.hits[0].Source != "both" ||
		paths.hits[2].Reranked || paths.hits[2].RerankScore != 0 || paths.hits[2].Source != "lexical" {
		t.Fatalf("hits 应逐条携带 reranked/rerank_score/source: %+v", paths.hits)
	}
	full, err := renderHitsDetail(handle, hits, 0, "full")
	if err != nil {
		t.Fatal(err)
	}
	idxB := strings.Index(full.text, "## b.go:1-1 B")
	idxMarker := strings.Index(full.text, rerankBoundaryMarker)
	idxC := strings.Index(full.text, "## c.go:1-1 C")
	if idxB < 0 || idxMarker < 0 || idxC < 0 || !(idxB < idxMarker && idxMarker < idxC) {
		t.Fatalf("full 模式标记应位于最后一个精排块与首个未精排块之间:\n%s", full.text)
	}
	// 全部候选都经过精排(或没有任何精排)时不插标记。
	allReranked, err := renderHitsDetail(handle, hits[:2], 0, detailPaths)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(allReranked.text, rerankBoundaryMarker) {
		t.Fatalf("无未精排候选时不应出现标记: %s", allReranked.text)
	}
	none, err := renderHitsDetail(handle, []rankedHit{{id: "a", score: 1}, {id: "c", score: 0.5}}, 0, detailPaths)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none.text, rerankBoundaryMarker) {
		t.Fatalf("未启用精排时不应出现标记: %s", none.text)
	}
}
