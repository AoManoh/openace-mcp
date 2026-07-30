package lexical

import (
	"context"
	"path/filepath"
	"testing"
)

func testDocs() []Doc {
	return []Doc{
		{ID: "c1", Path: "internal/workspace/syncer.go", Symbol: "Syncer.syncSingleflight", Language: "go", Content: "func (s *Syncer) syncSingleflight(ctx context.Context, key stateKey, reason engine.SyncReason) (engine.Result, error) { mapKey := key.mapKey() }"},
		{ID: "c2", Path: "internal/workspace/syncer.go", Symbol: "Syncer.Search", Language: "go", Content: "func (s *Syncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) { legacy ACE retrieval }"},
		{ID: "c3", Path: "internal/daemon/server.go", Symbol: "Server.routes", Language: "go", Content: "func (s *Server) routes() http.Handler { mux := http.NewServeMux() daemon http endpoints }"},
		{ID: "c4", Path: "README.md", Symbol: "", Language: "markdown", Content: "openACE is a local context engine daemon with workspace sync and retrieval tools."},
		{ID: "c5", Path: "internal/chunk/golang.go", Symbol: "mergeSmallSpans", Language: "go", Content: "func mergeSmallSpans(spans []declSpan, lines []string, maxBytes int) []declSpan { merge adjacent small declarations }"},
	}
}

func buildTestIndex(t *testing.T) *Index {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lexical.bleve")
	if err := Build(context.Background(), dir, testDocs()); err != nil {
		t.Fatalf("build: %v", err)
	}
	idx, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// TestExactSymbolTopHit 是 P2-T07 的业务验收：精确 identifier 查询
// 必须命中定义该符号的 chunk 且排第一。
func TestExactSymbolTopHit(t *testing.T) {
	idx := buildTestIndex(t)
	hits, err := idx.Search(context.Background(), "Syncer.syncSingleflight", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "c1" {
		t.Fatalf("exact symbol 应命中 c1 且排第一: %+v", hits)
	}
}

func TestConceptQueryHits(t *testing.T) {
	idx := buildTestIndex(t)
	hits, err := idx.Search(context.Background(), "daemon http endpoints", 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range hits {
		if hit.ID == "c3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("概念查询应命中 c3: %+v", hits)
	}
}

func TestDocCountAndClose(t *testing.T) {
	idx := buildTestIndex(t)
	count, err := idx.DocCount()
	if err != nil || count != 5 {
		t.Fatalf("doc count = %d err=%v, want 5", count, err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := idx.Search(context.Background(), "anything", 3); err != ErrClosed {
		t.Fatalf("closed index 应返回 ErrClosed，got %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close 应幂等: %v", err)
	}
}

// TestSearchWeightedClauseControl 是 T10a 权重契约：symbol 权重归零后
// 符号命中不再来自 symbol 子句；默认权重路径与 Search 等价。
func TestSearchWeightedClauseControl(t *testing.T) {
	idx := buildTestIndex(t)
	ctx := context.Background()

	// 默认权重 == Search 委托：同查询逐位一致。
	base, err := idx.Search(ctx, "Syncer.syncSingleflight", 5)
	if err != nil {
		t.Fatal(err)
	}
	weighted, err := idx.SearchWeighted(ctx, "Syncer.syncSingleflight", 5, DefaultWeights())
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != len(weighted) {
		t.Fatalf("默认权重应与 Search 等价: %d vs %d", len(base), len(weighted))
	}
	for i := range base {
		if base[i] != weighted[i] {
			t.Fatalf("第 %d 位不一致: %+v vs %+v", i, base[i], weighted[i])
		}
	}

	// 子句开关：构造 symbol 词不出现在 content 的文档（真实场景里符号
	// 通常也在声明行出现，此处特意分离以验证子句路由本身）。
	dir := filepath.Join(t.TempDir(), "clause.bleve")
	docs := []Doc{
		{ID: "s1", Path: "internal/guard/limit.go", Symbol: "quotaGuard", Language: "go", Content: "enforce daily budget limits"},
		{ID: "s2", Path: "internal/guard/other.go", Symbol: "", Language: "go", Content: "unrelated text body"},
	}
	if err := Build(ctx, dir, docs); err != nil {
		t.Fatal(err)
	}
	clauseIdx, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer clauseIdx.Close()
	noSymbol := Weights{Content: 1, Path: 0, Symbol: 0, SymbolExact: 0}
	hits, err := clauseIdx.SearchWeighted(ctx, "quotaGuard", 5, noSymbol)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == "s1" {
			t.Fatalf("symbol 子句关闭后不应经符号命中 s1: %+v", hits)
		}
	}
	withSymbol := Weights{Content: 1, Path: 0, Symbol: 1, SymbolExact: 0}
	hits, err = clauseIdx.SearchWeighted(ctx, "quotaGuard", 5, withSymbol)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range hits {
		if hit.ID == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("symbol 子句开启后应命中 s1: %+v", hits)
	}

	// 全零权重：省略全部子句，空结果而非报错。
	empty, err := idx.SearchWeighted(ctx, "anything", 5, Weights{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("全零权重应返回空: hits=%v err=%v", empty, err)
	}
}

func TestBuildRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var docs []Doc
	for i := 0; i < 2000; i++ {
		docs = append(docs, Doc{ID: string(rune('a'+i%26)) + "-doc", Path: "p", Content: "text"})
	}
	err := Build(ctx, filepath.Join(t.TempDir(), "cancelled.bleve"), docs)
	if err == nil {
		t.Fatal("已取消的 context 应中止构建")
	}
}
