package lexical

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
)

// explainJSON 把一次命中的分数解释序列化为文本，供分支断言使用。
func explainJSON(t *testing.T, expl *search.Explanation) string {
	t.Helper()
	if expl == nil {
		t.Fatal("命中缺少分数解释：请求未开启 Explain")
	}
	raw, err := json.Marshal(expl)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertBM25Explanation 要求解释里出现 Bleve BM25 分支的专有文字（as per bm25
// model 与 saturation/avgFieldLength 项），且不出现 TF-IDF 分支。
func assertBM25Explanation(t *testing.T, label string, text string) {
	t.Helper()
	if strings.Contains(text, "tf-idf") {
		t.Fatalf("%s: 实际评分走了 TF-IDF 分支: %s", label, text)
	}
	for _, marker := range []string{"as per bm25 model", "saturation(term:", "avgFieldLength="} {
		if !strings.Contains(text, marker) {
			t.Fatalf("%s: 分数解释缺少 BM25 分支标记 %q: %s", label, marker, text)
		}
	}
}

// termModels 收集解释树里每个词项打分节点声明的评分模型（"as per X model"）。
func termModels(expl *search.Explanation, out []string) []string {
	if expl == nil {
		return out
	}
	if start := strings.Index(expl.Message, "as per "); start >= 0 {
		rest := expl.Message[start+len("as per "):]
		if end := strings.Index(rest, " model"); end >= 0 {
			out = append(out, rest[:end])
		}
	}
	for _, child := range expl.Children {
		out = termModels(child, out)
	}
	return out
}

// buildWithScoringModel 复刻 Build 的写盘过程，只把 mapping 的评分模型换成
// model；model 为空即旧二进制的产物（Bleve 默认 TF-IDF）。
func buildWithScoringModel(t *testing.T, dir string, model string, docs []Doc) {
	t.Helper()
	m := blevemapping().(*mapping.IndexMappingImpl)
	m.ScoringModel = model
	idx, err := bleve.New(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	batch := idx.NewBatch()
	for _, doc := range docs {
		fields := indexFields{Content: doc.Content, Path: doc.Path, Symbol: doc.Symbol, PathRaw: doc.Path, SymbolRaw: doc.Symbol, Language: doc.Language}
		if err := batch.Index(doc.ID, fields); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Batch(batch); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestBuildReopenScoresWithBM25：经 Build 写盘、关闭句柄、再用 Open 读回
// 之后，持久化 mapping 必须是 bm25，实际检索的分数解释必须走 BM25 分支。
// mapping 未设置评分模型的旧实现在这里得到 scoring_model="" 与
// "as per tf-idf model"。
func TestBuildReopenScoresWithBM25(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lexical.bleve")
	docs := []Doc{
		{ID: "a", Path: "a.go", Content: "needle needle needle"},
		{ID: "b", Path: "b.go", Content: "needle other words in a longer body"},
	}
	if err := Build(context.Background(), dir, docs); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer ix.Close()
	persisted, ok := ix.idx.Mapping().(*mapping.IndexMappingImpl)
	if !ok {
		t.Fatalf("持久化 mapping 类型意外: %T", ix.idx.Mapping())
	}
	q := bleve.NewMatchQuery("needle")
	q.SetField("content")
	req := bleve.NewSearchRequestOptions(q, 10, 0, true)
	res, err := ix.idx.Search(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("前置：两篇文档都应命中: %d", len(res.Hits))
	}
	// 先断言实际分数解释，再断言持久化字段：失败信息直接给出实际走的分支。
	for _, hit := range res.Hits {
		assertBM25Explanation(t, "persisted scoring_model="+persisted.ScoringModel+" hit "+hit.ID, explainJSON(t, hit.Expl))
	}
	if persisted.ScoringModel != "bm25" {
		t.Fatalf("落盘重开后的持久化 scoring_model=%q，期望 bm25", persisted.ScoringModel)
	}
}

// TestProductionQueryExplainIsBM25AfterReopen：对生产检索用的同一查询（默认
// 子句权重、含 symbol_raw 精确子句与路径前缀收敛）开启 Explain，落盘重开后
// 每个词项打分节点都声明 bm25；分数与 SearchWeightedPrefix 返回的逐位一致，
// 证明解释的就是生产排序所用的分数。
func TestProductionQueryExplainIsBM25AfterReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lexical.bleve")
	if err := Build(context.Background(), dir, testDocs()); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	cases := []struct{ query, prefix string }{
		{"Syncer.syncSingleflight", ""},
		{"daemon http endpoints", ""},
		{"Syncer Search engine", "internal/workspace"},
	}
	for _, tc := range cases {
		production, err := ix.SearchWeightedPrefix(context.Background(), tc.query, 10, DefaultWeights(), tc.prefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(production) == 0 {
			t.Fatalf("前置：%q 应有命中", tc.query)
		}
		res, err := ix.idx.Search(bleve.NewSearchRequestOptions(buildQuery(tc.query, DefaultWeights(), tc.prefix), 10, 0, true))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) != len(production) {
			t.Fatalf("%q: Explain 请求命中数 %d 与生产 %d 不一致", tc.query, len(res.Hits), len(production))
		}
		explained := make(map[string]float64, len(res.Hits))
		for _, hit := range res.Hits {
			explained[hit.ID] = hit.Score
			models := termModels(hit.Expl, nil)
			if len(models) == 0 {
				t.Fatalf("%q hit %s: 解释里没有词项打分节点: %s", tc.query, hit.ID, explainJSON(t, hit.Expl))
			}
			for _, model := range models {
				if model != ScoringModel {
					t.Fatalf("%q hit %s: 词项按 %s 评分: %s", tc.query, hit.ID, model, explainJSON(t, hit.Expl))
				}
			}
		}
		for _, hit := range production {
			if score, ok := explained[hit.ID]; !ok || score != hit.Score {
				t.Fatalf("%q hit %s: 生产分数 %v 与 Explain 分数 %v 不一致", tc.query, hit.ID, hit.Score, score)
			}
		}
	}
}

// TestMultiSegmentAliasScoresWithBM25：两个经 Build 写盘的段经 OpenMulti 聚合
// 后，每个命中都按 BM25 评分；统计量按成员局部计算（delta 段命中的 maxDocs
// 等于该段文档数），与 Index 注释所述一致，没有开启 Bleve 的 GlobalScoring。
func TestMultiSegmentAliasScoresWithBM25(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base.bleve")
	deltaDir := filepath.Join(root, "delta.bleve")
	var base []Doc
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("filler text number %d about config parsing", i)
		if i%4 == 0 {
			content += " needle"
		}
		base = append(base, Doc{ID: fmt.Sprintf("base-%02d", i), Path: fmt.Sprintf("base/f%02d.go", i), Content: content})
	}
	delta := []Doc{
		{ID: "delta-00", Path: "delta/a.go", Content: "needle appears once in this delta document"},
		{ID: "delta-01", Path: "delta/b.go", Content: "short delta"},
	}
	if err := Build(context.Background(), baseDir, base); err != nil {
		t.Fatal(err)
	}
	if err := Build(context.Background(), deltaDir, delta); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenMulti([]string{baseDir, deltaDir})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	res, err := ix.idx.Search(bleve.NewSearchRequestOptions(buildQuery("needle", DefaultWeights(), ""), 50, 0, true))
	if err != nil {
		t.Fatal(err)
	}
	sawDelta := false
	for _, hit := range res.Hits {
		text := explainJSON(t, hit.Expl)
		assertBM25Explanation(t, "alias hit "+hit.ID, text)
		if hit.ID == "delta-00" {
			sawDelta = true
			if !strings.Contains(text, "maxDocs=2)") {
				t.Fatalf("delta 段命中应使用成员局部文档数 2: %s", text)
			}
		}
	}
	if !sawDelta || len(res.Hits) != 6 {
		t.Fatalf("前置：基段 5 篇加 delta 段 1 篇命中，实际 %d 篇 delta=%v", len(res.Hits), sawDelta)
	}
}

// TestOpenRejectsSegmentsNotScoredWithBM25：旧二进制写出的 TF-IDF 段（mapping
// 未设置评分模型）单独打开、或与 BM25 段放进同一 alias，都按
// ErrScoringModelMismatch 失败，错误指明目录与持久化取值。
func TestOpenRejectsSegmentsNotScoredWithBM25(t *testing.T) {
	root := t.TempDir()
	tfidfDir := filepath.Join(root, "tfidf.bleve")
	bm25Dir := filepath.Join(root, "bm25.bleve")
	buildWithScoringModel(t, tfidfDir, "", testDocs())
	if err := Build(context.Background(), bm25Dir, testDocs()); err != nil {
		t.Fatal(err)
	}
	_, err := Open(tfidfDir)
	if !errors.Is(err, ErrScoringModelMismatch) || !strings.Contains(err.Error(), tfidfDir) || !strings.Contains(err.Error(), "tf-idf") {
		t.Fatalf("TF-IDF 段应被拒绝并指明目录与取值: %v", err)
	}
	if _, err := OpenMulti([]string{bm25Dir, tfidfDir}); !errors.Is(err, ErrScoringModelMismatch) {
		t.Fatalf("BM25 与 TF-IDF 混合的 alias 应被拒绝: %v", err)
	}
	tfidfExplicit := filepath.Join(root, "tfidf-explicit.bleve")
	buildWithScoringModel(t, tfidfExplicit, "tf-idf", testDocs())
	if _, err := Open(tfidfExplicit); !errors.Is(err, ErrScoringModelMismatch) {
		t.Fatalf("显式 tf-idf 段同样应被拒绝: %v", err)
	}
	ix, err := OpenMulti([]string{bm25Dir})
	if err != nil {
		t.Fatalf("拒绝路径不得影响同目录 BM25 段随后的正常打开: %v", err)
	}
	_ = ix.Close()
}
