package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatCacheHitRechecksSizeLimits：daemon 重启时调小 OPENACE_MAX_FILE_BYTES 与
// OPENACE_MAX_TEXT_FILE_BYTES，从 stat 快照装入的条目仍会命中。命中时文件超过当前
// 通用上限就要重新按两个上限判定，与不带缓存的扫描结果一致；只看缓存时，已收录的
// 超限文件会一直留在索引里，直到文件本身改动。
func TestStatCacheHitRechecksSizeLimits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "big.go")
	content := []byte("package big\n\n// " + strings.Repeat("x", 640) + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if files, _, err := scanWithCacheStats(ctx, root, NewStatCache()); err != nil || len(files) != 1 {
		t.Fatalf("前置：默认上限下应收录 big.go: %v %d", err, len(files))
	}
	// 刚写入的文件 ctime 还在 racy 窗口内，扫描不会存入缓存；daemon 在之后的扫描里
	// 才存入。这里直接写入与扫描相同的条目，再经快照保存与加载模拟重启。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewStatCache()
	cache.entries["big.go"] = statEntry{key: statKeyOf(info), blobName: blobName("big.go", content)}
	snapshot := filepath.Join(t.TempDir(), StatSnapshotFile)
	if _, err := cache.SaveSnapshot(snapshot, snapshotMeta(), time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded := LoadStatSnapshot(snapshot, snapshotMeta(), nil)
	if loaded.Cache == nil || loaded.Loaded != 1 {
		t.Fatalf("前置：快照应装入 big.go 的条目: %+v", loaded)
	}

	// 等文件的 ctime 离开 racy 窗口，下面的扫描才会走缓存命中分支。
	if wait := racyWindow + 100*time.Millisecond - time.Since(statKeyOf(info).changedAt()); wait > 0 {
		time.Sleep(wait)
	}
	t.Setenv("OPENACE_MAX_FILE_BYTES", "128")
	t.Setenv("OPENACE_MAX_TEXT_FILE_BYTES", "128")
	warm, warmStats, err := scanWithCacheStats(ctx, root, loaded.Cache)
	if err != nil {
		t.Fatal(err)
	}
	cold, coldStats, err := scanWithCacheStats(ctx, root, NewStatCache())
	if err != nil {
		t.Fatal(err)
	}
	if len(warm) != 0 || warmStats.OversizeSkippedFiles != 1 || len(cold) != 0 || coldStats.OversizeSkippedFiles != 1 {
		t.Fatalf("上限调小后命中快照条目的超限文件应与冷缓存一样跳过: warm=%d skipped=%d cold=%d skipped=%d",
			len(warm), warmStats.OversizeSkippedFiles, len(cold), coldStats.OversizeSkippedFiles)
	}
}
