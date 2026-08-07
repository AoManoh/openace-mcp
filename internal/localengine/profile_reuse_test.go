package localengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/vector"
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

// 最大候选部分损坏时必须继续尝试次新健康 sibling,避免整仓重付。
func TestProfileUpgradeSkipsCorruptSiblingForHealthyOlderProfile(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "same-model")
	root := newFixtureWorkspace(t)
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")

	buildVersion := func(version string) (*Engine, *index.Store) {
		e, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		e.profile.Version = version
		e.storeProfile = strings.Replace(e.storeProfile, "default-v7", "default-v"+version, 1)
		if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		_, key, err := e.resolveRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		store, err := e.storeFor(key)
		if err != nil {
			t.Fatal(err)
		}
		return e, store
	}

	e5, _ := buildVersion("5")
	e6, store6 := buildVersion("6") // 自动从 v5 复用
	// 替换既有文件形成 delta:base物理行可等于active存活VectorCount，
	// 破坏delta时旧"物理行数>=存活行数"会误判完整。先让v5/v6都
	// 收敛相同内容，再让v6最后激活成为优先候选。
	writeFixture(t, root, "main.go", fixtureMainGo+"\n// profile reuse delta\n")
	if _, err := e5.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := e6.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest6, _, err := store6.ResolveUsable()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest6.Segments) < 2 {
		t.Fatalf("预期 multi-segment delta: %+v", manifest6.Segments)
	}
	segment := manifest6.Segments[len(manifest6.Segments)-1]
	if err := os.WriteFile(filepath.Join(store6.SegmentPathFor(segment.ID), vector.DataFileName), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = e6.Close(context.Background())
	_ = e5.Close(context.Background())

	server.mu.Lock()
	before := server.calls
	server.mu.Unlock()
	e7, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e7.Close(context.Background())
	res, err := e7.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	after := server.calls
	server.mu.Unlock()
	if after != before || res.CrossProfileReused == 0 {
		t.Fatalf("应跳过损坏 v6 并复用健康 v5: calls %d→%d result=%+v", before, after, res)
	}
}

// 当前 profile manifest 逻辑100%但物理向量损坏时,查询登记 repair 后
// Sync 应回退兼容 sibling,而不是整仓重新付费。
func TestVectorRepairReusesHealthySiblingProfile(t *testing.T) {
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
	if _, err := e6.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_ = e6.Close(context.Background())

	e7, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e7.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, key, _ := e7.resolveRoot(root)
	store, _ := e7.storeFor(key)
	manifest, _, _ := store.ResolveUsable()
	segment := manifest.Segments[0]
	if err := os.WriteFile(filepath.Join(store.SegmentPathFor(segment.ID), vector.DataFileName), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := e7.Search(context.Background(), searchRequest(root, "HandleLogin")); err != nil || !strings.Contains(res.DegradedReason, "vector-data-unavailable") {
		t.Fatalf("损坏查询应降级并登记 repair: %+v err=%v", res, err)
	}
	server.mu.Lock()
	before := server.calls
	server.mu.Unlock()
	res, err := e7.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	after := server.calls
	server.mu.Unlock()
	if after != before || res.CrossProfileReused == 0 || res.SemanticCoverage != "100%" {
		t.Fatalf("repair 应复用 sibling 零重付: calls %d→%d result=%+v", before, after, res)
	}
	_ = e7.Close(context.Background())
}

// active/previous 共享 segment 必须只加载一次,否则大仓迁移峰值翻倍。
func TestLoadPriorVectorsDeduplicatesSharedSegments(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "same-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "new.go", "package app\n\nfunc NewThing() {}\n")
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, key, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(key)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil {
		t.Fatal(err)
	}
	prior := e.loadPriorVectors(store, manifest)
	if prior.activeLoadedRows != manifest.VectorCount {
		t.Fatalf("active 载入行数=%d want=%d", prior.activeLoadedRows, manifest.VectorCount)
	}
	if prior.loadedRows != prior.activeLoadedRows {
		t.Fatalf("previous 共享 segment 被重复载入: total=%d active=%d", prior.loadedRows, prior.activeLoadedRows)
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
