package localengine

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// H3 回归(诊断报告 2026-08-03 §4-H3,P0):文件内容清空为纯空白(size>0)
// 后,变更文件切分得 0 chunk 且不入 tombstone、仍留 Files{ChunkCount:0},
// 旧段 chunk 无人覆盖继续可检索;compaction 复用条件命中后污染被原样
// 搬进新段,跨 compaction 存续。契约(K39/K44 家族):编辑后旧内容
// 即刻不可检索——0 chunk 的变更文件按删除语义处理。
func TestWhitespaceWipeRemovesOldContent(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	base, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(base.Text, "util.py") {
		t.Fatalf("前置:目标应可检索: %q", base.Text)
	}

	// 清空为纯空白(size>0,过扫描门禁;切分 0 chunk)。
	writeFixture(t, root, "util.py", "\n\n    \n\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	res, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "util.py") {
		t.Fatalf("空白化后旧内容不得可检索(H3): %q", res.Text)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if entry, ok := manifest.Files["util.py"]; ok {
		t.Fatalf("0 chunk 文件不得留 Files 条目: %+v", entry)
	}

	// 连续编辑他文件堆 delta 段直到触发一次 compaction(段数折叠回 1
	// 即证发生)——污染不得被复用搬进合并段。
	compacted := false
	for i := 0; i < compactSegmentThreshold*2 && !compacted; i++ {
		writeFixture(t, root, "churn.go", fmt.Sprintf("package app\n\n// Churn%d 迭代占位。\nfunc Churn%d() {}\n", i, i))
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		manifest, _ = loadActiveManifest(t, e, root)
		if i > 0 && len(manifest.Segments) == 1 {
			compacted = true
		}
	}
	if !compacted {
		t.Fatalf("前置:应已发生 compaction(段数=%d)", len(manifest.Segments))
	}
	res, err = e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "util.py") {
		t.Fatalf("compaction 后污染不得存续(H3): %q", res.Text)
	}

	// 文件恢复实质内容 → 重新可见。
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return read_settings(path)\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	res, err = e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "util.py") {
		t.Fatalf("恢复内容后应重新可检索: %q", res.Text)
	}
}

// TestFullBuildOmitsZeroChunkFiles:全量路径不得留 ChunkCount:0 脏条目
// (首建即含纯空白文件的场景)。
func TestFullBuildOmitsZeroChunkFiles(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	writeFixture(t, root, "blank.py", "\n   \n\t\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if entry, ok := manifest.Files["blank.py"]; ok {
		t.Fatalf("全量构建不得为 0 chunk 文件留条目: %+v", entry)
	}
}
