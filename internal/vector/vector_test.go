package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// makeVectors 生成 count 个确定性伪随机向量并归一化。
func makeVectors(t *testing.T, count, dim int, seed int64) ([]Entry, [][]float32) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	entries := make([]Entry, count)
	vectors := make([][]float32, count)
	for i := range vectors {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		if err := Normalize(v); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		vectors[i] = v
		entries[i] = Entry{ID: fmt.Sprintf("chunk-%04d", i), ContentHash: fmt.Sprintf("hash-%04d", i)}
	}
	return entries, vectors
}

// writeAndLoad 写入临时目录并载入。
func writeAndLoad(t *testing.T, entries []Entry, vectors [][]float32, dim int) *Index {
	t.Helper()
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	ix, err := Load(dir, dim, dataSum, idxSum, 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return ix
}

// referenceTopK 是独立的单线程 brute-force 参考实现（插入排序取 top-k）。
func referenceTopK(entries []Entry, vectors [][]float32, query []float32, topK int) []Hit {
	q := make([]float32, len(query))
	copy(q, query)
	if err := Normalize(q); err != nil {
		return nil
	}
	hits := make([]Hit, 0, len(vectors))
	for i, v := range vectors {
		var dot float64
		for j := range v {
			dot += float64(v[j]) * float64(q[j])
		}
		hits = append(hits, Hit{ID: entries[i].ID, ContentHash: entries[i].ContentHash, Score: dot})
	}
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].ID < hits[b].ID
	})
	if topK > len(hits) {
		topK = len(hits)
	}
	return hits[:topK]
}

// TestSearchMatchesBruteForceGolden 是 §11.2 的 exact 一致性验收。
func TestSearchMatchesBruteForceGolden(t *testing.T) {
	const count, dim, topK = 500, 16, 10
	entries, vectors := makeVectors(t, count, dim, 1)
	ix := writeAndLoad(t, entries, vectors, dim)
	rng := rand.New(rand.NewSource(2))
	for round := 0; round < 20; round++ {
		query := make([]float32, dim)
		for j := range query {
			query[j] = float32(rng.NormFloat64())
		}
		want := referenceTopK(entries, vectors, query, topK)
		q := make([]float32, dim)
		copy(q, query)
		got, err := ix.Search(context.Background(), q, topK)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d 与参考实现不一致:\ngot=%v\nwant=%v", round, got[:3], want[:3])
		}
	}
}

// TestSearchDeterministic 是暗坑 K27 的确定性断言。
func TestSearchDeterministic(t *testing.T) {
	const count, dim, topK = 300, 8, 15
	entries, vectors := makeVectors(t, count, dim, 3)
	ix := writeAndLoad(t, entries, vectors, dim)
	query := make([]float32, dim)
	for j := range query {
		query[j] = float32(j + 1)
	}
	var first []Hit
	for round := 0; round < 100; round++ {
		q := make([]float32, dim)
		copy(q, query)
		got, err := ix.Search(context.Background(), q, topK)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if first == nil {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("第 %d 次结果与首次不一致", round)
		}
	}
}

