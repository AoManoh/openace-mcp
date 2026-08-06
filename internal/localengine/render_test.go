package localengine

import (
	"os"
	"path/filepath"
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
	if !containsAll(got, "a.go", "[output truncated by max_output_length]") || contains(got, "b.go") {
		t.Fatalf("预算截断行为错误: %q", got)
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
