package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir(), "testns", "ws-abc", "profile-1")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// stageBuild 构造一个最小合法 staging：chunks.jsonl + lexical.bleve 目录。
func stageBuild(t *testing.T, store *Store, buildID string, chunksContent string) (string, string) {
	t.Helper()
	staging, err := store.BeginStaging(buildID)
	if err != nil {
		t.Fatal(err)
	}
	chunksPath := filepath.Join(staging, ChunksFileName)
	if err := os.WriteFile(chunksPath, []byte(chunksContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, LexicalDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	sum, err := ChecksumFile(chunksPath)
	if err != nil {
		t.Fatal(err)
	}
	return staging, sum
}

func testManifest(buildID string, previous string, checksum string) *Manifest {
	return &Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		Workspace:        WorkspaceIdentity{CanonicalPath: "/tmp/project"},
		EngineID:         "local-hybrid",
		EngineVersion:    "stage2",
		Revision:         "rev-" + buildID,
		PreviousRevision: previous,
		ChunkerID:        "default",
		ChunkerVersion:   "1",
		LexicalEngine:    "bleve",
		LexicalVersion:   "v2.5.7",
		SegmentID:        buildID,
		Files:            map[string]FileEntry{"main.go": {ContentHash: "h", ChunkCount: 1}},
		Counts:           Counts{Files: 1, Chunks: 1, Bytes: 10},
		ChunksChecksum:   checksum,
		CreatedAt:        time.Now().UTC(),
		ActivatedAt:      time.Now().UTC(),
	}
}

func TestPublishAndResolveRoundTrip(t *testing.T) {
	store := newTestStore(t)
	staging, sum := stageBuild(t, store, "b1", `{"id":"c1"}`+"\n")
	if err := store.Publish(testManifest("b1", "", sum), staging); err != nil {
		t.Fatalf("publish: %v", err)
	}
	manifest, skipped, err := store.ResolveUsable()
	if err != nil || len(skipped) != 0 {
		t.Fatalf("resolve: %v skipped=%v", err, skipped)
	}
	if manifest.Revision != "rev-b1" || manifest.SegmentID != "b1" {
		t.Fatalf("manifest 内容错误: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(store.SegmentPath(manifest), ChunksFileName)); err != nil {
		t.Fatalf("segment 数据缺失: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), stagingDir, "b1")); !os.IsNotExist(err) {
		t.Fatal("发布后 staging 目录应已移走")
	}
}

func TestSecondPublishKeepsPreviousRevision(t *testing.T) {
	store := newTestStore(t)
	stagingA, sumA := stageBuild(t, store, "a", "chunk-a\n")
	if err := store.Publish(testManifest("a", "", sumA), stagingA); err != nil {
		t.Fatal(err)
	}
	stagingB, sumB := stageBuild(t, store, "b", "chunk-b\n")
	if err := store.Publish(testManifest("b", "rev-a", sumB), stagingB); err != nil {
		t.Fatal(err)
	}
	active, _, err := store.ResolveUsable()
	if err != nil || active.Revision != "rev-b" {
		t.Fatalf("active 应为 rev-b: %+v err=%v", active, err)
	}
	previous, err := store.LoadManifest("rev-a")
	if err != nil {
		t.Fatalf("previous manifest 应保留: %v", err)
	}
	if err := store.VerifyManifest(previous); err != nil {
		t.Fatalf("previous segment 应保留且完整: %v", err)
	}
}

func TestResolveFallsBackWhenActiveCorrupted(t *testing.T) {
	store := newTestStore(t)
	stagingA, sumA := stageBuild(t, store, "a", "chunk-a\n")
	if err := store.Publish(testManifest("a", "", sumA), stagingA); err != nil {
		t.Fatal(err)
	}
	stagingB, sumB := stageBuild(t, store, "b", "chunk-b\n")
	if err := store.Publish(testManifest("b", "rev-a", sumB), stagingB); err != nil {
		t.Fatal(err)
	}
	// 破坏 active revision 的 chunks 数据（模拟半写/磁盘损坏）。
	activeManifest, _ := store.LoadManifest("rev-b")
	if err := os.WriteFile(filepath.Join(store.SegmentPath(activeManifest), ChunksFileName), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, skipped, err := store.ResolveUsable()
	if err != nil {
		t.Fatalf("应回退 previous: %v", err)
	}
	if manifest.Revision != "rev-a" {
		t.Fatalf("应回退到 rev-a，got %s", manifest.Revision)
	}
	if len(skipped) != 1 || skipped[0] != "rev-b" {
		t.Fatalf("应记录被跳过的损坏 revision: %v", skipped)
	}
}

func TestResolveHalfWrittenActivePointer(t *testing.T) {
	store := newTestStore(t)
	staging, sum := stageBuild(t, store, "a", "chunk-a\n")
	if err := store.Publish(testManifest("a", "", sum), staging); err != nil {
		t.Fatal(err)
	}
	// 模拟 active.json 半写损坏。
	if err := os.WriteFile(filepath.Join(store.Root(), activeFileName), []byte(`{"revi`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResolveUsable(); err == nil {
		t.Fatal("损坏的 active.json 应显式报错而非静默成功")
	}
}

func TestPublishInterruptedBeforeActiveUpdate(t *testing.T) {
	store := newTestStore(t)
	stagingA, sumA := stageBuild(t, store, "a", "chunk-a\n")
	if err := store.Publish(testManifest("a", "", sumA), stagingA); err != nil {
		t.Fatal(err)
	}
	// 模拟第二次发布在更新 active 前中断：segment 与 manifest 就位但指针未动。
	stagingB, sumB := stageBuild(t, store, "b", "chunk-b\n")
	manifestB := testManifest("b", "rev-a", sumB)
	if err := os.Rename(stagingB, store.SegmentPath(manifestB)); err != nil {
		t.Fatal(err)
	}
	data := mustJSON(manifestB)
	if err := os.WriteFile(filepath.Join(store.Root(), manifestsDir, "rev-b.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil || manifest.Revision != "rev-a" {
		t.Fatalf("中断发布不应影响 active：want rev-a got %+v err=%v", manifest, err)
	}
}

func TestCleanupStagingRemovesOrphans(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.BeginStaging("orphan1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginStaging("orphan2"); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupStaging(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), stagingDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging 应被清空，剩余 %d 项", len(entries))
	}
}

func TestSegmentImmutable(t *testing.T) {
	store := newTestStore(t)
	staging, sum := stageBuild(t, store, "a", "chunk-a\n")
	if err := store.Publish(testManifest("a", "", sum), staging); err != nil {
		t.Fatal(err)
	}
	stagingDup, _ := stageBuild(t, store, "a-dup", "x\n")
	dup := testManifest("a", "", sum)
	if err := store.Publish(dup, stagingDup); err == nil {
		t.Fatal("重复 segment ID 发布应被拒绝（segment 不可覆盖）")
	}
}

func TestResolveWithoutAnyRevision(t *testing.T) {
	store := newTestStore(t)
	if _, _, err := store.ResolveUsable(); err != ErrNoUsableRevision {
		t.Fatalf("空 store 应返回 ErrNoUsableRevision，got %v", err)
	}
}
