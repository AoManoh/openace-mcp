package localengine

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestArtifactKindPathRules 锁定产物类型的机械路径规则(与 C2 实验冻结的
// 分类器一致):tests 看目录段与文件名约定,docs 看扩展名/目录/文件名,其余
// 为 code。规则只影响 artifact_kind 分组次序,误分不隐藏任何候选。
func TestArtifactKindPathRules(t *testing.T) {
	cases := map[string]string{
		"internal/localengine/search.go":           artifactCode,
		"django/core/validators.py":                artifactCode,
		"src/testing/harness.go":                   artifactCode,
		"internal/localengine/search_test.go":      artifactTests,
		"tests/validators/tests.py":                artifactTests,
		"pkg/spec/thing.spec.ts":                   artifactTests,
		"web/__tests__/App.test.tsx":               artifactTests,
		"internal/chunk/testdata/sample.rb":        artifactTests,
		"lib/Foo/FooTest.php":                      artifactTests,
		"src/main/java/com/x/ServiceTests.java":    artifactTests,
		"scripts/test_helpers.py":                  artifactTests,
		"README.md":                                artifactDocs,
		"docs/references/2026-09-02-notes.md":      artifactDocs,
		"doc/manual.rst":                           artifactDocs,
		"CHANGELOG":                                artifactDocs,
		"AUTHORS.txt":                              artifactDocs,
		"internal/localengine/README.md":           artifactDocs,
		"packages/docs-site/src/index.tsx":         artifactCode,
		"cmd/openace-mcp/main.go":                  artifactCode,
		"internal/index/manifest.go":               artifactCode,
		"Documentation/kernel/api.txt":             artifactDocs,
		"internal/reliability/classify_p2_test.go": artifactTests,
	}
	for path, want := range cases {
		if got := artifactKind(path); got != want {
			t.Errorf("%s: want %s got %s", path, want, got)
		}
	}
}

// TestParseArtifactKind:""/any 为不分组;三个类型原样接受;其余按请求类
// 错误拒绝(daemon 面 400,MCP 面工具错误)。
func TestParseArtifactKind(t *testing.T) {
	for _, raw := range []string{"", "any", " any "} {
		if got, err := parseArtifactKind(raw); err != nil || got != artifactAny {
			t.Fatalf("%q: want any, got %q err=%v", raw, got, err)
		}
	}
	for _, raw := range []string{"code", "tests", "docs"} {
		if got, err := parseArtifactKind(raw); err != nil || got != raw {
			t.Fatalf("%q: want itself, got %q err=%v", raw, got, err)
		}
	}
	for _, raw := range []string{"test", "documentation", "CODE", "*"} {
		_, err := parseArtifactKind(raw)
		if err == nil || !engine.IsInvalidRequest(err) {
			t.Fatalf("%q: should be rejected as invalid request, got %v", raw, err)
		}
	}
}

// TestGroupByArtifactKindKeepsOrderAndCandidates:请求类型的候选按原相对
// 顺序排前,其余按原序跟随;候选数与集合不变;any 逐条不变。
func TestGroupByArtifactKindKeepsOrderAndCandidates(t *testing.T) {
	handle := newRenderHandle(t,
		chunkRecord{ID: "t1", RelPath: "tests/a_test.go", Language: "go", StartLine: 1, EndLine: 1, Content: "t1"},
		chunkRecord{ID: "c1", RelPath: "a.go", Language: "go", StartLine: 1, EndLine: 1, Content: "c1"},
		chunkRecord{ID: "d1", RelPath: "docs/a.md", Language: "markdown", StartLine: 1, EndLine: 1, Content: "d1"},
		chunkRecord{ID: "c2", RelPath: "b.go", Language: "go", StartLine: 1, EndLine: 1, Content: "c2"},
		chunkRecord{ID: "t2", RelPath: "b_test.go", Language: "go", StartLine: 1, EndLine: 1, Content: "t2"},
	)
	ordered := []rankedHit{{id: "t1", score: 5}, {id: "c1", score: 4}, {id: "d1", score: 3}, {id: "c2", score: 2}, {id: "t2", score: 1}}
	ids := func(hits []rankedHit) string {
		parts := make([]string, 0, len(hits))
		for _, h := range hits {
			parts = append(parts, h.id)
		}
		return strings.Join(parts, ",")
	}
	if got := ids(groupByArtifactKind(handle, ordered, artifactAny)); got != "t1,c1,d1,c2,t2" {
		t.Fatalf("any 应不改变顺序: %s", got)
	}
	if got := ids(groupByArtifactKind(handle, ordered, artifactCode)); got != "c1,c2,t1,d1,t2" {
		t.Fatalf("code 应把代码按原序排前、其余原序跟随: %s", got)
	}
	if got := ids(groupByArtifactKind(handle, ordered, artifactDocs)); got != "d1,t1,c1,c2,t2" {
		t.Fatalf("docs 分组错误: %s", got)
	}
	if got := ids(groupByArtifactKind(handle, ordered, artifactTests)); got != "t1,t2,c1,d1,c2" {
		t.Fatalf("tests 分组错误: %s", got)
	}
	// 渲染后 hits[] 逐条携带 kind,分组后序分仍单调(渲染排序依据序分)。
	grouped := groupByArtifactKind(handle, ordered, artifactCode)
	rendered, err := renderHitsDetail(handle, grouped, 0, detailPaths)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.hits[0].Path != "a.go" || rendered.hits[0].Kind != artifactCode || rendered.hits[2].Kind != artifactTests || rendered.hits[3].Kind != artifactDocs {
		t.Fatalf("hits 应按分组序并带 kind: %+v", rendered.hits)
	}
}
