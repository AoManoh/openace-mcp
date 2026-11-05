package localengine

import (
	"context"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

// TestEstimateDeltaBase：delta 估算加上既有索引一项（每个既有分段 chunk 约 0.9 KiB）与小段搬运
// 一项（搬进新段的 chunk 与改动文件的 chunk 同样计 5.7 KiB）。系数按 2026-11-05 三组原型读数拟合。
// 全量估算不变。
func TestEstimateDeltaBase(t *testing.T) {
	base := estimateBuild(true, nil, nil).withDeltaBase(100000, 0)
	if want := int64(100000 * estimatedExistingPeakBytesPerChunk); base.peak != want {
		t.Fatalf("改动为 0、既有 10 万 chunk 时估算应为 %d 字节，实际 %d", want, base.peak)
	}
	if base.peak>>20 != 87 {
		t.Fatalf("约 88 MiB，实际 %d MiB", base.peak>>20)
	}
	carried := estimateBuild(true, nil, nil).withDeltaBase(0, 1000)
	if carried.chunks != 1000 || carried.peak != 1000*estimatedPeakBytesPerChunk {
		t.Fatalf("搬运的 chunk 应计入新段: %+v", carried)
	}
	full := estimateBuild(false, syntheticAssets(1, 800*1000), nil)
	if full.peak != 1000*estimatedPeakBytesPerChunk {
		t.Fatalf("全量估算不应改变: %+v", full)
	}
}

// TestEstimateDeltaMailynBoundary：按生产 mailyn-cli 的既有分段规模（351,872 个 chunk，底座约
// 309 MiB），改动文件内容约 98 MiB 起判为重型，约 90 MiB 不是（改动前要约 140 MiB 才算重型）。
func TestEstimateDeltaMailynBoundary(t *testing.T) {
	const existing = 351872
	heavy := estimateBuild(true, nil, syntheticAssets(1, 98<<20)).withDeltaBase(existing, 0)
	light := estimateBuild(true, nil, syntheticAssets(1, 90<<20)).withDeltaBase(existing, 0)
	if !heavy.heavy() || light.heavy() {
		t.Fatalf("98 MiB 应为重型、90 MiB 不应为: heavy=%d MiB light=%d MiB", heavy.peak>>20, light.peak>>20)
	}
	if old := estimateBuild(true, nil, syntheticAssets(1, 98<<20)); old.heavy() {
		t.Fatalf("对照：不加既有索引项时 98 MiB 不是重型（%d MiB）", old.peak>>20)
	}
}

// TestCarriedChunkCount：搬运量只算当前版本位于被合并小段、本次没有改动或删除的文件，
// 与 loadSmallSegmentCarry 的取舍相同。
func TestCarriedChunkCount(t *testing.T) {
	previous := &index.Manifest{Files: map[string]index.FileEntry{
		"a.go": {SegmentID: "big", ChunkCount: 100},
		"b.go": {SegmentID: "small1", ChunkCount: 3},
		"c.go": {SegmentID: "small1", ChunkCount: 4},
		"d.go": {SegmentID: "small2", ChunkCount: 5},
		"e.go": {SegmentID: "small2", ChunkCount: 6},
	}}
	merge := map[string]bool{"small1": true, "small2": true}
	changed := []workspace.ContextAsset{{RelPath: "c.go"}}
	if got := carriedChunkCount(previous, merge, changed, []string{"e.go"}); got != 3+5 {
		t.Fatalf("搬运量应为 8，实际 %d", got)
	}
	if got := carriedChunkCount(previous, nil, nil, nil); got != 0 {
		t.Fatalf("不合并小段时搬运量应为 0，实际 %d", got)
	}
}

// TestPriorSegmentChunksDedupesTwoHops：既有分段按 active 与 previous 两个 revision 去重后求和，
// 与 loadPriorVectors 打开的段一致（首建段被两个 revision 共用，只算一次）。
func TestPriorSegmentChunksDedupesTwoHops(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "prior-chunks"))
	root := priorLoadWorkspace(t, priorLoadFiles)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	appendFunctionToAll(t, root, priorLoadFiles)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	if len(manifest.Segments) != 2 {
		t.Fatalf("应为首建段加 delta 段: %d", len(manifest.Segments))
	}
	want := int64(manifest.Segments[0].Counts.Chunks + manifest.Segments[1].Counts.Chunks)
	if got := priorSegmentChunks(store, manifest); got != want {
		t.Fatalf("两跳去重后应为 %d，实际 %d", want, got)
	}
}
