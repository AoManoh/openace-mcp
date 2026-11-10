package localengine

import (
	"context"
	"testing"
	"time"
)

// TestSearchTimingsIncludeVectorLoad：加载向量段的耗时写进 Timings.VectorLoadMs，各
// 分项之和与 TotalMs 对得上；同一 revision 的第二次检索不再加载。改动前这段时间
// 不在任何分项里，大工作区的检索结果会出现 total 比分项之和多出几秒的情况。
func TestSearchTimingsIncludeVectorLoad(t *testing.T) {
	const dim = 8
	const loadDelay = 80 * time.Millisecond
	loadVectorSegmentHook = func(string) { time.Sleep(loadDelay) }
	t.Cleanup(func() { loadVectorSegmentHook = nil })
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "carry-model"))
	root := newFixtureWorkspace(t)
	ctx := context.Background()

	first, err := e.Search(ctx, searchRequest(root, "establish user session"))
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	tm := first.Timings
	if tm == nil {
		t.Fatal("search should carry stage timings")
	}
	if tm.VectorLoadMs < loadDelay.Milliseconds() {
		t.Fatalf("vector load time should be reported in VectorLoadMs (>= %dms): %+v", loadDelay.Milliseconds(), tm)
	}
	parts := tm.SyncMs + tm.LexicalMs + tm.QueryEmbedMs + tm.VectorLoadMs + tm.VectorMs + tm.FuseMs + tm.RerankMs + tm.RenderMs
	if gap := tm.TotalMs - parts; gap > loadDelay.Milliseconds()/2 {
		t.Fatalf("stage timings should add up to total: total=%d parts=%d gap=%d (%+v)", tm.TotalMs, parts, gap, tm)
	}

	second, err := e.Search(ctx, searchRequest(root, "establish user session"))
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if second.Timings == nil || second.Timings.VectorLoadMs >= loadDelay.Milliseconds() {
		t.Fatalf("second search on the same revision should not load vectors again: %+v", second.Timings)
	}
}
