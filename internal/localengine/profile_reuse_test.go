package localengine

import (
	"context"
	"strings"
	"testing"
)

// 自动跨 profile 向量复用(2026-08-07 v7 迁移实证:524,885 unique 中
// 99.927% 可复用,正常用户却会整仓重付)。同 workspace + 同 embedding
// identity/template 的旧子树可作为只读 prior;chunk profile 变化只为
// 真实新 embedKey 调 provider。
func TestProfileUpgradeAutomaticallyReusesSiblingVectors(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "same-model")
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")

	e6, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e6.profile.Version = "6"
	e6.storeProfile = strings.Replace(e6.storeProfile, "default-v7", "default-v6", 1)
	first, err := e6.Sync(context.Background(), syncRequest(root))
	if err != nil || first.SemanticCoverage != "100%" {
		t.Fatalf("v6 首建失败: %+v err=%v", first, err)
	}
	server.mu.Lock()
	callsAfterV6 := server.calls
	server.mu.Unlock()
	if callsAfterV6 == 0 {
		t.Fatal("v6 首建应调用 provider")
	}
	if err := e6.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	e7, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e7.Close(context.Background())
	resolvedRoot, workspaceKey, err := e7.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e7.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	probe := priorVectors{}
	e7.mergeSiblingProfileVectors(store, resolvedRoot, &probe)
	if len(probe.crossProfileByHash) == 0 {
		t.Logf("current store=%s workspace=%+v profileHash=%s", store.Root(), resolvedRoot, e7.embedCfg.ProfileHash())
		t.Fatal("应发现兼容 v6 sibling vectors")
	}
	second, err := e7.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatalf("v7 升级失败: %v", err)
	}
	server.mu.Lock()
	callsAfterV7 := server.calls
	server.mu.Unlock()
	if callsAfterV7 != callsAfterV6 {
		t.Fatalf("同 identity profile 升级应零 provider: v6 calls=%d v7 total=%d", callsAfterV6, callsAfterV7)
	}
	if second.SemanticCoverage != "100%" {
		t.Fatalf("复用后应完整覆盖: %+v", second)
	}
	if second.CrossProfileReused <= 0 {
		t.Fatalf("结果应外显跨 profile 复用数: %+v", second)
	}
}

// embedding identity 不同(模型/维度/模板任一变化)绝不跨子树复用。
func TestProfileUpgradeRejectsSiblingWithDifferentEmbeddingIdentity(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")

	oldOpts := embedOptions(server.ts.URL, dim, 8, "old-model")
	old, err := New(oldOpts)
	if err != nil {
		t.Fatal(err)
	}
	old.profile.Version = "6"
	old.storeProfile = strings.Replace(old.storeProfile, "default-v7", "default-v6", 1)
	if _, err := old.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	before := server.calls
	server.mu.Unlock()
	_ = old.Close(context.Background())

	newOpts := embedOptions(server.ts.URL, dim, 8, "new-model")
	current, err := New(newOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close(context.Background())
	res, err := current.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	after := server.calls
	server.mu.Unlock()
	if after <= before {
		t.Fatalf("不同模型必须调用 provider: before=%d after=%d", before, after)
	}
	if res.CrossProfileReused != 0 {
		t.Fatalf("不同 identity 不得复用: %+v", res)
	}
}
