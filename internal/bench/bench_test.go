package bench

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// K64 自检：手算已知答案的 fixture，指标必须精确一致。
func selfCheckFixture() ([]Query, Qrels, Run) {
	queries := []Query{
		{ID: "q1", Group: "G-exact"},   // 命中位次 1
		{ID: "q2", Group: "G-exact"},   // 命中位次 3
		{ID: "q3", Group: "G-concept"}, // 命中位次 7（>5）
		{ID: "q4", Group: "G-concept"}, // 未命中
	}
	qrels := Qrels{
		"q1": {"d1": 1},
		"q2": {"d3": 1},
		"q3": {"d7": 1},
		"q4": {"d9": 1},
	}
	run := Run{
		"q1": {"d1", "x", "x2"},
		"q2": {"a", "b", "d3"},
		"q3": {"a", "b", "c", "d", "e", "f", "d7"},
		"q4": {"a", "b", "c"},
	}
	return queries, qrels, run
}

// TestMetricsKnownAnswers 是 K64：
// recall@5 = G-exact 1.0, G-concept 0.0, overall 0.5
// recall@10 = G-exact 1.0, G-concept 0.5, overall 0.75
// mrr@10 = G-exact (1 + 1/3)/2 = 0.6667, G-concept (1/7 + 0)/2 = 0.0714,
// overall (1 + 1/3 + 1/7 + 0)/4 = 0.3690
func TestMetricsKnownAnswers(t *testing.T) {
	queries, qrels, run := selfCheckFixture()
	scores := Evaluate(queries, qrels, run)
	want := map[string]float64{
		"recall@5/G-exact":    1.0,
		"recall@5/G-concept":  0.0,
		"recall@5/overall":    0.5,
		"recall@10/G-exact":   1.0,
		"recall@10/G-concept": 0.5,
		"recall@10/overall":   0.75,
		"mrr@10/G-exact":      (1.0 + 1.0/3.0) / 2,
		"mrr@10/G-concept":    (1.0 / 7.0) / 2,
		"mrr@10/overall":      (1.0 + 1.0/3.0 + 1.0/7.0) / 4,
	}
	got := map[string]float64{}
	for _, score := range scores {
		got[score.Metric+"/"+score.Group] = score.Mean
	}
	for key, expected := range want {
		if math.Abs(got[key]-expected) > 1e-12 {
			t.Fatalf("%s: got %.6f want %.6f", key, got[key], expected)
		}
	}
}

// TestMetricsSecondImplementationCrossCheck 是 K64 双实现对拍：
// 用与主实现结构不同的朴素代码重算 recall@5/mrr@10，逐查询一致。
func TestMetricsSecondImplementationCrossCheck(t *testing.T) {
	queries, qrels, run := selfCheckFixture()
	naiveRecall := func(qid string, k int) float64 {
		hitRank := -1
		for i, id := range run[qid] {
			if qrels[qid][id] > 0 {
				hitRank = i + 1
				break
			}
		}
		if hitRank > 0 && hitRank <= k {
			return 1
		}
		return 0
	}
	naiveMRR := func(qid string, k int) float64 {
		for i, id := range run[qid] {
			if i >= k {
				break
			}
			if qrels[qid][id] > 0 {
				return 1 / float64(i+1)
			}
		}
		return 0
	}
	for _, query := range queries {
		if got, want := recallAtK(run[query.ID], qrels[query.ID], 5), naiveRecall(query.ID, 5); got != want {
			t.Fatalf("%s recall@5 对拍失败: %v vs %v", query.ID, got, want)
		}
		if got, want := mrrAtK(run[query.ID], qrels[query.ID], 10), naiveMRR(query.ID, 10); got != want {
			t.Fatalf("%s mrr@10 对拍失败: %v vs %v", query.ID, got, want)
		}
	}
}

