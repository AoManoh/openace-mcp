package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// 2026-08-10 scanner 大文件修复(用户批准):超过通用上限(默认 1MiB)但不超过
// 文本上限(默认 4MiB)的"纯文本"文件必须被索引,而非整篇静默跳过——R02 证据:
// OTel CHANGELOG.md(1.109MB)单文件占 decision top10 miss 的 15.7%。
// 二进制文件与超过文本上限的文件维持跳过,且超限跳过按 K6 口径如实计数。

func writeTextFile(t *testing.T, path string, size int) {
	t.Helper()
	line := []byte("2026-08-10 changelog entry: fixed scanner oversize handling for plain text files\n")
	content := bytes.Repeat(line, size/len(line)+1)[:size]
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func scanRels(t *testing.T, root string) (map[string]bool, ScanStats) {
	t.Helper()
	files, stats, err := scanWithCacheStats(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	rels := make(map[string]bool, len(files))
	for _, file := range files {
		rels[file.RelPath] = true
	}
	return rels, stats
}

func TestScanIndexesOversizeTextUpToTextCap(t *testing.T) {
	root := t.TempDir()
	writeTextFile(t, filepath.Join(root, "CHANGELOG.md"), (1<<20)+512*1024) // 1.5MiB 文本
	writeTextFile(t, filepath.Join(root, "small.go"), 128)
	rels, stats := scanRels(t, root)
	if !rels["small.go"] || !rels["CHANGELOG.md"] {
		t.Fatalf("oversize text file must be indexed under text cap, got %v", rels)
	}
	if stats.OversizeSkippedFiles != 0 {
		t.Fatalf("no oversize skip expected, got %d", stats.OversizeSkippedFiles)
	}
}

func TestScanSkipsOversizeBinaryInTextBand(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte{0x00, 'a', 'b', 'c'}, ((1<<20)+1024)/4) // >1MiB 且含 NUL
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	rels, stats := scanRels(t, root)
	if rels["blob.bin"] {
		t.Fatal("binary file in text band must stay skipped")
	}
	if stats.OversizeSkippedFiles != 0 {
		t.Fatalf("binary skip is not an oversize skip, got %d", stats.OversizeSkippedFiles)
	}
}

func TestScanSkipsBeyondTextCapAndCounts(t *testing.T) {
	root := t.TempDir()
	writeTextFile(t, filepath.Join(root, "huge.md"), (4<<20)+4096) // >4MiB 文本
	writeTextFile(t, filepath.Join(root, "mid.md"), (1<<20)+4096)  // 文本带内,应收录
	rels, stats := scanRels(t, root)
	if rels["huge.md"] {
		t.Fatal("text file beyond text cap must be skipped")
	}
	if !rels["mid.md"] {
		t.Fatal("text file within text cap must be indexed")
	}
	if stats.OversizeSkippedFiles != 1 {
		t.Fatalf("oversize skip must be counted once, got %d", stats.OversizeSkippedFiles)
	}
}

func TestScanTextCapDisabledByEnvRestoresLegacyBehavior(t *testing.T) {
	t.Setenv("OPENACE_MAX_TEXT_FILE_BYTES", strconv.Itoa(1<<20)) // 文本上限收敛到通用上限=关闭扩展
	root := t.TempDir()
	writeTextFile(t, filepath.Join(root, "CHANGELOG.md"), (1<<20)+512*1024)
	rels, stats := scanRels(t, root)
	if rels["CHANGELOG.md"] {
		t.Fatal("extension disabled: oversize text must be skipped as before")
	}
	if stats.OversizeSkippedFiles != 1 {
		t.Fatalf("oversize skip must be counted, got %d", stats.OversizeSkippedFiles)
	}
}

func TestScanTextCapNeverBelowGeneralCap(t *testing.T) {
	t.Setenv("OPENACE_MAX_TEXT_FILE_BYTES", "1") // 非法配置:低于通用上限,按通用上限兜底
	root := t.TempDir()
	writeTextFile(t, filepath.Join(root, "normal.md"), 64*1024) // 64KiB,任何配置下都必须收录
	rels, _ := scanRels(t, root)
	if !rels["normal.md"] {
		t.Fatal("files within the general cap must always be indexed")
	}
}
