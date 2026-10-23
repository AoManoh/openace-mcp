package localengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 同步结果状态：revision、文件数、覆盖率、检索能力模式与原因都从同一个
// 明确的 manifest 得出；Deleted 是同步前后文件集合中被移除的路径数。

// TestRepeatedSyncKeepsRevisionCoverage：util.py 的片段被 provider 返回零向量
// 而永久拒绝，覆盖率不足 100%。同一 revision 连续两次 Sync 都返回相同的
// 覆盖率、原因与模式。
func TestRepeatedSyncKeepsRevisionCoverage(t *testing.T) {
	s := newEmbedServer(t, 8)
	s.zeroWhen = func(text string) bool { return strings.Contains(text, "util.py") }
	e := newTestEngineWith(t, embedOptions(s.ts.URL, 8, 16, "result-model"))
	root := newFixtureWorkspace(t)
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first=%s/%s/%s second=%s/%s/%s", first.SemanticCoverage, first.DegradedReason, first.RetrievalMode, second.SemanticCoverage, second.DegradedReason, second.RetrievalMode)
	if first.DegradedReason != "semantic-coverage-partial" || first.SemanticCoverage == "" || first.SemanticCoverage == "100%" {
		t.Fatalf("前置：首次同步应报告覆盖缺口: %+v", first)
	}
	if second.IndexRevision != first.IndexRevision || second.SemanticCoverage != first.SemanticCoverage ||
		second.DegradedReason != first.DegradedReason || second.RetrievalMode != "hybrid" || first.RetrievalMode != "hybrid" {
		t.Fatalf("同一 revision 的两次同步状态应一致: first=%+v second=%+v", first, second)
	}
	if second.Deleted != 0 || second.BuildMode != "" {
		t.Fatalf("没有发布新 revision 时 Deleted 为 0、BuildMode 为空: %+v", second)
	}
}

// TestSyncDeletedCountsRemovedPaths：Deleted 是同步前后 manifest 文件集合中
// 被移除的路径数。首次构建与无变化为 0；删除 5 个文件走 delta、删除 20 个
// 文件触发合并重建都如实计数；改为忽略的文件计入；被改写的文件不计入。
func TestSyncDeletedCountsRemovedPaths(t *testing.T) {
	e := newTestEngine(t)
	root := t.TempDir()
	for i := 0; i < 40; i++ {
		writeFixture(t, root, fmt.Sprintf("doc%02d.txt", i), fmt.Sprintf("Document number %d content.\n", i))
	}
	sync := func(label string) engine.Result {
		t.Helper()
		res, err := e.Sync(context.Background(), syncRequest(root))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		t.Logf("%s: build=%q files=%d deleted=%d", label, res.BuildMode, res.FileCount, res.Deleted)
		return res
	}
	if res := sync("首次构建"); res.Deleted != 0 || res.FileCount != 40 {
		t.Fatalf("首次构建 Deleted 应为 0: %+v", res)
	}
	if res := sync("无变化"); res.Deleted != 0 {
		t.Fatalf("无变化 Deleted 应为 0: %+v", res)
	}
	for i := 0; i < 5; i++ {
		if err := os.Remove(filepath.Join(root, fmt.Sprintf("doc%02d.txt", i))); err != nil {
			t.Fatal(err)
		}
	}
	if res := sync("删除 5 个"); res.Deleted != 5 || res.BuildMode != "delta" || res.FileCount != 35 {
		t.Fatalf("delta 删除 5 个文件应报 deleted=5: %+v", res)
	}
	writeFixture(t, root, "doc10.txt", "Rewritten document content.\n")
	if res := sync("改写 1 个"); res.Deleted != 0 || res.FileCount != 35 {
		t.Fatalf("改写文件不算删除: %+v", res)
	}
	writeFixture(t, root, ".openaceignore", "doc1[1-3].txt\n")
	if res := sync("忽略 3 个"); res.Deleted != 3 {
		t.Fatalf("改为忽略的 3 个文件应计入 deleted: %+v", res)
	}
	for i := 14; i < 34; i++ {
		if err := os.Remove(filepath.Join(root, fmt.Sprintf("doc%02d.txt", i))); err != nil {
			t.Fatal(err)
		}
	}
	if res := sync("删除 20 个"); res.Deleted != 20 || !strings.HasPrefix(res.BuildMode, "full:") {
		t.Fatalf("合并重建时删除 20 个文件应报 deleted=20: %+v", res)
	}
	if res := sync("再次无变化"); res.Deleted != 0 {
		t.Fatalf("后续无变化 Deleted 应为 0: %+v", res)
	}
}

// TestSyncModeShowsRevisionCapability：Sync 的 retrieval_mode 说明返回的
// revision 已建立的检索能力：未配置语义为 lexical；有向量为 hybrid；配置了
// 语义但一个向量也没有时为 lexical，原因为 semantic-index-empty，同步照常
// 发布并返回结果，同时返回错误（这样的 revision 不能用于语义检索）。
func TestSyncModeShowsRevisionCapability(t *testing.T) {
	lexical := newTestEngine(t)
	root := newFixtureWorkspace(t)
	res, err := lexical.Sync(context.Background(), syncRequest(root))
	if err != nil || res.RetrievalMode != "lexical" || res.SemanticCoverage != "" || res.DegradedReason != "" {
		t.Fatalf("纯词法同步应显式 lexical、无覆盖与原因: %+v err=%v", res, err)
	}

	s := newEmbedServer(t, 8)
	hybrid := newTestEngineWith(t, embedOptions(s.ts.URL, 8, 16, "result-model"))
	res, err = hybrid.Sync(context.Background(), syncRequest(newFixtureWorkspace(t)))
	if err != nil || res.RetrievalMode != "hybrid" || res.SemanticCoverage != "100%" {
		t.Fatalf("有向量的 revision 应为 hybrid: %+v err=%v", res, err)
	}

	zero := newEmbedServer(t, 8)
	zero.zeroWhen = func(string) bool { return true }
	none := newTestEngineWith(t, embedOptions(zero.ts.URL, 8, 16, "result-model"))
	res, err = none.Sync(context.Background(), syncRequest(newFixtureWorkspace(t)))
	if err == nil || !strings.Contains(err.Error(), semanticIndexEmptyReason) || res.IndexRevision == "" || res.RetrievalMode != "lexical" || res.SemanticCoverage != "0%" || res.DegradedReason != semanticIndexEmptyReason {
		t.Fatalf("配置了语义但没有向量的 revision 应为 lexical、报告 0%% 覆盖并返回错误: %+v err=%v", res, err)
	}
}

// TestSearchReportsLexicalModeExplicitly：未配置任何 provider 的正常词法检索
// 显式返回 retrieval_mode=lexical，没有覆盖率、原因或横幅。
func TestSearchReportsLexicalModeExplicitly(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RetrievalMode != "lexical" || res.DegradedReason != "" || res.SemanticCoverage != "" || strings.Contains(res.Text, "[DEGRADED]") {
		t.Fatalf("纯词法检索应显式 lexical: %+v", res)
	}
}
