package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestStatCacheDetectsChanges 变更检测正确性:未变文件复用、尺寸变化
// /同尺寸不同 mtime 的内容变化都必须产生新 blobName。
func TestStatCacheDetectsChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("package a // v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// mtime 拨回过去,离开 racy 窗口,使短路路径可命中。
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)

	cache := NewStatCache()
	scan1, err := scanWithCache(context.Background(), root, cache)
	if err != nil || len(scan1) != 1 {
		t.Fatalf("scan1: %v %d", err, len(scan1))
	}
	scan2, err := scanWithCache(context.Background(), root, cache)
	if err != nil || len(scan2) != 1 {
		t.Fatalf("scan2: %v %d", err, len(scan2))
	}
	if scan2[0].BlobName != scan1[0].BlobName {
		t.Fatalf("未变文件应复用 blobName: %s vs %s", scan2[0].BlobName, scan1[0].BlobName)
	}

	// 同尺寸内容变化 + 新 mtime(仍在窗口外):必须检出。
	if err := os.WriteFile(path, []byte("package a // v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().Add(-30 * time.Minute)
	os.Chtimes(path, newer, newer)
	scan3, err := scanWithCache(context.Background(), root, cache)
	if err != nil {
		t.Fatal(err)
	}
	if scan3[0].BlobName == scan1[0].BlobName {
		t.Fatal("同尺寸内容变化未被检出")
	}

	// 尺寸变化:必须检出。
	if err := os.WriteFile(path, []byte("package a // v3 长内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, newer.Add(time.Minute), newer.Add(time.Minute))
	scan4, err := scanWithCache(context.Background(), root, cache)
	if err != nil {
		t.Fatal(err)
	}
	if scan4[0].BlobName == scan3[0].BlobName {
		t.Fatal("尺寸变化未被检出")
	}
}

// TestStatCacheRacyWindowAlwaysRehashes mtime 落在 racy 窗口内的文件
// 不走短路:同秒内"改内容但 stat 不变"的编辑不会漏检。哈希时仍在窗口内
// 的结果不存入,窗口过去之后也不会命中。
func TestStatCacheRacyWindowAlwaysRehashes(t *testing.T) {
	cache := NewStatCache()
	now := time.Now()
	at := func(mtime time.Time) statKey { return statKey{size: 10, mtimeNs: mtime.UnixNano()} }
	cache.store("a.go", at(now), "blob-1", now)
	if _, ok := cache.lookup("a.go", at(now), now); ok {
		t.Fatal("racy 窗口内不得命中")
	}
	if _, ok := cache.lookup("a.go", at(now), now.Add(time.Hour)); ok {
		t.Fatal("哈希时仍在窗口内的结果不得在窗口过去后命中")
	}
	past := now.Add(-time.Hour)
	cache.store("b.go", at(past), "blob-2", now)
	if hit, ok := cache.lookup("b.go", at(past), now); !ok || hit != "blob-2" {
		t.Fatalf("窗口外应命中: %v %q", ok, hit)
	}
	cache.store("b.go", at(now), "blob-3", now)
	if _, ok := cache.lookup("b.go", at(past), now.Add(time.Hour)); ok {
		t.Fatal("同一路径窗口内重新哈希后,旧记录应删除")
	}
	// 修改时间早已离开窗口、ctime 还在窗口内(刚做过保留修改时间的改写):同样不命中、不存入。
	recentCtime := statKey{size: 10, mtimeNs: past.UnixNano(), ctimeNs: now.UnixNano()}
	cache.store("c.go", recentCtime, "blob-4", now)
	if _, ok := cache.lookup("c.go", recentCtime, now.Add(time.Hour)); ok {
		t.Fatal("ctime 在窗口内时哈希的结果不得在窗口过去后命中")
	}
}

// TestStatCacheComparesCtimeInodeDevice 修改时间与大小相同、ctime、inode 或设备号
// 任一不同都不命中;全部相同才命中。
func TestStatCacheComparesCtimeInodeDevice(t *testing.T) {
	cache := NewStatCache()
	now := time.Now()
	past := now.Add(-time.Hour)
	base := statKey{size: 10, mtimeNs: past.UnixNano(), ctimeNs: past.UnixNano(), ino: 7, dev: 3}
	cache.store("a.go", base, "blob-1", now)
	if hit, ok := cache.lookup("a.go", base, now); !ok || hit != "blob-1" {
		t.Fatalf("各字段一致应命中: %v %q", ok, hit)
	}
	for name, changed := range map[string]statKey{
		"ctime": {size: 10, mtimeNs: past.UnixNano(), ctimeNs: past.Add(time.Minute).UnixNano(), ino: 7, dev: 3},
		"inode": {size: 10, mtimeNs: past.UnixNano(), ctimeNs: past.UnixNano(), ino: 8, dev: 3},
		"dev":   {size: 10, mtimeNs: past.UnixNano(), ctimeNs: past.UnixNano(), ino: 7, dev: 4},
	} {
		if _, ok := cache.lookup("a.go", changed, now); ok {
			t.Fatalf("%s 变化时不得命中", name)
		}
	}
}

// TestScanRehashesFileHashedInsideRacyWindow 文件在 racy 窗口内被哈希后,
// 又在同一时间刻内被等长改写(内容变了,大小与修改时间都没变):窗口过去
// 之后的扫描必须重新读内容。与 git racy-git 的规则同向:哈希时修改时间
// 还在窗口内的结果不能当作以后可信的缓存记录。
func TestScanRehashesFileHashedInsideRacyWindow(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	v1 := []byte("package a // v1\n")
	v2 := []byte("package a // v2\n")
	if err := os.WriteFile(path, v1, 0o644); err != nil {
		t.Fatal(err)
	}
	// 修改时间设在扫描前 1 秒,落在 2 秒窗口内。
	mtime := time.Now().Add(-time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	cache := NewStatCache()
	first, err := scanWithCache(context.Background(), root, cache)
	if err != nil || len(first) != 1 {
		t.Fatalf("first scan: %v %d", err, len(first))
	}
	if first[0].BlobName != blobName("a.go", v1) {
		t.Fatal("第一次扫描应读到 v1")
	}
	if err := os.WriteFile(path, v2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(mtime.Add(racyWindow + 200*time.Millisecond)))
	second, err := scanWithCache(context.Background(), root, cache)
	if err != nil || len(second) != 1 {
		t.Fatalf("second scan: %v %d", err, len(second))
	}
	if second[0].BlobName != blobName("a.go", v2) {
		t.Fatal("窗口内哈希的记录被当作有效缓存,等长改写没有检出")
	}
}

// sameLengthRewriteKeepsStat 把 path 改写为 content(长度不变)后把修改时间拨回 mtime,
// 模拟 tar -x、rsync -a、cp -p 这类保留修改时间的改写:大小与修改时间都和改写前相同,
// 只有 ctime(以及 rename 时的 inode)会变。
func sameLengthRewriteKeepsStat(t *testing.T, path string, content []byte, mtime time.Time, viaRename bool) {
	t.Helper()
	target := path
	if viaRename {
		target = path + ".tmp"
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if viaRename {
		if err := os.Rename(target, path); err != nil {
			t.Fatal(err)
		}
	}
}

// statRewriteScenario 先扫描一次让 v1 进缓存,再做保留修改时间的等长改写,等 ctime 离开
// racy 窗口后再扫描,第二次必须读到 v2。只在 Linux 与 macOS 上比较 ctime 与 inode。
func statRewriteScenario(t *testing.T, viaRename bool) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("ctime 与 inode 只在 Linux 与 macOS 上参与比较")
	}
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	v1 := []byte("package a // v1\n")
	v2 := []byte("package a // v2\n")
	mtime := time.Now().Add(-time.Hour)
	if err := os.WriteFile(path, v1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	cache := NewStatCache()
	first, err := scanWithCache(context.Background(), root, cache)
	if err != nil || len(first) != 1 || first[0].BlobName != blobName("a.go", v1) {
		t.Fatalf("第一次扫描应读到 v1: %v %+v", err, first)
	}
	sameLengthRewriteKeepsStat(t, path, v2, mtime, viaRename)
	time.Sleep(racyWindow + 300*time.Millisecond)
	second, err := scanWithCache(context.Background(), root, cache)
	if err != nil || len(second) != 1 {
		t.Fatalf("second scan: %v %d", err, len(second))
	}
	if second[0].BlobName != blobName("a.go", v2) {
		t.Fatal("保留修改时间的等长改写没有检出:stat 缓存按大小与修改时间命中了改写前的哈希")
	}
}

// TestStatCacheCtime 原地改写内容并把修改时间拨回原值:大小与修改时间不变,ctime 变了。
func TestStatCacheCtime(t *testing.T) {
	statRewriteScenario(t, false)
}

// TestStatCacheInode 新文件写好后 rename 覆盖原文件:大小与修改时间不变,inode 变了。
func TestStatCacheInode(t *testing.T) {
	statRewriteScenario(t, true)
}

// TestStatCachePrunesMissing 本轮未见路径被清理,防删除后同名复活误命中。
func TestStatCachePrunesMissing(t *testing.T) {
	cache := NewStatCache()
	past := statKey{size: 10, mtimeNs: time.Now().Add(-time.Hour).UnixNano()}
	cache.store("gone.go", past, "blob-old", time.Now())
	cache.prune(map[string]bool{})
	if _, ok := cache.lookup("gone.go", past, time.Now()); ok {
		t.Fatal("被清理路径不得命中")
	}
}

// TestScanWithNilCacheMatchesScan nil 缓存路径与历史 scan 逐字节一致。
func TestScanWithNilCacheMatchesScan(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := scan(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scanWithCache(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || a[0].BlobName != b[0].BlobName || a[0].RelPath != b[0].RelPath {
		t.Fatalf("nil 缓存应与 scan 一致: %+v vs %+v", a, b)
	}
}
