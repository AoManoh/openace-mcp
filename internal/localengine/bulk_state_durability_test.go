package localengine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
)

// TestSaveBulkStateReplacesFileAtomically 固定 Batch 状态文件的写入合同：
// 每次保存都完整替换目标文件、权限为 0600、目录里不留临时文件，读回的
// 内容与最后一次保存一致。落盘顺序（临时文件 fsync、改名、目录 fsync）
// 在单元测试里观察不到，由 strace 核对，记录在提交说明里。
func TestSaveBulkStateReplacesFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, bulkStateFileName)
	first := bulkState{
		ProfileHash:   "profile-a",
		PendingIntent: &bulkIntent{KeysSHA256: "digest-1", Count: 3, CreatedAt: time.Unix(1, 0).UTC()},
	}
	if err := saveBulkState(path, first); err != nil {
		t.Fatal(err)
	}
	second := bulkState{
		ProfileHash: "profile-a",
		Jobs:        []embedding.BulkJob{{ID: "batch-1", InputFileID: "file-1", Keys: []string{"k1", "k2", "k3"}, SubmittedAt: time.Unix(2, 0).UTC()}},
	}
	if err := saveBulkState(path, second); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadBulkState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PendingIntent != nil || len(loaded.Jobs) != 1 || loaded.Jobs[0].ID != "batch-1" || len(loaded.Jobs[0].Keys) != 3 {
		t.Fatalf("读回的状态应等于最后一次保存：%+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Batch 状态文件权限应为 0600，实际 %v", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != bulkStateFileName {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("目录里只应有 %s，实际 %v", bulkStateFileName, names)
	}
}
