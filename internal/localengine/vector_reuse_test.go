package localengine

import (
	"context"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// 跨子树向量复用(2026-08-05,v5 迁移 117.8M 教训:profile 升版使全部
// 子树键失效,但 embedKey = f(模板,路径,符号,语言,内容) 与 chunk profile
// 版本无关——逐字节相同的 chunk 在新子树里本可零费复用,v4→v5 却整库重付
// ≈40M)。机制:DumpVectorEntries 从既有子树导出 {embedKey, vector},
// ImportEmbeddings 灌入新子树 journal,sync 零 provider 收编。
// 本测试用"同内容异目录工作区"(wsKey 不同、embedKey 相同)模拟子树切换。
func TestCrossStoreVectorReuse(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 2, "fake-model"))

	writeCorpus := func(root string) {
		writeFixture(t, root, "pkg/a.go", "package pkg\n\nfunc Alpha() {}\n")
		writeFixture(t, root, "pkg/b.go", "package pkg\n\nfunc Beta() {}\n")
	}
	rootA := t.TempDir()
	writeCorpus(rootA)
	refA := engine.WorkspaceRef{DirectoryPath: rootA}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: refA}); err != nil {
		t.Fatal(err)
	}
	paidCalls := server.callCount()
	if paidCalls == 0 {
		t.Fatal("首建应有付费嵌入")
	}

	// 从 A 的已发布子树导出 (embedKey, vector)。
	manifest, storeA := loadActiveManifest(t, e, rootA)
	dumped := map[string][]float32{}
	for _, segment := range manifest.Segments {
		if segment.VectorsChecksum == "" {
			continue
		}
		ix, err := vector.Load(storeA.SegmentPathFor(segment.ID), 8,
			segment.VectorsChecksum, segment.VectorsIndexChecksum, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i, entry := range ix.Entries() {
			if _, ok := dumped[entry.ContentHash]; !ok {
				dumped[entry.ContentHash] = append([]float32(nil), ix.Row(i)...)
			}
		}
	}
	if len(dumped) == 0 {
		t.Fatal("A 子树应有向量可导出")
	}

	// 同内容异目录 → 新 wsKey 子树;灌入导出向量后 sync 必须零新增付费。
	rootB := t.TempDir()
	writeCorpus(rootB)
	refB := engine.WorkspaceRef{DirectoryPath: rootB}
	keys := make([]string, 0, len(dumped))
	for k := range dumped {
		keys = append(keys, k)
	}
	i := 0
	report, err := e.ImportEmbeddings(context.Background(), refB, func() (string, []float32, bool, error) {
		if i >= len(keys) {
			return "", nil, false, nil
		}
		k := keys[i]
		i++
		return k, append([]float32(nil), dumped[k]...), true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Appended != len(dumped) {
		t.Fatalf("导出向量应全部可灌入: %+v", report)
	}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: refB}); err != nil {
		t.Fatal(err)
	}
	if server.callCount() != paidCalls {
		t.Fatalf("复用后 B 首建不得新增付费嵌入: calls %d → %d", paidCalls, server.callCount())
	}
	manifestB, _ := loadActiveManifest(t, e, rootB)
	if !manifestB.SemanticComplete() {
		t.Fatalf("B 应语义完整发布: %+v", manifestB.Counts)
	}
	var _ *index.Manifest = manifestB
}
