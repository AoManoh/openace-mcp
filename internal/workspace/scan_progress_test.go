package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// P-gray-01(2026-08-11 外部灰度):大仓首扫期间状态面 files 长期为 0,用户
// 误判卡死而取消。修复=扫描期增量进度回报:Progress 回调在 walk 过程中
// 周期性收到"已收录文件数",完成前至少回报一次中间值。

func TestScanReportsIncrementalProgress(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2*scanProgressEvery+10; i++ {
		dir := filepath.Join(root, "pkg", string(rune('a'+i%26)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)+".go"), []byte("package p\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []int
	source := FileAssetSource{Progress: func(scanned int) { calls = append(calls, scanned) }}
	assets, _, err := source.LoadWithStats(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) < 2*scanProgressEvery {
		t.Fatalf("fixture too small: %d", len(assets))
	}
	if len(calls) < 2 {
		t.Fatalf("progress must fire at least twice during walk, got %v", calls)
	}
	for i := 1; i < len(calls); i++ {
		if calls[i] <= calls[i-1] {
			t.Fatalf("progress must be monotonic: %v", calls)
		}
	}
	if last := calls[len(calls)-1]; last > len(assets) {
		t.Fatalf("progress %d exceeds final asset count %d", last, len(assets))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
