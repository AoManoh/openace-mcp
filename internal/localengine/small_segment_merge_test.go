package localengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
)

// writeLargeBase 写入 40 个 Go 文件（每个两个函数），让基础段远大于单文件
// 改动产生的小段，段数达到阈值时有小段可合并。
func writeLargeBase(t *testing.T, root string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		writeFixture(t, root, fmt.Sprintf("pkg/file%02d.go", i), fmt.Sprintf(
			"package pkg\n\n// Alpha%02d returns a constant used by the base segment.\nfunc Alpha%02d() int {\n\treturn %d\n}\n\n// Beta%02d doubles its input.\nfunc Beta%02d(x int) int {\n\treturn x * %d\n}\n", i, i, i, i, i, i+2))
	}
}

// liveChunkHashes 把 revision 的存活 chunk 按文件列成内容 hash 序列，用来比较
// 两个索引是否服务同样的内容。
func liveChunkHashes(t *testing.T, store *index.Store, manifest *index.Manifest) map[string][]string {
	t.Helper()
	byFile, err := loadLiveChunkRecordsByFile(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]string, len(byFile))
	for path, records := range byFile {
		hashes := make([]string, 0, len(records))
		for _, record := range records {
			hashes = append(hashes, record.ContentHash)
		}
		sort.Strings(hashes)
		out[path] = hashes
	}
	return out
}

