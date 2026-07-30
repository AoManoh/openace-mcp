package localengine

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestDeltaBuildEmbedsOnlyChangedFile 是 G1 的 fake 验收：单文件编辑的
// provider 送审文本恰为该文件变更内容；删除文件走 manifest-only 发布，
// 零 provider 调用（D1）。
func TestDeltaBuildEmbedsOnlyChangedFile(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)

	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	firstManifest, _ := loadActiveManifest(t, e, root)
	if len(firstManifest.Segments) != 1 {
		t.Fatalf("首建应为单段: %d", len(firstManifest.Segments))
	}
	callsAfterFull := server.callCount()

	// 编辑单文件 → delta：只嵌该文件变更内容。
	writeFixture(t, root, "util.py", fixtureUtilPy+"\ndef reload_config():\n    return parse_config('/etc/app.conf')\n")
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if second.IndexRevision == first.IndexRevision {
		t.Fatalf("变更应发布新 revision")
	}
	secondManifest, _ := loadActiveManifest(t, e, root)
	if len(secondManifest.Segments) != 2 {
		t.Fatalf("delta 应追加段: %d", len(secondManifest.Segments))
	}
	if secondManifest.Segments[0].ID != firstManifest.Segments[0].ID {
		t.Fatalf("基段应共享（D1）")
	}
	for _, text := range server.textsSince(callsAfterFull) {
		if strings.Contains(text, "HandleLogin") || strings.Contains(text, "Demo App") {
			t.Fatalf("未变更内容被送 provider（违反 G1）: %q", text[:min(40, len(text))])
		}
	}
	if !secondManifest.SemanticComplete() {
		t.Fatalf("delta 后覆盖口径应完整（K31/K51）: %d/%d", secondManifest.VectorCount, secondManifest.Counts.Chunks)
	}

	// 删除文件 → manifest-only delta：零 provider 调用、tombstone 生效。
	callsBeforeRemove := server.callCount()
	if err := os.Remove(rootJoin(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	third, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if third.IndexRevision == second.IndexRevision {
		t.Fatalf("删除应发布新 revision")
	}
	if server.callCount() != callsBeforeRemove {
		t.Fatalf("删除不得触发 provider 调用: %d → %d", callsBeforeRemove, server.callCount())
	}
	thirdManifest, _ := loadActiveManifest(t, e, root)
	if len(thirdManifest.Segments) != 2 {
		t.Fatalf("manifest-only 发布不应新增段: %d", len(thirdManifest.Segments))
	}
	if _, exists := thirdManifest.Files["README.md"]; exists {
		t.Fatalf("已删除文件不得留在 Files")
	}
	if !thirdManifest.TombstoneSet()["README.md"] {
		t.Fatalf("tombstones 应含已删除文件: %v", thirdManifest.Tombstones)
	}
	// 词法路死内容零泄漏（K39）。
	result, err := e.Search(context.Background(), searchRequest(root, "Demo App documentation"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "README.md") || strings.Contains(result.Text, "Demo App") {
		t.Fatalf("已删除内容泄漏回结果（违反 G3）: %q", result.Text)
	}
}

// TestMultiSegmentSearchNewestWins 是暗坑 K44：同文件跨 segment 多版本
// 并存时，查询只见最新内容且行号与当前文件一致。
func TestMultiSegmentSearchNewestWins(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// 三连改同一文件，形成 delta 链。
	versions := []string{
		"def parse_config(path):\n    return open(path).read()  # v2\n",
		"def parse_config(path):\n    data = open(path).read()  # v3\n    return data\n",
		"def parse_config(path):\n    \"\"\"Final v4 config parser.\"\"\"\n    with open(path) as handle:\n        return handle.read()\n",
	}
	for _, content := range versions {
		writeFixture(t, root, "util.py", content)
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if len(manifest.Segments) != 4 {
		t.Fatalf("应形成 4 段 delta 链: %d", len(manifest.Segments))
	}

	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Final v4 config parser") {
		t.Fatalf("应命中最新版本: %q", result.Text)
	}
	for _, stale := range []string{"# v2", "# v3"} {
		if strings.Contains(result.Text, stale) {
			t.Fatalf("旧版本内容泄漏（K44）: %q", result.Text)
		}
	}
	// util.py 只渲染一个块且起始行与当前文件一致（行窗口对尾随空行的
	// 计数沿用 Stage 2 golden 行为）。
	utilHeaders := 0
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.HasPrefix(line, "## util.py:") {
			utilHeaders++
			if !strings.HasPrefix(line, "## util.py:1-") {
				t.Fatalf("起始行应与最新文件一致: %q", line)
			}
		}
	}
	if utilHeaders != 1 {
		t.Fatalf("同文件应只渲染一个存活版本块: %d", utilHeaders)
	}
}

func rootJoin(root string, rel string) string {
	return root + string(os.PathSeparator) + rel
}
