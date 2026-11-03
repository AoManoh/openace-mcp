package localengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestBuildDrainsParserPoolsBeforeEmbedding：全量与 delta 构建在切分结束、进入
// 嵌入与写分段之前清空 tree-sitter 的 arena 池。改动前只有 runBuild 返回时的
// defer 清空，嵌入与写分段期间池里的 arena 一直和全部向量同时占内存
// （mailyn-cli 一半副本 135 MiB，第 8 步堆剖析）。
func TestBuildDrainsParserPoolsBeforeEmbedding(t *testing.T) {
	var stages []engine.IndexStage
	drainParserPoolsHook = func(stage engine.IndexStage) { stages = append(stages, stage) }
	t.Cleanup(func() { drainParserPoolsHook = nil })

	e := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	full := len(stages)
	if err := os.WriteFile(filepath.Join(root, "extra.go"), []byte("package main\n\nfunc extra() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if full < 1 || len(stages) <= full {
		t.Fatalf("full and delta builds should each drain the parser pools after chunking: stages=%v (full=%d, build_mode=%q)", stages, full, result.BuildMode)
	}
	for _, stage := range stages {
		if stage != engine.IndexStageChunking {
			t.Fatalf("parser pools should be drained at the end of chunking, before embedding: stages=%v", stages)
		}
	}
}
