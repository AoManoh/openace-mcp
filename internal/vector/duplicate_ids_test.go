package vector

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

// 段内可能有相同 ID 的多行：同一超长行切出内容完全相同的字节片段时，
// 片段的 ID 相同，向量段照常保存每一行。SearchFiltered 的 topK 指不重复的
// ID，每个 ID 取它得分最高的一行。

// withWorkers 临时设置 GOMAXPROCS。SearchFiltered 的评分 worker 数取
// min(GOMAXPROCS, 8)，以此分别覆盖单 worker 与 8 个 worker。
func withWorkers(t *testing.T, n int) {
	t.Helper()
	old := runtime.GOMAXPROCS(n)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
}

// referenceUniqueTopK 是按 ID 去重的参考实现：全量评分后按 score 降序、
// ID 升序、行号升序排序，每个 ID 只取第一次出现的行（该 ID 得分最高的行，
// 同分时行号最小），再取前 topK。
func referenceUniqueTopK(entries []Entry, vectors [][]float32, query []float32, topK int, allow func(id string) bool) []Hit {
	q := append([]float32(nil), query...)
	if err := Normalize(q); err != nil {
		return nil
	}
	type scored struct {
		hit Hit
		row int
	}
	all := make([]scored, 0, len(vectors))
	for i, v := range vectors {
		if allow != nil && !allow(entries[i].ID) {
			continue
		}
		var dot float64
		for j := range v {
			dot += float64(v[j]) * float64(q[j])
		}
		all = append(all, scored{Hit{ID: entries[i].ID, ContentHash: entries[i].ContentHash, Score: dot}, i})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].hit.Score != all[b].hit.Score {
			return all[a].hit.Score > all[b].hit.Score
		}
		if all[a].hit.ID != all[b].hit.ID {
			return all[a].hit.ID < all[b].hit.ID
		}
		return all[a].row < all[b].row
	})
	seen := map[string]bool{}
	hits := []Hit{}
	for _, s := range all {
		if seen[s.hit.ID] {
			continue
		}
		seen[s.hit.ID] = true
		hits = append(hits, s.hit)
		if len(hits) == topK {
			break
		}
	}
	return hits
}

// unitAt 返回与查询 [1,0] 的点积恰为 score 的二维单位向量。
func unitAt(score float64) []float32 {
	return []float32{float32(score), float32(math.Sqrt(1 - score*score))}
}

func searchUnique(t *testing.T, ix *Index, query []float32, topK int, allow func(string) bool) []Hit {
	t.Helper()
	hits, err := ix.SearchFiltered(context.Background(), append([]float32(nil), query...), topK, allow)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, hit := range hits {
		if seen[hit.ID] {
			t.Fatalf("结果含重复 ID %s: %+v", hit.ID, hits)
		}
		seen[hit.ID] = true
	}
	return hits
}

// TestSearchDuplicateIDsKeepBestRow：5 个 ID 各有 12 行、分数各不相同，另有
// 40 个唯一 ID。各 topK 下结果与按 ID 去重的参考实现逐项相同，重复 ID 取
// 最高分行。
func TestSearchDuplicateIDsKeepBestRow(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 100, dim, 11)
	for i := 0; i < 60; i++ {
		entries[i] = Entry{ID: fmt.Sprintf("dup-%d", i%5), ContentHash: fmt.Sprintf("dup-row-%02d", i)}
	}
	query := []float32{0.3, -0.2, 0.9, 0.1}
	for _, workers := range []int{1, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			withWorkers(t, workers)
			ix := writeAndLoad(t, entries, vectors, dim)
			for _, topK := range []int{1, 3, 10, 45, 60} {
				got := searchUnique(t, ix, query, topK, nil)
				want := referenceUniqueTopK(entries, vectors, query, topK, nil)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("topK=%d 与去重参考不一致:\nwant=%v\n got=%v", topK, want, got)
				}
			}
		})
	}
}

// TestSearchDuplicateEvictedThenReappears：x 先以低分出现并被更优的行逐出，
// 之后以最高分再次出现，必须进入结果；y 的两行都低于门槛，不进入结果。
func TestSearchDuplicateEvictedThenReappears(t *testing.T) {
	rows := []struct {
		id    string
		score float64
	}{
		{"x", 0.10}, {"a", 0.50}, {"b", 0.60}, {"c", 0.70}, {"d", 0.80}, {"e", 0.90},
		{"x", 0.95}, {"y", 0.20}, {"y", 0.30},
	}
	entries := make([]Entry, len(rows))
	vectors := make([][]float32, len(rows))
	for i, row := range rows {
		entries[i] = Entry{ID: row.id, ContentHash: fmt.Sprintf("row-%d", i)}
		vectors[i] = unitAt(row.score)
	}
	query := []float32{1, 0}
	for _, workers := range []int{1, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			withWorkers(t, workers)
			ix := writeAndLoad(t, entries, vectors, 2)
			got := searchUnique(t, ix, query, 3, nil)
			var ids []string
			for _, hit := range got {
				ids = append(ids, hit.ID)
			}
			if !reflect.DeepEqual(ids, []string{"x", "e", "d"}) || got[0].ContentHash != "row-6" {
				t.Fatalf("x 应以第 6 行的最高分进入前 3: %+v", got)
			}
			if want := referenceUniqueTopK(entries, vectors, query, 3, nil); !reflect.DeepEqual(got, want) {
				t.Fatalf("与去重参考不一致:\nwant=%v\n got=%v", want, got)
			}
		})
	}
}

