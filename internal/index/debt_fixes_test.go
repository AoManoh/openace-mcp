package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCycleManifests 构造 previous 链成环的两个损坏 manifest。
func writeCycleManifests(t *testing.T, store *Store) {
	t.Helper()
	now := time.Now().UTC()
	for _, pair := range [][2]string{{"rev-a", "rev-b"}, {"rev-b", "rev-a"}} {
		manifest := &Manifest{
			SchemaVersion: ManifestSchemaVersion,
			EngineID:      "local-hybrid", EngineVersion: "test",
			Revision: pair[0], PreviousRevision: pair[1],
			ChunkerID: "default", ChunkerVersion: "2",
			LexicalEngine: "bleve", LexicalVersion: "test",
			SegmentID: "seg-" + pair[0],
			// checksum 指向不存在的数据 → VerifyManifest 必然失败 → 沿链回退。
			ChunksChecksum: "deadbeef",
			CreatedAt:      now, ActivatedAt: now,
		}
		if err := writeFileAtomic(filepath.Join(store.root, manifestsDir, pair[0]+".json"), mustJSON(manifest)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFileAtomic(filepath.Join(store.root, activeFileName), mustJSON(activePointer{Revision: "rev-a"})); err != nil {
		t.Fatal(err)
	}
}

// TestResolveUsableSurvivesManifestCycle 是 review S3：被外部编辑成环的
// previous 链必须有界返回，不得挂起 daemon。
func TestResolveUsableSurvivesManifestCycle(t *testing.T) {
	store, err := NewStore(t.TempDir(), "ns", "ws", "profile")
	if err != nil {
		t.Fatal(err)
	}
	writeCycleManifests(t, store)
	done := make(chan struct{})
	var resolveErr error
	var skipped []string
	go func() {
		_, skipped, resolveErr = store.ResolveUsable()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveUsable 在成环链上未返回（S3）")
	}
	if !errors.Is(resolveErr, ErrNoUsableRevision) {
		t.Fatalf("应返回无可用 revision: %v", resolveErr)
	}
	if len(skipped) == 0 || len(skipped) > MaxRevisionChain {
		t.Fatalf("skipped 应有界: %d", len(skipped))
	}
}

// TestCleanupOrphanSegments 是 review S5：无 manifest 引用的 segment
// 在启动清理时回收，被引用者保留。
func TestCleanupOrphanSegments(t *testing.T) {
	store, err := NewStore(t.TempDir(), "ns", "ws", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// 通过正常发布产生被引用 segment。
	staging, err := store.BeginStaging("live")
	if err != nil {
		t.Fatal(err)
	}
	chunksPath := filepath.Join(staging, ChunksFileName)
	if err := os.WriteFile(chunksPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, LexicalDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	sum, err := ChecksumFile(chunksPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manifest := &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		EngineID:      "local-hybrid", EngineVersion: "test",
		Revision: "rev-live", ChunkerID: "default", ChunkerVersion: "2",
		LexicalEngine: "bleve", LexicalVersion: "test",
		SegmentID: "live", ChunksChecksum: sum,
		CreatedAt: now, ActivatedAt: now,
	}
	if err := store.Publish(manifest, staging); err != nil {
		t.Fatal(err)
	}
	// 模拟 Publish 在 rename 后中断留下的孤儿 segment（无 manifest）。
	orphan := filepath.Join(store.root, segmentsDir, "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "chunks.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.CleanupOrphanSegments(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤儿 segment 应被回收: %v", err)
	}
	if _, err := os.Stat(store.SegmentPath(manifest)); err != nil {
		t.Fatalf("被引用 segment 不得误删: %v", err)
	}
}
