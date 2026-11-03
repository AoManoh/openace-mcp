package index

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// twoSegmentManifest 落盘一个两段的 v2 manifest 并按正常路径读回。
func twoSegmentManifest(t *testing.T, store *Store) *Manifest {
	t.Helper()
	baseSum := makeSegmentDir(t, store, "seg-base")
	deltaSum := makeSegmentDir(t, store, "seg-delta")
	now := time.Now().UTC()
	writeManifestFile(t, store, &Manifest{
		SchemaVersion: ManifestSchemaV2,
		EngineID:      "local-hybrid", EngineVersion: "stage4",
		Revision: "rev-v2", ChunkerID: "default", ChunkerVersion: "2",
		LexicalEngine: "bleve", LexicalVersion: "test",
		Segments: []SegmentRef{
			{ID: "seg-base", ChunksChecksum: baseSum, Counts: Counts{Files: 1, Chunks: 1}},
			{ID: "seg-delta", ChunksChecksum: deltaSum, Counts: Counts{Files: 1, Chunks: 1}},
		},
		Files: map[string]FileEntry{
			"kept.go":    {ContentHash: "h1", ChunkCount: 1, SegmentID: "seg-base"},
			"changed.go": {ContentHash: "h2", ChunkCount: 1, SegmentID: "seg-delta"},
		},
		Counts:    Counts{Files: 2, Chunks: 2},
		CreatedAt: now, ActivatedAt: now,
	})
	manifest, err := store.LoadManifest("rev-v2")
	if err != nil {
		t.Fatalf("manifest 应可读: %v", err)
	}
	return manifest
}

// countChecksums 把分段校验函数换成计数包装，测试结束时还原。
func countChecksums(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	original := checksumFileForVerify
	checksumFileForVerify = func(path string) (string, error) {
		calls.Add(1)
		return original(path)
	}
	t.Cleanup(func() { checksumFileForVerify = original })
	return &calls
}

// TestVerifyManifestCachesUnchangedSegments：分段发布后不再改写，同一进程内分段
// 文件的大小、修改时间与 inode 都没变时只算一次校验和。改动前每次 VerifyManifest
// 都对全部分段重算，一个 6 段、共 390 MB 的工作区每次检索重复三次、每次约 0.33 秒。
func TestVerifyManifestCachesUnchangedSegments(t *testing.T) {
	store := newV2TestStore(t)
	manifest := twoSegmentManifest(t, store)
	calls := countChecksums(t)
	for i := 1; i <= 3; i++ {
		if err := store.VerifyManifest(manifest); err != nil {
			t.Fatalf("第 %d 次校验应通过: %v", i, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("两个分段各应只算一次校验和，实际 %d 次", got)
	}
}

// TestVerifyManifestRechecksRewrittenSegment：缓存记录之后分段文件被改写，下一次
// 校验必须重新计算并指名报出损坏的分段；恢复原内容后重新计算并通过。
func TestVerifyManifestRechecksRewrittenSegment(t *testing.T) {
	store := newV2TestStore(t)
	manifest := twoSegmentManifest(t, store)
	if err := store.VerifyManifest(manifest); err != nil {
		t.Fatalf("首次校验应通过: %v", err)
	}
	path := filepath.Join(store.SegmentPathFor("seg-base"), ChunksFileName)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyManifest(manifest); err == nil || !strings.Contains(err.Error(), "seg-base") {
		t.Fatalf("改写后的分段应被指名拦截: %v", err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countChecksums(t)
	if err := store.VerifyManifest(manifest); err != nil {
		t.Fatalf("恢复原内容后应通过: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("恢复后只应重算被改写过的分段，实际计算 %d 次", got)
	}
}

// TestRemoveRevisionForgetsVerifiedSegments：GC 删除分段目录时同时删除校验记录，
// 校验记录不随 daemon 运行时长无限增长。
func TestRemoveRevisionForgetsVerifiedSegments(t *testing.T) {
	store := newV2TestStore(t)
	manifest := twoSegmentManifest(t, store)
	if err := store.VerifyManifest(manifest); err != nil {
		t.Fatalf("首次校验应通过: %v", err)
	}
	if err := store.RemoveRevision(manifest.Revision); err != nil {
		t.Fatalf("删除 revision: %v", err)
	}
	store.verifiedMu.Lock()
	remaining := len(store.verified)
	store.verifiedMu.Unlock()
	if remaining != 0 {
		t.Fatalf("分段删除后不应保留校验记录，剩 %d 条", remaining)
	}
}