// TestSmallSegmentsMergeKeepsBaseSegment：一个小文件反复改写时，段数达到阈值
// 只合并小段，基础段一直不重写；搬运的记录不送审；最终服务的内容与对同一
// 目录全新建索引的结果逐文件一致。
func TestSmallSegmentsMergeKeepsBaseSegment(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := t.TempDir()
	writeLargeBase(t, root)
	writeFixture(t, root, "docs/board.md", "# Board\n\nstatus iteration 0\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	base, _ := loadActiveManifest(t, e, root)
	if len(base.Segments) != 1 {
		t.Fatalf("首建应为单段: %d", len(base.Segments))
	}
	baseID := base.Segments[0].ID

	merged := 0
	for i := 1; i <= 20; i++ {
		marker := fmt.Sprintf("status iteration %d", i)
		writeFixture(t, root, "docs/board.md", "# Board\n\n"+marker+"\n")
		callsBefore := server.callCount()
		result, err := e.Sync(context.Background(), syncRequest(root))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(result.BuildMode, "delta") {
			t.Fatalf("第 %d 次改写不应走全量路径: %s", i, result.BuildMode)
		}
		if result.BuildMode == "delta:merge-small-segments" {
			merged++
		}
		for _, text := range server.textsSince(callsBefore) {
			if !strings.Contains(text, marker) {
				t.Fatalf("第 %d 次改写把未变内容送审: %q", i, text[:min(80, len(text))])
			}
		}
		manifest, _ := loadActiveManifest(t, e, root)
		if manifest.Segments[0].ID != baseID {
			t.Fatalf("第 %d 次改写重写了基础段: %s → %s", i, baseID, manifest.Segments[0].ID)
		}
		if len(manifest.Segments) > compactSegmentThreshold {
			t.Fatalf("第 %d 次改写后段数超过阈值: %d", i, len(manifest.Segments))
		}
		if !manifest.SemanticComplete() {
			t.Fatalf("第 %d 次改写后覆盖不完整: %d/%d", i, manifest.VectorCount, manifest.Counts.Chunks)
		}
	}
	if merged == 0 {
		t.Fatal("20 次改写应至少触发一次小段合并")
	}

	final, store := loadActiveManifest(t, e, root)
	result, err := e.Search(context.Background(), searchRequest(root, "status iteration"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "status iteration 20") || strings.Contains(result.Text, "status iteration 3\n") {
		t.Fatalf("检索应只返回最新版本: %q", result.Text)
	}

	fresh := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	if _, err := fresh.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	freshManifest, freshStore := loadActiveManifest(t, fresh, root)
	got, want := liveChunkHashes(t, store, final), liveChunkHashes(t, freshStore, freshManifest)
	if len(got) != len(want) {
		t.Fatalf("存活文件数不一致: %d vs %d", len(got), len(want))
	}
	for path, hashes := range want {
		if strings.Join(got[path], ",") != strings.Join(hashes, ",") {
			t.Fatalf("%s 的存活 chunk 与全新建索引不一致", path)
		}
	}
	if final.Counts != freshManifest.Counts || final.VectorCount != freshManifest.VectorCount {
		t.Fatalf("计数不一致: %+v/%d vs %+v/%d", final.Counts, final.VectorCount, freshManifest.Counts, freshManifest.VectorCount)
	}
}

// TestSmallSegmentMergeFallsBackWhenSegmentUnreadable：被合并小段的向量读不回
// 时改走全量合并，不让合并后的覆盖率下降；全量合并照既有规则重嵌坏段的内容，
// 构建成功、覆盖完整，其余已付费内容不送审。chunk 记录缺失由 VerifyManifest
// 在选路前拦下（整个 revision 不可用），这里改坏的是 VerifyManifest 不校验的
// 向量数据。
func TestSmallSegmentMergeFallsBackWhenSegmentUnreadable(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := t.TempDir()
	writeLargeBase(t, root)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < compactSegmentThreshold; i++ {
		writeFixture(t, root, fmt.Sprintf("docs/note%d.md", i), fmt.Sprintf("# Note %d\n\nnote body %d\n", i, i))
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, store := loadActiveManifest(t, e, root)
	if len(manifest.Segments) != compactSegmentThreshold {
		t.Fatalf("应达到阈值段数: %d", len(manifest.Segments))
	}
	vectorsPath := filepath.Join(store.SegmentPathFor(manifest.Segments[1].ID), "vectors.dat")
	info, err := os.Stat(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vectorsPath, make([]byte, info.Size()), 0o600); err != nil {
		t.Fatal(err)
	}

	writeFixture(t, root, "docs/trigger.md", "# Trigger\n\ntrigger body\n")
	callsBefore := server.callCount()
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if result.BuildMode != "full:compaction-segments" {
		t.Fatalf("小段向量读不回时应改走全量合并: %s", result.BuildMode)
	}
	for _, text := range server.textsSince(callsBefore) {
		if !strings.Contains(text, "trigger body") && !strings.Contains(text, "note body 1") {
			t.Fatalf("全量合并只应重嵌本次改动与坏段的内容: %q", text[:min(80, len(text))])
		}
	}
	compacted, _ := loadActiveManifest(t, e, root)
	if len(compacted.Segments) != 1 || !compacted.SemanticComplete() {
		t.Fatalf("全量合并后应为单段且覆盖完整: segments=%d %d/%d", len(compacted.Segments), compacted.VectorCount, compacted.Counts.Chunks)
	}
}

// TestSmallSegmentsToMergeSelection 核对小段的划分与放弃条件。
func TestSmallSegmentsToMergeSelection(t *testing.T) {
	segments := func(sizes ...int) *index.Manifest {
		manifest := &index.Manifest{}
		for i, size := range sizes {
			manifest.Segments = append(manifest.Segments, index.SegmentRef{ID: fmt.Sprintf("s%d", i), Counts: index.Counts{Chunks: size}})
		}
		return manifest
	}
	got := smallSegmentsToMerge(segments(1000, 10, 20, 130, 5, 3, 2, 1))
	want := []string{"s1", "s2", "s4", "s5", "s6", "s7"}
	if len(got) != len(want) {
		t.Fatalf("小段应为 %v，实际 %v", want, got)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("小段应为 %v，实际 %v", want, got)
		}
	}
	if got := smallSegmentsToMerge(segments(10, 9, 8, 7, 6, 5, 4, 3)); got != nil {
		t.Fatalf("段大小相近时没有小段，应返回 nil: %v", got)
	}
	if got := smallSegmentsToMerge(segments(1000, 900, 800, 700, 600, 500, 400, 5)); got != nil {
		t.Fatalf("保留段加新段仍达阈值时应返回 nil: %v", got)
	}
}
