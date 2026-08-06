package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/localengine"
)

// R3(review 二批):-sync-only 宣称零 provider 发布,但 Sync 对 journal
// 未覆盖的 pending 键照常在线嵌入付费。发布前用零费计划面复核缺口,
// 缺口非零必须拒绝执行而非静默补付。
func TestEnsureSyncOnlyZeroProviderRejectsPendingGap(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "synconly-test")
	// 必死端点:预检本身零 provider 调用,任何真实请求都会立即失败。
	t.Setenv("OPENACE_EMBEDDING_PROVIDER", "openai")
	t.Setenv("OPENACE_EMBEDDING_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("OPENACE_EMBEDDING_API_KEY", "dead")
	t.Setenv("OPENACE_EMBEDDING_MODEL", "m")
	t.Setenv("OPENACE_EMBEDDING_DIMENSION", "8")
	t.Setenv("OPENACE_RERANK_PROVIDER", "off")

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := localengine.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close(context.Background())

	err = ensureSyncOnlyZeroProvider(context.Background(), eng, engine.WorkspaceRef{DirectoryPath: root})
	if err == nil {
		t.Fatal("pending 缺口下 -sync-only 必须拒绝执行")
	}
	if !strings.Contains(err.Error(), "-import-embeddings") {
		t.Fatalf("拒绝错误应给出补灌恢复路径: %v", err)
	}
}

// R3 对照面:纯词法形态(无 provider)无在线费用,预检直接放行。
func TestEnsureSyncOnlyZeroProviderAllowsLexicalOnly(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "synconly-test")
	t.Setenv("OPENACE_EMBEDDING_PROVIDER", "")
	t.Setenv("OPENACE_EMBEDDING_BASE_URL", "")
	t.Setenv("OPENACE_EMBEDDING_API_KEY", "")
	t.Setenv("OPENACE_RERANK_PROVIDER", "off")

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := localengine.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close(context.Background())

	if err := ensureSyncOnlyZeroProvider(context.Background(), eng, engine.WorkspaceRef{DirectoryPath: root}); err != nil {
		t.Fatalf("纯词法形态应放行: %v", err)
	}
}
