package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 方案①(2026-08-02 批准)A2' 嵌入模板与键升级(R1/R2)。

// TestEmbedDocTextTemplate:A2' 冻结文案(有/无 symbol、无 language 分支)。
func TestEmbedDocTextTemplate(t *testing.T) {
	withSym := chunkRecord{RelPath: "pkg/auth/login.go", Language: "go", Symbol: "HandleLogin",
		StartLine: 10, EndLine: 20, Content: "func HandleLogin() {}"}
	got := embedDocText(withSym)
	want := "This chunk is from pkg/auth/login.go, go, defining HandleLogin.\nfunc HandleLogin() {}"
	if got != want {
		t.Fatalf("A2' 有 symbol 分支:\n got: %q\nwant: %q", got, want)
	}
	noSym := chunkRecord{RelPath: "README.md", Language: "markdown", Content: "# Title"}
	got = embedDocText(noSym)
	want = "This chunk is from README.md, markdown.\n# Title"
	if got != want {
		t.Fatalf("A2' 无 symbol 分支:\n got: %q\nwant: %q", got, want)
	}
	// R2:行号不进模板(增量经济性),模板文本与行号无关。
	shifted := withSym
	shifted.StartLine, shifted.EndLine = 110, 120
	if embedDocText(shifted) != embedDocText(withSym) {
		t.Fatal("R2:行号不得影响嵌入文本")
	}
}

// TestEmbedKeyContract:R1 键升级——同内容异路径键不同(防串路径);
// 行号漂移键不变(R2);全同键相同。
func TestEmbedKeyContract(t *testing.T) {
	a := chunkRecord{RelPath: "a/x.py", Language: "python", Symbol: "f", ContentHash: "h1"}
	b := chunkRecord{RelPath: "b/x.py", Language: "python", Symbol: "f", ContentHash: "h1"}
	if embedKey(a) == embedKey(b) {
		t.Fatal("R1:同内容异路径必须不同键(防带头向量串路径)")
	}
	shifted := a
	shifted.StartLine = 999
	if embedKey(shifted) != embedKey(a) {
		t.Fatal("R2:行号漂移不得改变键")
	}
	dup := a
	if embedKey(dup) != embedKey(a) {
		t.Fatal("全同记录键必须稳定")
	}
}

// TestEmbedInputCarriesTemplateAndDupContentSplits:端到端——provider 收到
// 的文本带 A2' 头;同内容双文件各自成键、各得路径正确的向量。
func TestEmbedInputCarriesTemplateAndDupContentSplits(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := t.TempDir()
	same := "def parse():\n    return 1\n"
	writeFixture(t, root, "one/util.py", same)
	writeFixture(t, root, "two/util.py", same)
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}}); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	var sent []string
	for _, call := range server.perCall {
		sent = append(sent, call...)
	}
	server.mu.Unlock()
	var one, two bool
	for _, text := range sent {
		if !strings.HasPrefix(text, "This chunk is from ") {
			t.Fatalf("嵌入输入应带 A2' 头: %q", text)
		}
		if strings.Contains(text, "one/util.py") {
			one = true
		}
		if strings.Contains(text, "two/util.py") {
			two = true
		}
	}
	if !one || !two {
		t.Fatalf("同内容双文件应各自嵌入(R1 键含路径): one=%v two=%v sent=%d", one, two, len(sent))
	}
}

// TestTemplateVersionInSubtree:模板版本进子树名(T3)——版本变化 = 平行
// 子树全量重建的迁移触发器,旧子树保留供回退。
func TestTemplateVersionInSubtree(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	if !strings.Contains(e.storeProfile, "-"+embedTemplateVersion) {
		t.Fatalf("storeProfile 应含模板版本: %q", e.storeProfile)
	}
}
