package localengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestJournalReadErrorPublishesLexicalWithoutSubmitting：打开本子树 journal 时读文件报错
// （这里用去掉读权限构造），同步不失败，词法与已有来源的向量照常发布，缺口一律不送审，
// last_error 写明 journal 读不出与恢复步骤；journal 恢复可读后，内容没变的下一次同步
// 补齐缺口。改动前同步直接失败、不发布，新改动的内容连词法都搜不到。
func TestJournalReadErrorPublishesLexicalWithoutSubmitting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 os.Chmod 不能让文件不可读，无法构造读错误，跳过")
	}
	const dim = 8
	ctx := context.Background()
	// 两个引擎实例共用同一个隔离的 cache 目录；不设置时会写进本机真实缓存。
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 4, "journal-read-error")
	root := refsWorkspace(t, 10)
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	_, workspaceKey, err := first.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := first.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf("package main\n\nfunc added%02d() string { return %q }\n", i, fmt.Sprint("added-", i))
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("n%02d.go", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	journalPath := filepath.Join(store.JournalDir(), "vectors.journal")
	if err := os.Chmod(journalPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(journalPath, 0o600) })
	if f, err := os.Open(journalPath); err == nil {
		_ = f.Close()
		t.Skip("当前用户不受文件权限位限制，无法构造读错误，跳过")
	}

	callsBefore := server.callCount()
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(ctx) })
	res, err := second.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("journal 读不出时同步不应失败: %v", err)
	}
	if docs := documentTexts(server, callsBefore); len(docs) != 0 {
		t.Fatalf("journal 读不出时缺口不应送审: %q", docs)
	}
	if !strings.Contains(res.DegradedReason, "semantic-coverage-partial") {
		t.Fatalf("同步结果应报部分覆盖: %+v", res)
	}
	manifest, _ := loadActiveManifest(t, second, root)
	if _, ok := manifest.Files["n00.go"]; !ok || manifest.SemanticComplete() {
		t.Fatalf("新文件应进入词法索引、语义覆盖不完整: files=%d vectors=%d/%d", len(manifest.Files), manifest.VectorCount, manifest.Counts.Chunks)
	}
	reason := lastSemanticError(t, second, root)
	for _, want := range []string{store.JournalDir(), "读不出", "本次不为缺口送审", "重新 sync"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("last_error 应包含 %q: %s", want, reason)
		}
	}

	if err := os.Chmod(journalPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("journal 恢复可读后的同步: %v", err)
	}
	if docs := documentTexts(server, callsBefore); len(docs) != 3 {
		t.Fatalf("恢复后应只为 3 个新文件送审，实际 %d 条", len(docs))
	}
	if manifest, _ := loadActiveManifest(t, second, root); !manifest.SemanticComplete() {
		t.Fatalf("恢复后应补齐: %d/%d", manifest.VectorCount, manifest.Counts.Chunks)
	}
}
