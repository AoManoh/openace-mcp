package localengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// assertNoDeadReferences 对词法与语义双路断言零死引用（G3/K39）。
func assertNoDeadReferences(t *testing.T, e *Engine, root string, query string, forbidden []string) {
	t.Helper()
	result, err := e.Search(context.Background(), searchRequest(root, query))
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	for _, marker := range forbidden {
		if strings.Contains(result.Text, marker) {
			t.Fatalf("死引用泄漏（G3）: query=%q marker=%q\n%s", query, marker, result.Text)
		}
	}
}

// TestLifecycleDeleteSingleFile 删除单文件后双路零引用（watcher 触发路径
// SyncBackground 与手动 sync 同语义）。
func TestLifecycleDeleteSingleFile(t *testing.T) {
	const dim = 8
	server := newMagicEmbedServer(t, dim, func(text string) bool {
		return strings.Contains(text, "parse_config") || strings.Contains(text, "qqxx")
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// 语义路定向命中确认（删除前基线）。
	baseline, err := e.Search(context.Background(), searchRequest(root, lexicalMissQuery))
	if err != nil || !strings.Contains(baseline.Text, "util.py") {
		t.Fatalf("基线语义命中应存在: %v %q", err, baseline.Text)
	}

	if err := os.Remove(filepath.Join(root, "util.py")); err != nil {
		t.Fatal(err)
	}
	// watcher 触发路径。
	if _, err := e.SyncBackground(context.Background(), engine.SyncRequest{Workspace: engineRef(root)}); err != nil {
		t.Fatal(err)
	}
	// 词法路（直接词命中）与语义路（定向向量必然召回旧 chunk）双断言。
	assertNoDeadReferences(t, e, root, "parse_config configuration", []string{"util.py", "parse_config"})
	assertNoDeadReferences(t, e, root, lexicalMissQuery, []string{"util.py", "parse_config"})
}

// TestLifecycleDeleteDirectory 删除目录后其下全部文件零引用。
func TestLifecycleDeleteDirectory(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	writeFixture(t, root, "pkg/alpha.go", "package pkg\n\n// AlphaHandler 处理 alpha 请求。\nfunc AlphaHandler() {}\n")
	writeFixture(t, root, "pkg/beta.go", "package pkg\n\n// BetaHandler 处理 beta 请求。\nfunc BetaHandler() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "pkg")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	assertNoDeadReferences(t, e, root, "AlphaHandler BetaHandler", []string{"pkg/alpha.go", "pkg/beta.go", "AlphaHandler", "BetaHandler"})
}

// TestLifecycleRenameSameContent 重命名（同内容）：旧路径零引用、新路径
// 可检索，且向量按纯 content hash 复用零重付（G3 + D2）。
func TestLifecycleRenameSameContent(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	callsBefore := server.callCount()
	if err := os.Rename(filepath.Join(root, "util.py"), filepath.Join(root, "config_util.py")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if server.callCount() != callsBefore {
		t.Fatalf("同内容 rename 不得重付 embedding（D2）: %d → %d", callsBefore, server.callCount())
	}
	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "config_util.py") {
		t.Fatalf("新路径应可检索: %q", result.Text)
	}
	if strings.Contains(result.Text, "## util.py") {
		t.Fatalf("旧路径零引用（G3）: %q", result.Text)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	if !manifest.SemanticComplete() {
		t.Fatalf("rename 后覆盖应完整: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
}

// TestLifecycleRenameWithEdit 重命名 + 修改：旧路径与旧内容双零引用。
func TestLifecycleRenameWithEdit(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "util.py")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "loader.py", "def load_settings(path):\n    \"\"\"Renamed and rewritten loader.\"\"\"\n    return path\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	assertNoDeadReferences(t, e, root, "parse_config load_settings configuration", []string{"util.py", "parse_config"})
	result, err := e.Search(context.Background(), searchRequest(root, "load_settings"))
	if err != nil || !strings.Contains(result.Text, "loader.py") {
		t.Fatalf("新文件应可检索: %v %q", err, result.Text)
	}
}

// TestLifecycleBranchSwitch 模拟分支切换：大规模内容替换后旧分支内容
// 零引用、新分支内容完整可检索（G3）。
func TestLifecycleBranchSwitch(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	// "feature 分支"额外内容。
	writeFixture(t, root, "feature/gateway.go", "package feature\n\n// GatewayRoute 网关路由注册。\nfunc GatewayRoute() {}\n")
	writeFixture(t, root, "feature/session.go", "package feature\n\n// SessionStore 会话存储。\nfunc SessionStore() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	// 切回 "main 分支"：feature 目录消失、util.py 回到旧实现、新增 docs。
	if err := os.RemoveAll(filepath.Join(root, "feature")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "util.py", "def parse_config(path):\n    return None  # main branch variant\n")
	writeFixture(t, root, "docs.md", "# Main branch documentation\nmainline content\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	assertNoDeadReferences(t, e, root, "GatewayRoute SessionStore gateway", []string{"feature/", "GatewayRoute", "SessionStore"})
	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil || !strings.Contains(result.Text, "main branch variant") {
		t.Fatalf("切换后应命中当前分支内容: %v %q", err, result.Text)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	for path := range manifest.Files {
		if strings.HasPrefix(path, "feature/") {
			t.Fatalf("旧分支文件不得留在 live 集: %s", path)
		}
	}
}