// TestEvaluateSkipsMissingQrels 缺 qrels 的查询剔除并如实计数，不稀释均值。
func TestEvaluateSkipsMissingQrels(t *testing.T) {
	queries := []Query{{ID: "q1", Group: "g"}, {ID: "q-noqrels", Group: "g"}}
	qrels := Qrels{"q1": {"d1": 1}}
	run := Run{"q1": {"d1"}, "q-noqrels": {"d1"}}
	scores := Evaluate(queries, qrels, run)
	for _, score := range scores {
		if score.Group == "g" && score.Metric == "recall@5" {
			if score.Mean != 1.0 || score.Queries != 1 || score.SkippedNoQrels != 1 {
				t.Fatalf("缺 qrels 处理错误: %+v", score)
			}
		}
	}
}

// TestPairedBootstrapIdenticalSystems 同一系统对比：Δ=0 且不显著。
func TestPairedBootstrapIdenticalSystems(t *testing.T) {
	queries, qrels, run := selfCheckFixture()
	result, err := PairedBootstrap(queries, qrels, run, run, StandardMetrics()[0], "overall", 2000, 42)
	if err != nil {
		t.Fatal(err)
	}
	if result.Delta != 0 || result.Signific {
		t.Fatalf("同系统应无差异: %+v", result)
	}
}

// TestPairedBootstrapDominatedSystem 全面占优的系统：CI 排除 0，显著。
func TestPairedBootstrapDominatedSystem(t *testing.T) {
	queries := make([]Query, 0, 40)
	qrels := Qrels{}
	worse, better := Run{}, Run{}
	for i := 0; i < 40; i++ {
		qid := "q" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		queries = append(queries, Query{ID: qid, Group: "g"})
		qrels[qid] = map[string]int{"gold": 1}
		worse[qid] = []string{"x1", "x2", "x3", "x4", "x5", "x6"} // 全miss
		better[qid] = []string{"gold"}
	}
	result, err := PairedBootstrap(queries, qrels, worse, better, StandardMetrics()[0], "g", 2000, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Signific || result.Delta != 1.0 || result.CILow <= 0 {
		t.Fatalf("占优系统应显著: %+v", result)
	}
}

// TestPairedBootstrapDeterministic 同 seed 结果逐位一致（run 可重放）。
func TestPairedBootstrapDeterministic(t *testing.T) {
	queries, qrels, run := selfCheckFixture()
	other := Run{"q1": {"x"}, "q2": {"d3"}, "q3": {"d7"}, "q4": {"d9"}}
	a, err := PairedBootstrap(queries, qrels, run, other, StandardMetrics()[2], "overall", 5000, 7)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := PairedBootstrap(queries, qrels, run, other, StandardMetrics()[2], "overall", 5000, 7)
	if a != b {
		t.Fatalf("同 seed 应逐位一致: %+v vs %+v", a, b)
	}
}

// TestLoaderRoundTrip 统一格式读写往返。
func TestLoaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	queriesPath := filepath.Join(dir, "queries.jsonl")
	corpusPath := filepath.Join(dir, "corpus.jsonl")
	qrelsPath := filepath.Join(dir, "qrels.tsv")
	if err := os.WriteFile(queriesPath, []byte(`{"id":"q1","text":"如何登录","group":"G-zh"}
{"id":"q2","text":"parse config"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corpusPath, []byte(`{"id":"d1","text":"func Login() {}","language":"go"}
{"id":"d2","text":"def parse(): pass","language":"python"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qrelsPath, []byte("q1\td1\t1\nq2\td2\t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	queries, err := LoadQueries(queriesPath)
	if err != nil || len(queries) != 2 || queries[0].Group != "G-zh" {
		t.Fatalf("queries: %v %v", queries, err)
	}
	count := 0
	if err := LoadDocs(corpusPath, func(doc Doc) error { count++; return nil }); err != nil || count != 2 {
		t.Fatalf("docs: %d %v", count, err)
	}
	qrels, err := LoadQrels(qrelsPath)
	if err != nil || qrels["q1"]["d1"] != 1 || qrels["q2"]["d2"] != 1 {
		t.Fatalf("qrels: %v %v", qrels, err)
	}
}
