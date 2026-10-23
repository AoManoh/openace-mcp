package localengine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// copyWorkspace 把 src 下的文件按相同相对路径复制到新的临时目录，模拟同一
// 仓库的另一个 worktree：相对路径与内容都相同，embedKey 也相同。
func copyWorkspace(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestSharedVectorsReuseAcrossWorktrees：同一 embedding 身份下，工作区 A 付过费
// 的向量记入共享向量库；内容与相对路径都相同的工作区 B（同一进程）与 C（另一
// 个引擎实例，模拟另一个进程）首建时不再为这些内容发文档嵌入请求，覆盖 100%。
func TestSharedVectorsReuseAcrossWorktrees(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	server := newEmbedServer(t, 8)
	opts := embedOptions(server.ts.URL, 8, 8, "shared")
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	ctx := context.Background()
	a := newFixtureWorkspace(t)
	if _, err := first.Sync(ctx, syncRequest(a)); err != nil {
		t.Fatal(err)
	}
	embeddedA := len(documentTexts(server, 0))
	if embeddedA == 0 {
		t.Fatal("A 首建应发出文档嵌入请求")
	}

	b := copyWorkspace(t, a)
	before := server.callCount()
	resB, err := first.Sync(ctx, syncRequest(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := documentTexts(server, before); len(got) != 0 || resB.SemanticCoverage != "100%" {
		t.Fatalf("B 与 A 内容相同，不应再送审（实际 %d 条），覆盖应为 100%%（%s）", len(got), resB.SemanticCoverage)
	}
	if resB.SharedReused != embeddedA {
		t.Fatalf("同步结果应写明从共享库复用了 %d 条，实际 %d", embeddedA, resB.SharedReused)
	}
	status, err := first.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: b})
	if err != nil || status.Semantic == nil || status.Semantic.SharedReused != embeddedA || status.Semantic.SharedVectorsBytes == 0 || status.Semantic.SharedVectorsError != "" {
		t.Fatalf("状态应给出复用数与共享库大小: %+v err=%v", status.Semantic, err)
	}

	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	c := copyWorkspace(t, a)
	before = server.callCount()
	resC, err := second.Sync(ctx, syncRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	if got := documentTexts(server, before); len(got) != 0 || resC.SemanticCoverage != "100%" {
		t.Fatalf("另一个引擎实例构建相同内容不应再送审（实际 %d 条），覆盖应为 100%%（%s）", len(got), resC.SemanticCoverage)
	}
}

// TestSharedVectorsDirectoryRemovedByUser：用户随时可以整目录删除共享库。删除
// 后同一个引擎实例（模拟长驻 daemon）构建相同内容照常送审、覆盖 100%，并把
// 新付费的向量写进重建的目录；之后另一个引擎实例构建同样的内容不再送审。
func TestSharedVectorsDirectoryRemovedByUser(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	server := newEmbedServer(t, 8)
	opts := embedOptions(server.ts.URL, 8, 8, "shared-removed")
	daemon, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close(context.Background()) })
	ctx := context.Background()
	a := newFixtureWorkspace(t)
	if _, err := daemon.Sync(ctx, syncRequest(a)); err != nil {
		t.Fatal(err)
	}
	embeddedA := len(documentTexts(server, 0))
	dir, err := daemon.sharedVectorsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	before := server.callCount()
	res, err := daemon.Sync(ctx, syncRequest(copyWorkspace(t, a)))
	if err != nil {
		t.Fatal(err)
	}
	if got := documentTexts(server, before); len(got) != embeddedA || res.SemanticCoverage != "100%" || res.SharedReused != 0 {
		t.Fatalf("共享库删除后应照常送审 %d 条并覆盖 100%%: 实际 %d 条 coverage=%s shared=%d", embeddedA, len(got), res.SemanticCoverage, res.SharedReused)
	}

	other, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(context.Background()) })
	before = server.callCount()
	res, err = other.Sync(ctx, syncRequest(copyWorkspace(t, a)))
	if err != nil {
		t.Fatal(err)
	}
	if got := documentTexts(server, before); len(got) != 0 || res.SharedReused != embeddedA {
		t.Fatalf("删除后重新付费的向量应写进重建的共享库: 送审 %d 条 shared=%d", len(got), res.SharedReused)
	}
}
