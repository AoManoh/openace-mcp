package localengine

import (
	"testing"

	"github.com/AoManoh/openace-mcp/internal/lexical"
)

// TestRenderGolden 锁定渲染格式（暗坑 K13）：宿主 AI 依赖该文本形状，
// 任何格式变化都必须显式修改本 golden 并在阶段记录中声明。
func TestRenderGolden(t *testing.T) {
	handle := &revisionHandle{chunks: map[string]chunkRecord{
		"c1": {ID: "c1", RelPath: "internal/app/login.go", Language: "go", StartLine: 10, EndLine: 12, Symbol: "HandleLogin", Content: "func HandleLogin() error {\n\treturn nil\n}"},
		"c2": {ID: "c2", RelPath: "docs/guide.md", Language: "markdown", StartLine: 1, EndLine: 2, Content: "# Guide\nlogin flow"},
	}}
	hits := []lexical.Hit{{ID: "c1", Score: 2.0}, {ID: "c2", Score: 1.0}}
	got := renderHits(handle, hits, 0)
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

// TestRenderMergesAdjacentChunks 同文件相邻块合并且内容不重复。
func TestRenderMergesAdjacentChunks(t *testing.T) {
	handle := &revisionHandle{chunks: map[string]chunkRecord{
		"a": {ID: "a", RelPath: "m.go", Language: "go", StartLine: 1, EndLine: 3, Content: "l1\nl2\nl3"},
		"b": {ID: "b", RelPath: "m.go", Language: "go", StartLine: 4, EndLine: 5, Content: "l4\nl5"},
	}}
	got := renderHits(handle, []lexical.Hit{{ID: "a", Score: 1.5}, {ID: "b", Score: 1.0}}, 0)
	want := "## m.go:1-5\n```go\nl1\nl2\nl3\nl4\nl5\n```"
	if got != want {
		t.Fatalf("相邻块合并错误:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestRenderBudgetTruncation 预算截断保留完整块并声明截断。
func TestRenderBudgetTruncation(t *testing.T) {
	handle := &revisionHandle{chunks: map[string]chunkRecord{
		"a": {ID: "a", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 1, Content: "alpha"},
		"b": {ID: "b", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Content: "beta"},
	}}
	got := renderHits(handle, []lexical.Hit{{ID: "a", Score: 2}, {ID: "b", Score: 1}}, 30)
	if !containsAll(got, "a.go", "[output truncated by max_output_length]") || contains(got, "b.go") {
		t.Fatalf("预算截断行为错误: %q", got)
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