// TestTieBreakByID 构造同分向量验证 tie-break 稳定（K27）。
func TestTieBreakByID(t *testing.T) {
	const dim = 4
	unit := []float32{1, 0, 0, 0}
	entries := []Entry{{ID: "zz"}, {ID: "aa"}, {ID: "mm"}}
	vectors := [][]float32{append([]float32{}, unit...), append([]float32{}, unit...), append([]float32{}, unit...)}
	ix := writeAndLoad(t, entries, vectors, dim)
	got, err := ix.Search(context.Background(), []float32{1, 0, 0, 0}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	if !reflect.DeepEqual(ids, []string{"aa", "mm", "zz"}) {
		t.Fatalf("同分应按 ID 升序: %v", ids)
	}
}

func TestSearchCancellation(t *testing.T) {
	const count, dim = 50_000, 8
	entries, vectors := makeVectors(t, count, dim, 4)
	ix := writeAndLoad(t, entries, vectors, dim)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	query := []float32{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := ix.Search(ctx, query, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应中止扫描（§11.2）: %v", err)
	}
}

func TestEnvelopeExceeded(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 11, dim, 5)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := Load(dir, dim, dataSum, idxSum, 10); !errors.Is(err, ErrEnvelopeExceeded) {
		t.Fatalf("超 envelope 应显式拒绝（§18）: %v", err)
	}
}

func TestLoadRejectsCorruption(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 8, dim, 6)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 篡改 vectors.dat 内容 → checksum 拒绝（K25）。
	dataPath := filepath.Join(dir, DataFileName)
	raw, _ := os.ReadFile(dataPath)
	raw[0] ^= 0xFF
	if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, dim, dataSum, idxSum, 0); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("篡改应被 checksum 拦截: %v", err)
	}

	// 截断 → 尺寸校验拒绝（K24；跳过 checksum 以命中尺寸分支）。
	if err := os.WriteFile(dataPath, raw[:len(raw)-4], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, dim, "", idxSum, 0); err == nil || !strings.Contains(err.Error(), "尺寸") {
		t.Fatalf("尺寸不符应被拦截: %v", err)
	}
}

func TestLoadRejectsDimensionMismatch(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 3, dim, 7)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := Load(dir, 8, dataSum, idxSum, 0); err == nil || !strings.Contains(err.Error(), "维度") {
		t.Fatalf("维度不符应拒绝（K24 禁止混用）: %v", err)
	}
}

func TestNormalizeRejectsInvalid(t *testing.T) {
	if err := Normalize([]float32{0, 0, 0}); err == nil {
		t.Fatalf("零向量应拒绝（K35）")
	}
	if err := Normalize([]float32{1, float32(math.NaN())}); err == nil {
		t.Fatalf("NaN 应拒绝（K35）")
	}
	if err := Normalize([]float32{1, float32(math.Inf(1))}); err == nil {
		t.Fatalf("Inf 应拒绝（K35）")
	}
	v := []float32{3, 4}
	if err := Normalize(v); err != nil {
		t.Fatalf("合法向量: %v", err)
	}
	if math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Fatalf("归一化结果错误: %v", v)
	}
}

func TestWriteRejectsUnnormalizedAndMisaligned(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Write(dir, 2, []Entry{{ID: "a"}}, [][]float32{{3, 4}}); err == nil {
		t.Fatalf("未归一化输入应拒绝（D5 单点归一保证跨 revision 位级一致）")
	}
	if _, _, err := Write(dir, 2, []Entry{{ID: "a"}}, [][]float32{{1}}); err == nil {
		t.Fatalf("维度不符应拒绝")
	}
	if _, _, err := Write(dir, 2, []Entry{{ID: "a"}, {ID: "b"}}, [][]float32{{1, 0}}); err == nil {
		t.Fatalf("条目错位应拒绝")
	}
}

func TestEmptyIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, 4, nil, nil)
	if err != nil {
		t.Fatalf("空集应合法（K10 同族）: %v", err)
	}
	ix, err := Load(dir, 4, dataSum, idxSum, 0)
	if err != nil || ix.Count() != 0 {
		t.Fatalf("空索引载入: count=%d err=%v", ix.Count(), err)
	}
	hits, err := ix.Search(context.Background(), []float32{1, 0, 0, 0}, 5)
	if err != nil || hits != nil {
		t.Fatalf("空索引检索应返回空: %v %v", hits, err)
	}
}

func TestRowReturnsStoredBits(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 5, dim, 8)
	ix := writeAndLoad(t, entries, vectors, dim)
	for i := range vectors {
		if !reflect.DeepEqual(ix.Row(i), vectors[i]) {
			t.Fatalf("Row(%d) 应与写入位级一致（复用拷贝依据，D2）", i)
		}
	}
}
