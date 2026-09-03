package localengine

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestCompactionOnDeletionOnlyBuild 复现 2026-09-03 评测观察:.openaceignore
// 变更让约 57% 已索引文件出局,该次 sync 没有任何新增/变更 chunk(纯删除),
// 发布出的 revision 是"旧单段 + 33,009 tombstone"(垃圾占比 57.1%),
// compaction 不触发;查询按段取 top-N 后再过滤死 chunk 且不回填,有效召回
// 深度随之缩水,直到某次内容变更才碰巧合并。
//
// 期望(D3 垃圾占比触发条件按"构建结果"裁决):结果垃圾占比 >=
// compactGarbageRatio 的构建即便纯删除也走 full:compaction-garbage,合并
// 回单段、tombstone 清零;存活 chunk 的向量按内容键从旧段搬运,零
// provider 调用。
func TestCompactionOnDeletionOnlyBuild(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	// bulk/ 下文件的 chunk 占比过半,排除后垃圾占比越阈。
	const bulkFiles = 12
	for i := 0; i < bulkFiles; i++ {
		writeFixture(t, root, fmt.Sprintf("bulk/mod%02d.py", i),
			fmt.Sprintf("def bulk_handler_%02d(payload):\n    return payload * %d\n", i, i))
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	base, _ := loadActiveManifest(t, e, root)
	if len(base.Segments) != 1 || !base.SemanticComplete() {
		t.Fatalf("首建应为覆盖完整的单段: segments=%d vectors=%d/%d",
			len(base.Segments), base.VectorCount, base.Counts.Chunks)
	}
	bulkChunks := 0
	for path, entry := range base.Files {
		if strings.HasPrefix(path, "bulk/") {
			bulkChunks += entry.ChunkCount
		}
	}
	if ratio := float64(bulkChunks) / float64(base.Counts.Chunks); ratio < compactGarbageRatio {
		t.Fatalf("场景前提:排除 bulk/ 后垃圾占比应越阈,got %.2f", ratio)
	}

	// 用 .openaceignore 排除 bulk/(与评测现场同一触发方式)。规则文件自身
	// 一并排除:否则它会作为 1 个 text chunk 的新文件进入索引,本次构建就
	// 不再是纯删除。
	callsBefore := server.callCount()
	writeFixture(t, root, ".openaceignore", "bulk/\n.openaceignore\n")
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	compacted, _ := loadActiveManifest(t, e, root)
	if result.BuildMode != "full:compaction-garbage" {
		t.Fatalf("纯删除构建的结果垃圾占比越阈应触发 compaction: build_mode=%q segments=%d tombstones=%d garbage=%.2f",
			result.BuildMode, len(compacted.Segments), len(compacted.Tombstones), garbageRatio(compacted))
	}
	if len(compacted.Segments) != 1 || compacted.Segments[0].ID == base.Segments[0].ID {
		t.Fatalf("compaction 应合并为新的单段: %+v", compacted.Segments)
	}
	if len(compacted.Tombstones) != 0 || garbageRatio(compacted) != 0 {
		t.Fatalf("合并后 tombstone 与垃圾应清零: tombstones=%v garbage=%.2f",
			compacted.Tombstones, garbageRatio(compacted))
	}
	wantFiles := len(base.Files) - bulkFiles
	wantChunks := base.Counts.Chunks - bulkChunks
	if compacted.Counts.Files != wantFiles || compacted.Counts.Chunks != wantChunks {
		t.Fatalf("存活计数应只剩未排除文件: files=%d chunks=%d, want files=%d chunks=%d",
			compacted.Counts.Files, compacted.Counts.Chunks, wantFiles, wantChunks)
	}
	for path := range compacted.Files {
		if strings.HasPrefix(path, "bulk/") || path == ".openaceignore" {
			t.Fatalf("被排除文件不得留在 Files: %s", path)
		}
	}
	// 新段 chunk 与向量一一对应:存活 chunk 的向量全部从旧段按内容键复用
	// (旧段向量文件含被 tombstone 的行,复用只取存活行),覆盖完整。
	segment := compacted.Segments[0]
	if segment.Counts.Chunks != wantChunks || segment.VectorCount != wantChunks ||
		compacted.VectorCount != wantChunks || !compacted.SemanticComplete() {
		t.Fatalf("新段 chunk 与向量应一一对应且覆盖完整: segment chunks=%d vectors=%d, manifest vectors=%d/%d",
			segment.Counts.Chunks, segment.VectorCount, compacted.VectorCount, compacted.Counts.Chunks)
	}
	// 合并纯搬运,零 provider 调用(D3)。
	if server.callCount() != callsBefore {
		t.Fatalf("compaction 搬运不得付费: provider 调用 %d → %d", callsBefore, server.callCount())
	}
	// Result 计数口径:Added 为写入新段的 chunk 数,FileCount 为存活文件数。
	if result.Added != wantChunks || result.FileCount != wantFiles {
		t.Fatalf("Result 计数应与合并后 manifest 一致: added=%d files=%d, want %d/%d",
			result.Added, result.FileCount, wantChunks, wantFiles)
	}

	// 合并后检索:存活内容可查,被排除内容零引用(G3)。
	hit, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil || !strings.Contains(hit.Text, "HandleLogin") {
		t.Fatalf("合并后存活内容应可检索: %v %q", err, hit.Text)
	}
	miss, err := e.Search(context.Background(), searchRequest(root, "bulk_handler_03 payload"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(miss.Text, "bulk/") || strings.Contains(miss.Text, "bulk_handler") {
		t.Fatalf("被排除内容泄漏回结果: %q", miss.Text)
	}

	// 无变更再 sync:no-op,compaction 裁决不得引发重建风暴。
	again, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if again.IndexRevision != result.IndexRevision || again.BuildMode != "" {
		t.Fatalf("无变更 sync 应为 no-op: revision %s → %s build_mode=%q",
			result.IndexRevision, again.IndexRevision, again.BuildMode)
	}
}
