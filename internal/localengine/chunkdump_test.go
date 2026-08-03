package localengine

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestDumpChunkRecords 验证评测 hook 导出存活 chunk 全量记录:
// 确定性排序、字段完整、与 ChunkDocTexts 同一存活集(敏感文件不出现)。
func TestDumpChunkRecords(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatal(err)
	}
	var got []ChunkDumpRecord
	if err := e.DumpChunkRecords(context.Background(), ref, func(rec ChunkDumpRecord) error {
		got = append(got, rec)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("应导出至少一个 chunk")
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].ID < got[j].ID }) {
		t.Fatal("导出必须按 chunk ID 确定性排序")
	}
	foundLogin := false
	for _, rec := range got {
		if rec.ID == "" || rec.RelPath == "" || rec.Content == "" || rec.StartLine <= 0 || rec.EndLine < rec.StartLine {
			t.Fatalf("记录字段不完整: %+v", rec)
		}
		if strings.Contains(rec.RelPath, ".env") || strings.Contains(rec.Content, "super-secret-value-canary") {
			t.Fatalf("敏感文件泄漏进导出: %+v", rec)
		}
		if rec.Symbol == "HandleLogin" && strings.Contains(rec.Content, "establishSession(user)") {
			foundLogin = true
		}
	}
	if !foundLogin {
		t.Fatal("未找到 HandleLogin 声明 chunk(symbol+content 应齐备)")
	}
}
