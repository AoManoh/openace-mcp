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

// writeAndLoad 写入临时目录并载入；索引随测试结束自动 Close（映射
// 生命周期契约）。
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
	t.Cleanup(func() { _ = ix.Close() })
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

// referenceTopKFiltered 是带放行谓词的参考实现：全量评分排序后，在
// 放行子序列中取前 topK（与"放行集内 topK"语义等价，独立于生产实现）。
func referenceTopKFiltered(entries []Entry, vectors [][]float32, query []float32, topK int, allow func(id string) bool) []Hit {
	all := referenceTopK(entries, vectors, query, len(vectors))
	hits := make([]Hit, 0, topK)
	for _, hit := range all {
		if allow != nil && !allow(hit.ID) {
			continue
		}
		hits = append(hits, hit)
		if len(hits) >= topK {
			break
		}
	}
	return hits
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

// TestSearchFilteredMatchesReference 用随机谓词/边界 K 值对照参考实现，
// 锁定"放行集内 topK"语义与 (score desc, ID asc) 全序在选择替换后不漂移。
func TestSearchFilteredMatchesReference(t *testing.T) {
	const count, dim = 500, 16
	entries, vectors := makeVectors(t, count, dim, 11)
	ix := writeAndLoad(t, entries, vectors, dim)
	rng := rand.New(rand.NewSource(12))
	predicates := map[string]func(id string) bool{
		"nil":    nil,
		"half":   func(id string) bool { return id[len(id)-1]%2 == 0 },
		"sparse": func(id string) bool { return strings.HasSuffix(id, "7") },
		"none":   func(id string) bool { return false },
		"all":    func(id string) bool { return true },
	}
	for name, allow := range predicates {
		for _, topK := range []int{1, 10, count - 1, count, count + 50} {
			for round := 0; round < 5; round++ {
				query := make([]float32, dim)
				for j := range query {
					query[j] = float32(rng.NormFloat64())
				}
				want := referenceTopKFiltered(entries, vectors, query, topK, allow)
				q := make([]float32, dim)
				copy(q, query)
				got, err := ix.SearchFiltered(context.Background(), q, topK, allow)
				if err != nil {
					t.Fatalf("%s topK=%d: %v", name, topK, err)
				}
				if len(got) == 0 && len(want) == 0 {
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s topK=%d round=%d 与参考不一致:\ngot=%v\nwant=%v", name, topK, round, got, want)
				}
			}
		}
	}
}

// TestSearchAllTiedScores 全并列分数下 topK 必须严格按 ID 升序截取。
func TestSearchAllTiedScores(t *testing.T) {
	const dim, count = 4, 64
	unit := []float32{0, 1, 0, 0}
	entries := make([]Entry, count)
	vectors := make([][]float32, count)
	for i := range entries {
		entries[i] = Entry{ID: fmt.Sprintf("id-%03d", count-1-i)} // 逆序写入
		vectors[i] = append([]float32{}, unit...)
	}
	ix := writeAndLoad(t, entries, vectors, dim)
	got, err := ix.Search(context.Background(), []float32{0, 1, 0, 0}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("topK=7 应返回 7 条: %d", len(got))
	}
	for i, hit := range got {
		want := fmt.Sprintf("id-%03d", i)
		if hit.ID != want {
			t.Fatalf("全并列第 %d 位应为 %s: %s", i, want, hit.ID)
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

// TestMappedAndHeapModesBitIdentical 锁定两种驻留形态的位级等价:同一
// 文件分别以 mmap 与 heap 解码载入,Row 位模式与检索结果必须逐位一致。
func TestMappedAndHeapModesBitIdentical(t *testing.T) {
	const count, dim, topK = 200, 8, 12
	entries, vectors := makeVectors(t, count, dim, 21)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := load(dir, dim, dataSum, idxSum, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.Close()
	heap, err := load(dir, dim, dataSum, idxSum, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer heap.Close()
	if !mapped.Mapped() {
		t.Fatal("mmap 形态应报告文件后备驻留")
	}
	if heap.Mapped() {
		t.Fatal("heap 形态不应报告文件后备驻留")
	}
	for i := 0; i < count; i++ {
		if !reflect.DeepEqual(mapped.Row(i), heap.Row(i)) {
			t.Fatalf("第 %d 行两形态位模式不一致", i)
		}
	}
	query := make([]float32, dim)
	for j := range query {
		query[j] = float32(j%3 + 1)
	}
	q1, q2 := make([]float32, dim), make([]float32, dim)
	copy(q1, query)
	copy(q2, query)
	hitsMapped, err := mapped.Search(context.Background(), q1, topK)
	if err != nil {
		t.Fatal(err)
	}
	hitsHeap, err := heap.Search(context.Background(), q2, topK)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hitsMapped, hitsHeap) {
		t.Fatalf("两形态检索结果不一致:\nmmap=%v\nheap=%v", hitsMapped, hitsHeap)
	}
}

// TestMappedModeRejectsCorruption 映射形态必须保持整读 checksum 拒载
// 语义(K25 自愈链依赖 Load 报错)。
func TestMappedModeRejectsCorruption(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 8, dim, 22)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(dir, DataFileName)
	raw, _ := os.ReadFile(dataPath)
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir, dim, dataSum, idxSum, 0, true); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("映射形态篡改应被 checksum 拦截: %v", err)
	}
}

// TestRowReaderMatchesLoadBits 按行读取必须与整读装载位级一致(复用
// 拷贝的位保真依据),且行拷贝在 Close 后仍有效。
func TestRowReaderMatchesLoadBits(t *testing.T) {
	const count, dim = 32, 8
	entries, vectors := makeVectors(t, count, dim, 31)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Load(dir, dim, dataSum, idxSum, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	reader, err := OpenRowReader(dir, dim, idxSum)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reader.Entries(), ix.Entries()) {
		t.Fatal("行映射应与整读一致")
	}
	rows := make([][]float32, count)
	for i := 0; i < count; i++ {
		row, err := reader.ReadRow(i)
		if err != nil {
			t.Fatalf("ReadRow(%d): %v", i, err)
		}
		if !reflect.DeepEqual(row, ix.Row(i)) {
			t.Fatalf("第 %d 行按行读取与整读位模式不一致", i)
		}
		rows[i] = row
	}
	if _, err := reader.ReadRow(count); err == nil {
		t.Fatal("越界行号应拒绝")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadRow(0); err == nil {
		t.Fatal("Close 后读取应返回显式错误")
	}
	// 行拷贝独立于 reader 生命周期。
	if !reflect.DeepEqual(rows[0], ix.Row(0)) {
		t.Fatal("行拷贝应在 Close 后仍有效")
	}
}

// TestRowReaderRejectsCorruptedRow 范数探针必须拦截损坏行(按行路径
// 没有全文件 checksum,这是它的显式完整性底线)。
func TestRowReaderRejectsCorruptedRow(t *testing.T) {
	const count, dim = 4, 8
	entries, vectors := makeVectors(t, count, dim, 32)
	dir := t.TempDir()
	_, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	// 把第 2 行全部清零(范数 0,必被探针拒绝);其余行不受影响。
	dataPath := filepath.Join(dir, DataFileName)
	raw, _ := os.ReadFile(dataPath)
	for b := dim * 4 * 2; b < dim*4*3; b++ {
		raw[b] = 0
	}
	if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRowReader(dir, dim, idxSum)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ReadRow(2); err == nil {
		t.Fatal("损坏行应被范数探针拒绝")
	}
	if _, err := reader.ReadRow(1); err != nil {
		t.Fatalf("健康行不应受损坏行影响: %v", err)
	}
}

// TestCloseThenDeleteAndUseAfterClose 钉住生命周期秩序:Close 后段目录
// 可删除(Windows 上已映射文件不可删,先 munmap 再删是 GC/compaction 的
// 必要顺序);Close 后检索返回显式错误而非脏读,重复 Close 幂等。
func TestCloseThenDeleteAndUseAfterClose(t *testing.T) {
	const dim = 4
	entries, vectors := makeVectors(t, 6, dim, 23)
	dir := t.TempDir()
	dataSum, idxSum, err := Write(dir, dim, entries, vectors)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Load(dir, dim, dataSum, idxSum, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("Close 后段目录应可删除: %v", err)
	}
	if _, err := ix.Search(context.Background(), []float32{1, 0, 0, 0}, 3); err == nil {
		t.Fatal("Close 后检索应返回显式错误")
	}
}
