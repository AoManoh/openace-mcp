package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeManifestFile 直接落盘一个 manifest（绕过 Publish，用于 v2 模型测试）。
func writeManifestFile(t *testing.T, store *Store, manifest *Manifest) {
	t.Helper()
	if err := writeFileAtomic(filepath.Join(store.root, manifestsDir, manifest.Revision+".json"), mustJSON(manifest)); err != nil {
		t.Fatal(err)
	}
}

// makeSegmentDir 构造带合法 chunks.jsonl 与 lexical 目录的 segment，返回其 checksum。
func makeSegmentDir(t *testing.T, store *Store, segmentID string) string {
	t.Helper()
	dir := store.SegmentPathFor(segmentID)
	if err := os.MkdirAll(filepath.Join(dir, LexicalDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	chunksPath := filepath.Join(dir, ChunksFileName)
	if err := os.WriteFile(chunksPath, []byte(`{"id":"`+segmentID+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := ChecksumFile(chunksPath)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func newV2TestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir(), "ns", "ws", "profile")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestV1ManifestNormalizedOnLoad 是 D1/K43 兼容承诺：Stage 2/3 产出的
// v1 manifest 在新代码下读取即归一为单段 v2 视图，升级零感知。
func TestV1ManifestNormalizedOnLoad(t *testing.T) {
	store := newV2TestStore(t)
	sum := makeSegmentDir(t, store, "seg-v1")
	now := time.Now().UTC()
	writeManifestFile(t, store, &Manifest{
		SchemaVersion: ManifestSchemaV1,
		EngineID:      "local-hybrid", EngineVersion: "stage3",
		Revision: "rev-v1", ChunkerID: "default", ChunkerVersion: "2",
		LexicalEngine: "bleve", LexicalVersion: "test",
		SegmentID: "seg-v1", ChunksChecksum: sum,
		VectorsChecksum: "vsum", VectorsIndexChecksum: "visum", VectorCount: 7,
		Files:     map[string]FileEntry{"a.go": {ContentHash: "h1", ChunkCount: 3}},
		Counts:    Counts{Files: 1, Chunks: 3, Bytes: 10},
		CreatedAt: now, ActivatedAt: now,
	})

	manifest, err := store.LoadManifest("rev-v1")
	if err != nil {
		t.Fatalf("v1 manifest 应可读: %v", err)
	}
	if len(manifest.Segments) != 1 {
		t.Fatalf("应归一为单段: %+v", manifest.Segments)
	}
	segment := manifest.Segments[0]
	if segment.ID != "seg-v1" || segment.ChunksChecksum != sum ||
		segment.VectorsChecksum != "vsum" || segment.VectorsIndexChecksum != "visum" ||
		segment.VectorCount != 7 || segment.Counts.Chunks != 3 {
		t.Fatalf("归一字段不符: %+v", segment)
	}
	if manifest.Files["a.go"].SegmentID != "seg-v1" {
		t.Fatalf("Files 应填充 SegmentID: %+v", manifest.Files["a.go"])
	}
	if store.SegmentPath(manifest) != store.SegmentPathFor("seg-v1") {
		t.Fatalf("SegmentPath 应指向唯一 segment")
	}
	if err := store.VerifyManifest(manifest); err != nil {
		t.Fatalf("归一后校验应通过: %v", err)
	}
}

// TestV2ManifestRoundTrip 是多段 manifest 的读写与校验。
func TestV2ManifestRoundTrip(t *testing.T) {
	store := newV2TestStore(t)
	baseSum := makeSegmentDir(t, store, "seg-base")
	deltaSum := makeSegmentDir(t, store, "seg-delta")
	now := time.Now().UTC()
	writeManifestFile(t, store, &Manifest{
		SchemaVersion: ManifestSchemaV2,
		EngineID:      "local-hybrid", EngineVersion: "stage4",
		Revision: "rev-v2", ChunkerID: "default", ChunkerVersion: "2",
		LexicalEngine: "bleve", LexicalVersion: "test",
		Segments: []SegmentRef{
			{ID: "seg-base", ChunksChecksum: baseSum, Counts: Counts{Files: 2, Chunks: 5}, VectorCount: 5},
			{ID: "seg-delta", ChunksChecksum: deltaSum, Counts: Counts{Files: 1, Chunks: 2}, VectorCount: 2},
		},
		Tombstones: []string{"removed.go"},
		Files: map[string]FileEntry{
			"kept.go":    {ContentHash: "h1", ChunkCount: 3, SegmentID: "seg-base"},
			"changed.go": {ContentHash: "h2", ChunkCount: 2, SegmentID: "seg-delta"},
		},
		Counts:    Counts{Files: 2, Chunks: 5, Bytes: 50},
		CreatedAt: now, ActivatedAt: now,
	})

	manifest, err := store.LoadManifest("rev-v2")
	if err != nil {
		t.Fatalf("v2 manifest 应可读: %v", err)
	}
	if len(manifest.Segments) != 2 || manifest.NewestSegment().ID != "seg-delta" {
		t.Fatalf("段序应保持（最新在后）: %+v", manifest.Segments)
	}
	if !manifest.TombstoneSet()["removed.go"] {
		t.Fatalf("tombstone 应可查: %+v", manifest.Tombstones)
	}
	if err := store.VerifyManifest(manifest); err != nil {
		t.Fatalf("多段校验应通过: %v", err)
	}
	// 任一段损坏即整体校验失败。
	if err := os.WriteFile(filepath.Join(store.SegmentPathFor("seg-base"), ChunksFileName), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyManifest(manifest); err == nil || !strings.Contains(err.Error(), "seg-base") {
		t.Fatalf("损坏段应被指名拦截: %v", err)
	}
}

// TestUnknownSchemaRejected 是暗坑 K43：未来版本显式拒绝而非静默错读。
func TestUnknownSchemaRejected(t *testing.T) {
	manifest := &Manifest{SchemaVersion: 3, Revision: "rev-x", SegmentID: "s", ChunksChecksum: "c"}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "不受支持") {
		t.Fatalf("v3 应显式拒绝: %v", err)
	}
}

// TestV2ValidateRequiresSegments 是 v2 结构完整性。
func TestV2ValidateRequiresSegments(t *testing.T) {
	manifest := &Manifest{SchemaVersion: ManifestSchemaV2, Revision: "rev-x"}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "segments") {
		t.Fatalf("空 segments 应拒绝: %v", err)
	}
	manifest.Segments = []SegmentRef{{ID: "", ChunksChecksum: "c"}}
	if err := manifest.Validate(); err == nil {
		t.Fatalf("空 segment id 应拒绝")
	}
}

// TestRemoveRevisionKeepsSharedSegments 是 D1/K42：delta 链共享 segment
// 的引用计数删除——被后继 revision 引用的段不得随旧 revision 回收。
func TestRemoveRevisionKeepsSharedSegments(t *testing.T) {
	store := newV2TestStore(t)
	baseSum := makeSegmentDir(t, store, "seg-shared")
	deltaSum := makeSegmentDir(t, store, "seg-only-r2")
	now := time.Now().UTC()
	writeManifestFile(t, store, &Manifest{
		SchemaVersion: ManifestSchemaV2, Revision: "rev-1",
		EngineID: "local-hybrid", EngineVersion: "stage4",
		ChunkerID: "default", ChunkerVersion: "2", LexicalEngine: "bleve", LexicalVersion: "test",
		Segments:  []SegmentRef{{ID: "seg-shared", ChunksChecksum: baseSum}},
		CreatedAt: now, ActivatedAt: now,
	})
	writeManifestFile(t, store, &Manifest{
		SchemaVersion: ManifestSchemaV2, Revision: "rev-2", PreviousRevision: "rev-1",
		EngineID: "local-hybrid", EngineVersion: "stage4",
		ChunkerID: "default", ChunkerVersion: "2", LexicalEngine: "bleve", LexicalVersion: "test",
		Segments: []SegmentRef{
			{ID: "seg-shared", ChunksChecksum: baseSum},
			{ID: "seg-only-r2", ChunksChecksum: deltaSum},
		},
		CreatedAt: now, ActivatedAt: now,
	})

	if err := store.RemoveRevision("rev-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.SegmentPathFor("seg-shared")); err != nil {
		t.Fatalf("共享 segment 不得随 rev-1 删除: %v", err)
	}
	if _, err := store.LoadManifest("rev-1"); !os.IsNotExist(err) {
		t.Fatalf("rev-1 manifest 应已删除: %v", err)
	}

	if err := store.RemoveRevision("rev-2"); err != nil {
		t.Fatal(err)
	}
	for _, segment := range []string{"seg-shared", "seg-only-r2"} {
		if _, err := os.Stat(store.SegmentPathFor(segment)); !os.IsNotExist(err) {
			t.Fatalf("无引用 segment %s 应被回收: %v", segment, err)
		}
	}
}
