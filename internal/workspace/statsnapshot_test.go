package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func snapshotMeta() StatSnapshotMeta {
	return StatSnapshotMeta{Workspace: "/srv/repo", Subtree: "default-v9+lex-bm25+emb-x", ChunkerVersion: "9"}
}

// TestStatSnapshotRoundTrip 保存后加载得到相同的条目与命中结果;窗口内哈希的结果本来就
// 不进缓存,也不会出现在快照里。
func TestStatSnapshotRoundTrip(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	a := statKey{size: 10, mtimeNs: past.UnixNano(), ctimeNs: past.UnixNano(), ino: 11, dev: 2}
	b := statKey{size: 20, mtimeNs: past.Add(time.Minute).UnixNano()}
	cache := NewStatCache()
	cache.store("a.go", a, "blob-a", now)
	cache.store("dir/b.md", b, "blob-b", now)
	cache.store("racy.go", statKey{size: 5, mtimeNs: now.UnixNano()}, "blob-r", now)
	path := filepath.Join(t.TempDir(), StatSnapshotFile)
	saved, err := cache.SaveSnapshot(path, snapshotMeta(), now)
	if err != nil || saved.Entries != 2 {
		t.Fatalf("save: %v %+v", err, saved)
	}
	got := LoadStatSnapshot(path, snapshotMeta(), func(string, string) bool { return true })
	if got.Cache == nil || got.Ignored != "" || got.Loaded != 2 || got.Dropped != 0 {
		t.Fatalf("load: %+v", got)
	}
	if hit, ok := got.Cache.lookup("a.go", a, now); !ok || hit != "blob-a" {
		t.Fatalf("a.go 应命中: %v %q", ok, hit)
	}
	if hit, ok := got.Cache.lookup("dir/b.md", b, now); !ok || hit != "blob-b" {
		t.Fatalf("dir/b.md 应命中: %v %q", ok, hit)
	}
	if _, ok := got.Cache.lookup("racy.go", statKey{size: 5, mtimeNs: now.UnixNano()}, now.Add(time.Hour)); ok {
		t.Fatal("窗口内哈希的结果不得经快照恢复")
	}
}

// TestStatSnapshotRejectsMismatch 格式版本、工作区规范路径、子树名、扫描规则版本、切分版本
// 任一不同,或文件被截断,整份忽略并给出原因;文件不存在时既不加载也没有忽略原因。
func TestStatSnapshotRejectsMismatch(t *testing.T) {
	now := time.Now()
	cache := NewStatCache()
	cache.store("a.go", statKey{size: 10, mtimeNs: now.Add(-time.Hour).UnixNano()}, "blob-a", now)
	dir := t.TempDir()
	path := filepath.Join(dir, StatSnapshotFile)
	if _, err := cache.SaveSnapshot(path, snapshotMeta(), now); err != nil {
		t.Fatal(err)
	}
	keepAll := func(string, string) bool { return true }
	for name, meta := range map[string]StatSnapshotMeta{
		"workspace": {Workspace: "/srv/other", Subtree: snapshotMeta().Subtree, ChunkerVersion: "9"},
		"subtree":   {Workspace: "/srv/repo", Subtree: "default-v8+emb-x", ChunkerVersion: "9"},
		"chunker":   {Workspace: "/srv/repo", Subtree: snapshotMeta().Subtree, ChunkerVersion: "10"},
	} {
		got := LoadStatSnapshot(path, meta, keepAll)
		if got.Cache != nil || !strings.Contains(got.Ignored, name) {
			t.Fatalf("%s 不同应整份忽略并写明原因: %+v", name, got)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]any{"format": 99, "scan_rules_version": "old"} {
		changed := map[string]any{}
		for k, v := range file {
			changed[k] = v
		}
		changed[field] = value
		data, _ := json.Marshal(changed)
		other := filepath.Join(dir, field+".json")
		if err := os.WriteFile(other, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := LoadStatSnapshot(other, snapshotMeta(), keepAll); got.Cache != nil || got.Ignored == "" {
			t.Fatalf("%s 不同应整份忽略: %+v", field, got)
		}
	}
	truncated := filepath.Join(dir, "truncated.json")
	if err := os.WriteFile(truncated, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadStatSnapshot(truncated, snapshotMeta(), keepAll); got.Cache != nil || got.Ignored == "" {
		t.Fatalf("截断的快照应整份忽略: %+v", got)
	}
	if got := LoadStatSnapshot(filepath.Join(dir, "missing.json"), snapshotMeta(), keepAll); got.Cache != nil || got.Ignored != "" {
		t.Fatalf("快照不存在时不加载、也没有忽略原因: %+v", got)
	}
}

// TestStatSnapshotDropsEntriesFailingCheck 单条记录的内容哈希与 active manifest 不一致时
// 只丢这一条,其余照常加载。
func TestStatSnapshotDropsEntriesFailingCheck(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	cache := NewStatCache()
	cache.store("keep.go", statKey{size: 1, mtimeNs: past.UnixNano()}, "blob-keep", now)
	cache.store("stale.go", statKey{size: 2, mtimeNs: past.UnixNano()}, "blob-stale", now)
	path := filepath.Join(t.TempDir(), StatSnapshotFile)
	if _, err := cache.SaveSnapshot(path, snapshotMeta(), now); err != nil {
		t.Fatal(err)
	}
	got := LoadStatSnapshot(path, snapshotMeta(), func(rel, blob string) bool { return rel == "keep.go" && blob == "blob-keep" })
	if got.Cache == nil || got.Loaded != 1 || got.Dropped != 1 {
		t.Fatalf("应加载 1 条、丢弃 1 条: %+v", got)
	}
	if _, ok := got.Cache.lookup("stale.go", statKey{size: 2, mtimeNs: past.UnixNano()}, now); ok {
		t.Fatal("被丢弃的条目不得命中")
	}
}

// TestStatCacheGenerationTracksChanges 只有条目真的变化时代号才前进,保存方据此决定是否重写快照。
func TestStatCacheGenerationTracksChanges(t *testing.T) {
	now := time.Now()
	key := statKey{size: 1, mtimeNs: now.Add(-time.Hour).UnixNano()}
	cache := NewStatCache()
	g0 := cache.Generation()
	cache.store("a.go", key, "blob-a", now)
	g1 := cache.Generation()
	cache.store("a.go", key, "blob-a", now)
	cache.prune(map[string]bool{"a.go": true})
	if g1 == g0 || cache.Generation() != g1 {
		t.Fatalf("新增条目应前进、重复写入与无删除的清理不应前进: %d %d %d", g0, g1, cache.Generation())
	}
	cache.prune(map[string]bool{})
	if cache.Generation() == g1 {
		t.Fatal("清理掉条目后代号应前进")
	}
}
