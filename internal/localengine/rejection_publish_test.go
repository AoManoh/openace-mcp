package localengine

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestRejectionOnlyGapFillPublishesRejection：前一 revision 的缺口在补缺时被 provider
// 拒绝（返回零向量），本次没有新增任何向量。这次全量构建也要发布：manifest 记下被拒绝
// 的 chunk，覆盖率分母扣除它们，provider_rejected_files 列出该文件。只比较有效向量数时
// 不发布，拒绝数留在 0、SemanticComplete 一直为 false，之后每次内容不变的同步与检索都会
// 再走一遍全量构建。
func TestRejectionOnlyGapFillPublishesRejection(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 1, "fake-model")
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	ctx := context.Background()
	if res, err := e.Sync(ctx, syncRequest(root)); err != nil || res.SemanticCoverage != "100%" {
		t.Fatalf("前置：首建应完整覆盖: %+v err=%v", res, err)
	}
	server.setFailWhen(func(texts []string) bool {
		return len(texts) == 1 && strings.Contains(texts[0], "RejectedLater")
	})
	writeFixture(t, root, "rejected.go", "package app\n\nfunc RejectedLater() {}\n")
	gapped, err := e.Sync(ctx, syncRequest(root))
	if err != nil || gapped.SemanticCoverage == "100%" {
		t.Fatalf("前置：rejected.go 嵌入失败应留下缺口: %+v err=%v", gapped, err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// provider 恢复可达，但对这段内容返回零向量；换新引擎让熔断器回到初始状态。
	server.mu.Lock()
	server.failWhen = nil
	server.zeroWhen = func(text string) bool { return strings.Contains(text, "RejectedLater") }
	server.mu.Unlock()
	e2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close(ctx) })

	filled, err := e2.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := loadActiveManifest(t, e2, root)
	if filled.IndexRevision == gapped.IndexRevision || filled.SemanticCoverage != "100%" ||
		m.RejectedCount != 1 || m.Files["rejected.go"].RejectedChunks != 1 || !m.SemanticComplete() {
		t.Fatalf("只新增拒绝记录的补缺应发布带拒绝记录的 revision: res=%+v manifest_rejected=%d file_rejected=%d complete=%v",
			filled, m.RejectedCount, m.Files["rejected.go"].RejectedChunks, m.SemanticComplete())
	}

	builds := 0
	drainParserPoolsHook = func(engine.IndexStage) { builds++ }
	t.Cleanup(func() { drainParserPoolsHook = nil })
	calls := server.callCount()
	for i := 0; i < 2; i++ {
		res, err := e2.Sync(ctx, syncRequest(root))
		if err != nil || res.IndexRevision != filled.IndexRevision {
			t.Fatalf("内容不变的同步应返回同一 revision: %+v err=%v", res, err)
		}
	}
	found, err := e2.Search(ctx, searchRequest(root, "login configuration"))
	if err != nil {
		t.Fatal(err)
	}
	if found.SemanticCoverage != "100%" || slices.Contains(found.UncoveredFiles, "rejected.go") || !slices.Contains(found.ProviderRejectedFiles, "rejected.go") {
		t.Fatalf("被拒绝的文件应列入 provider_rejected_files、不再算作缺口: coverage=%s uncovered=%v rejected=%v",
			found.SemanticCoverage, found.UncoveredFiles, found.ProviderRejectedFiles)
	}
	if builds != 0 {
		t.Fatalf("拒绝记录发布后，内容不变的同步与检索不应再走全量构建: %d 次", builds)
	}
	for _, text := range server.textsSince(calls) {
		if strings.HasPrefix(text, "This chunk is from ") {
			t.Fatalf("拒绝记录发布后不应再发文档嵌入请求: %q", text[:min(60, len(text))])
		}
	}
}