// TestSearchDuplicateIDTiesDeterministic：全部行同分时按 ID 升序；同一 ID 的
// 多行同分时取行号最小的一行；8 个 worker 下重复 20 次结果不变。
func TestSearchDuplicateIDTiesDeterministic(t *testing.T) {
	ids := []string{"b", "c", "b", "a", "b", "a", "d", "c", "e", "a", "b", "d"}
	entries := make([]Entry, len(ids))
	vectors := make([][]float32, len(ids))
	for i, id := range ids {
		entries[i] = Entry{ID: id, ContentHash: fmt.Sprintf("%s-row-%02d", id, i)}
		vectors[i] = []float32{1, 0}
	}
	withWorkers(t, 8)
	ix := writeAndLoad(t, entries, vectors, 2)
	want := []Hit{{ID: "a", ContentHash: "a-row-03", Score: 1}, {ID: "b", ContentHash: "b-row-00", Score: 1}, {ID: "c", ContentHash: "c-row-01", Score: 1}}
	for run := 0; run < 20; run++ {
		got := searchUnique(t, ix, []float32{1, 0}, 3, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("第 %d 次：同分应按 ID 升序、同 ID 取最小行号:\nwant=%v\n got=%v", run, want, got)
		}
	}
}

// TestSearchRepeatedSingleLineSample：同一超长行切出 62 个内容相同的字节片段，
// 它们 ID 相同、向量相同，另有一个独立目标。取 60 个不重复候选时两者都在。
func TestSearchRepeatedSingleLineSample(t *testing.T) {
	entries := make([]Entry, 0, 63)
	vectors := make([][]float32, 0, 63)
	for i := 0; i < 62; i++ {
		entries = append(entries, Entry{ID: "repeated", ContentHash: "repeated-hash"})
		vectors = append(vectors, []float32{1, 0})
	}
	entries = append(entries, Entry{ID: "target", ContentHash: "target-hash"})
	vectors = append(vectors, []float32{0.8, 0.6})
	for _, workers := range []int{1, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			withWorkers(t, workers)
			ix := writeAndLoad(t, entries, vectors, 2)
			got := searchUnique(t, ix, []float32{1, 0}, 60, nil)
			if len(got) != 2 || got[0].ID != "repeated" || got[1].ID != "target" {
				t.Fatalf("应返回 repeated 与 target 两个不重复候选: %+v", got)
			}
		})
	}
}

// TestSearchDuplicateIDsRandomized：500 行的 ID 取自 120 个值，向量随机；
// 不同 topK、有无放行谓词、1 与 8 个 worker 下都与去重参考逐项相同。
func TestSearchDuplicateIDsRandomized(t *testing.T) {
	const dim = 8
	for _, seed := range []int64{3, 17, 29} {
		entries, vectors := makeVectors(t, 500, dim, seed)
		rng := rand.New(rand.NewSource(seed))
		for i := range entries {
			id := rng.Intn(120)
			entries[i] = Entry{ID: fmt.Sprintf("id-%03d", id), ContentHash: fmt.Sprintf("row-%03d", i)}
		}
		query := make([]float32, dim)
		for i := range query {
			query[i] = float32(rng.NormFloat64())
		}
		allow := func(id string) bool { return id[len(id)-1] != '7' }
		for _, workers := range []int{1, 8} {
			t.Run(fmt.Sprintf("seed=%d/workers=%d", seed, workers), func(t *testing.T) {
				withWorkers(t, workers)
				ix := writeAndLoad(t, entries, vectors, dim)
				for _, topK := range []int{1, 7, 60, 200} {
					for _, filter := range []func(string) bool{nil, allow} {
						got := searchUnique(t, ix, query, topK, filter)
						want := referenceUniqueTopK(entries, vectors, query, topK, filter)
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("topK=%d filtered=%v 与去重参考不一致:\nwant=%v\n got=%v", topK, filter != nil, want, got)
						}
					}
				}
			})
		}
	}
}
