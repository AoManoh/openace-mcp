package lexical

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	bleve "github.com/blevesearch/bleve/v2"
)

// 以下测试的命中数都超过每页的 livePageSize（128），SearchLive 必须跨页。

// buildSegments 把每组文档建成一个段并聚合打开，模拟多段 revision。
func buildSegments(t *testing.T, segments ...[]Doc) *Index {
	t.Helper()
	dirs := make([]string, 0, len(segments))
	for _, docs := range segments {
		dir := filepath.Join(t.TempDir(), "lexical.bleve")
		if err := Build(context.Background(), dir, docs); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	idx, err := OpenMulti(dirs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func notDeleted(id string) bool { return !strings.HasPrefix(id, "deleted-") }

func hitIDs(hits []Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

// docsWith 生成 n 个内容相同的文档，ID 为 prefix-序号。
func docsWith(prefix string, n int, content string) []Doc {
	docs := make([]Doc, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%03d", prefix, i)
		docs = append(docs, Doc{ID: id, Path: id + ".txt", Content: content})
	}
	return docs
}

// TestSearchLiveMatchesSingleQueryAcrossPages：三个段各有 30 条已删除、30 条
// 存活记录，外加三段共有的同一 ID，共 183 个物理命中，跨两页。分页得到的
// 存活且唯一的结果与一次性取全部命中再按 score 降序、ID 升序去重过滤的结果
// 逐项相同；同分时 ID 升序。
func TestSearchLiveMatchesSingleQueryAcrossPages(t *testing.T) {
	var segments [][]Doc
	for segment := 0; segment < 3; segment++ {
		docs := []Doc{{ID: "shared", Path: "shared.txt", Content: "cursorneedle content"}}
		docs = append(docs, docsWith(fmt.Sprintf("deleted-%d", segment), 30, "cursorneedle content")...)
		docs = append(docs, docsWith(fmt.Sprintf("live-%d", segment), 30, "cursorneedle content")...)
		segments = append(segments, docs)
	}
	idx := buildSegments(t, segments...)

	request := bleve.NewSearchRequestOptions(buildQuery("cursorneedle", DefaultWeights(), ""), 1000, 0, false)
	request.SortBy([]string{"-_score", "_id"})
	full, err := idx.idx.SearchInContext(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	seen := map[string]bool{}
	for _, hit := range full.Hits {
		if notDeleted(hit.ID) && !seen[hit.ID] {
			want = append(want, hit.ID)
			seen[hit.ID] = true
		}
	}
	got, err := idx.SearchLive(context.Background(), "cursorneedle", 1000, DefaultWeights(), "", notDeleted)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("physical_hits=%d page_size=%d live_unique=%d", len(full.Hits), livePageSize, len(got))
	if len(full.Hits) <= livePageSize || len(want) != 91 || !reflect.DeepEqual(hitIDs(got), want) {
		t.Fatalf("跨页结果应与一次性查询的存活去重结果一致:\nwant=%v\n got=%v", want, hitIDs(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score == got[i].Score && got[i-1].ID > got[i].ID {
			t.Fatalf("同分时应按 ID 升序: %v", hitIDs(got))
		}
	}
}

// TestSearchLiveFillsTopKPastDeadAndDuplicateHits：200 条得分更高的已删除记录
// 占满第一页，跨段重复的同一 ID 也排在前面，仍要按存活且唯一的 ID 收满 topK。
func TestSearchLiveFillsTopKPastDeadAndDuplicateHits(t *testing.T) {
	old := docsWith("deleted", 200, "fillneedle fillneedle fillneedle fillneedle")
	newer := docsWith("live", 3, "fillneedle and other words here")
	for _, docs := range []*[]Doc{&old, &newer} {
		*docs = append(*docs, Doc{ID: "dup", Path: "dup.go", Content: "fillneedle fillneedle fillneedle"})
	}
	idx := buildSegments(t, old, newer)
	got, err := idx.SearchLive(context.Background(), "fillneedle", 3, DefaultWeights(), "", notDeleted)
	if err != nil {
		t.Fatal(err)
	}
	ids := hitIDs(got)
	if len(ids) != 3 || ids[0] != "dup" {
		t.Fatalf("应收满 3 个存活且唯一的 ID，首位是得分最高的 dup: %v", ids)
	}
	unique := map[string]bool{}
	for _, id := range ids {
		if unique[id] || !notDeleted(id) {
			t.Fatalf("结果含重复或已删除 ID: %v", ids)
		}
		unique[id] = true
	}
}

// TestSearchLiveScansUntilExhausted：300 条已删除记录得分都高于唯一的存活记录，
// 要翻三页；分页不按页数或扫描倍数截止，一直取到命中耗尽，返回这条存活记录。
func TestSearchLiveScansUntilExhausted(t *testing.T) {
	docs := docsWith("deleted", 300, "scanneedle scanneedle scanneedle")
	docs = append(docs, Doc{ID: "live-last", Path: "live-last.go", Content: "scanneedle plus several unrelated words to lower the score"})
	idx := buildSegments(t, docs)
	got, err := idx.SearchLive(context.Background(), "scanneedle", 5, DefaultWeights(), "", notDeleted)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hitIDs(got), []string{"live-last"}) {
		t.Fatalf("应取到命中耗尽并返回唯一的存活记录: %v", hitIDs(got))
	}
}

// TestSearchLivePathPrefix：前缀在 Bleve 查询里过滤，存活谓词再去掉前缀内
// 150 条已删除的记录。
func TestSearchLivePathPrefix(t *testing.T) {
	var docs []Doc
	for i := 0; i < 150; i++ {
		id := fmt.Sprintf("deleted-pkg-%03d", i)
		docs = append(docs, Doc{ID: id, Path: fmt.Sprintf("pkg/old%03d.go", i), Content: "prefixneedle prefixneedle"})
	}
	docs = append(docs,
		Doc{ID: "live-pkg", Path: "pkg/target.go", Content: "prefixneedle once among more words"},
		Doc{ID: "live-outside", Path: "other/target.go", Content: "prefixneedle prefixneedle prefixneedle"},
	)
	idx := buildSegments(t, docs)
	got, err := idx.SearchLive(context.Background(), "prefixneedle", 5, DefaultWeights(), "pkg", notDeleted)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hitIDs(got), []string{"live-pkg"}) {
		t.Fatalf("前缀内只应返回存活的 live-pkg: %v", hitIDs(got))
	}
}

// TestSearchLiveChecksLivenessBeforeDedup：去重集合只记被接纳的存活 ID，已删除
// 的 ID 不记录，每次出现都重新查存活谓词。三个段各有同一个已删除 ID gone
// 和同一个存活 ID kept：gone 被查 3 次且不进入结果，kept 被查 3 次、只接纳一次。
func TestSearchLiveChecksLivenessBeforeDedup(t *testing.T) {
	var segments [][]Doc
	for segment := 0; segment < 3; segment++ {
		docs := docsWith(fmt.Sprintf("filler-%d", segment), 50, "dedupneedle filler text with extra words")
		docs = append(docs,
			Doc{ID: "gone", Path: "gone.txt", Content: "dedupneedle dedupneedle"},
			Doc{ID: "kept", Path: "kept.txt", Content: "dedupneedle dedupneedle"},
		)
		segments = append(segments, docs)
	}
	idx := buildSegments(t, segments...)
	var mu sync.Mutex
	calls := map[string]int{}
	live := func(id string) bool {
		mu.Lock()
		calls[id]++
		mu.Unlock()
		return id != "gone"
	}
	got, err := idx.SearchLive(context.Background(), "dedupneedle", 200, DefaultWeights(), "", live)
	if err != nil {
		t.Fatal(err)
	}
	ids := hitIDs(got)
	if len(ids) != 151 || ids[0] != "kept" || calls["gone"] != 3 || calls["kept"] != 3 {
		t.Fatalf("已删除 ID 每次出现都应重新判定、不入去重集合: results=%d first=%v calls=%v", len(ids), ids[:1], map[string]int{"gone": calls["gone"], "kept": calls["kept"]})
	}
}

// TestSearchLiveStopsOnCancel：ctx 已取消时返回取消错误，不返回部分结果。
func TestSearchLiveStopsOnCancel(t *testing.T) {
	idx := buildSegments(t, docsWith("deleted", 10, "cancelneedle"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := idx.SearchLive(ctx, "cancelneedle", 5, DefaultWeights(), "", notDeleted)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("取消后应返回 context.Canceled 且没有结果: hits=%v err=%v", got, err)
	}
}

// TestSearchLiveReportsPartialMemberFailure：多段里有一段检索失败时，Bleve 把
// 失败记在 SearchResult.Status 并返回其余段的命中；这里必须返回错误，不能
// 把部分段的结果当作完整候选。
func TestSearchLiveReportsPartialMemberFailure(t *testing.T) {
	first := []Doc{{ID: "a", Path: "a.go", Content: "statusneedle"}}
	second := []Doc{{ID: "b", Path: "b.go", Content: "statusneedle"}}
	idx := buildSegments(t, first, second)
	if err := idx.members[1].Close(); err != nil {
		t.Fatal(err)
	}
	// 已关闭的段从成员表移除，测试收尾的 Close 不再重复关闭它（Bleve 重复
	// 关闭会 panic）；聚合检索句柄仍引用它，检索时该段报错。
	idx.members = idx.members[:1]
	got, err := idx.SearchLive(context.Background(), "statusneedle", 5, DefaultWeights(), "", nil)
	if err == nil {
		t.Fatalf("部分段检索失败时应返回错误，实际返回 %v", hitIDs(got))
	}
	t.Logf("partial failure error: %v", err)
}
