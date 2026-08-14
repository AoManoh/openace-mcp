package localengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件回归 T1(docs/tasks/T1-first-touch-fast-path.md,用户 2026-08-14
// 指令立项):daemon 重启后首触查询必须立即以磁盘上的可服务 revision
// 应答(reason=index-refreshing 显式披露),后台异步收敛;真实链路复现
// 证据=gradle 29K files 首触阻塞 43.6s 后才 stale-serve(本可 t=0 服务)。
// 快路径边界:显式 Sync 不走(F2)/真冷仓不走/deny 不走/strict 不走。

// restartedEngine 在同一 cache 上构造第二个引擎实例,模拟 daemon 重启
// (进程内 lastSyncOK 清空,磁盘 revision 保留)。
func restartedEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	e2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e2.Close(context.Background()) })
	return e2
}

func waitFirstTouchConverged(t *testing.T, e *Engine, root string) {
	t.Helper()
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		_, ok := e.lastSyncOK[workspaceKey]
		e.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("后台首触同步未在 30s 内收敛")
}

// TestFirstTouchServesPersistedRevisionImmediately:重启后首触毫秒级
// 返回旧 revision + index-refreshing 披露;后台收敛后第二查干净。
func TestFirstTouchServesPersistedRevisionImmediately(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "t1-model")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t1")
	root := newFixtureWorkspace(t)

	e1, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	first, err := e1.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	e2 := restartedEngine(t, opts)
	begin := time.Now()
	result, err := e2.Search(context.Background(), searchRequest(root, "parse_config"))
	elapsed := time.Since(begin)
	if err != nil {
		t.Fatalf("首触查询失败: %v", err)
	}
	if !strings.Contains(result.DegradedReason, "index-refreshing") {
		t.Fatalf("首触必须显式披露 refreshing,实际 degraded=%q", result.DegradedReason)
	}
	if result.IndexRevision != first.IndexRevision {
		t.Fatalf("首触应服务磁盘上的既有 revision: got %s want %s", result.IndexRevision, first.IndexRevision)
	}
	if len(result.Hits) == 0 || result.Hits[0].Path != first.Hits[0].Path {
		t.Fatalf("首触结果应与重启前一致: %+v", result.Hits)
	}
	// 快路径的意义就是不等扫描:留出巨大余量断言(真实修复前为 40s+)。
	if elapsed > 5*time.Second {
		t.Fatalf("首触应立即返回,实际耗时 %s", elapsed)
	}
	waitFirstTouchConverged(t, e2, root)
	clean, err := e2.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(clean.DegradedReason, "index-refreshing") {
		t.Fatalf("后台收敛后不应再披露 refreshing: %q", clean.DegradedReason)
	}
}

// TestFirstTouchConvergesToChangedContent:停机期间文件变更,首触先服务
// 旧内容(显式披露),后台收敛后新内容可检索——最终新鲜度证明。
func TestFirstTouchConvergesToChangedContent(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "t1-model-b")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t1b")
	root := newFixtureWorkspace(t)

	e1, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if err := e1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 停机期变更:新增一个可检索的函数文件。
	if err := os.WriteFile(filepath.Join(root, "offline_added.go"), []byte("package main\n\n// offlineAddedSentinel 是停机期新增的标记函数。\nfunc offlineAddedSentinel() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	e2 := restartedEngine(t, opts)
	stale, err := e2.Search(context.Background(), searchRequest(root, "offlineAddedSentinel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stale.DegradedReason, "index-refreshing") {
		t.Fatalf("首触应披露 refreshing: %q", stale.DegradedReason)
	}
	waitFirstTouchConverged(t, e2, root)
	fresh, err := e2.Search(context.Background(), searchRequest(root, "offlineAddedSentinel"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range fresh.Hits {
		if hit.Path == "offline_added.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("后台收敛后停机期新增内容必须可检索: %+v", fresh.Hits)
	}
	if strings.Contains(fresh.DegradedReason, "index-refreshing") {
		t.Fatalf("收敛后不应再披露 refreshing: %q", fresh.DegradedReason)
	}
}

// TestFirstTouchSkippedWhenDenyOrStrictOrCold:deny/strict/真冷仓三边界
// 均不走快路径。
func TestFirstTouchSkippedWhenDenyOrStrictOrCold(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()

	t.Run("deny", func(t *testing.T) {
		opts := embedOptions(server.ts.URL, dim, 8, "t1-deny")
		opts.RetrievalDegrade = DegradeDeny
		t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
		t.Setenv("OPENACE_CACHE_NAMESPACE", "t1deny")
		root := newFixtureWorkspace(t)
		e1, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e1.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		_ = e1.Close(context.Background())
		e2 := restartedEngine(t, opts)
		result, err := e2.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil {
			t.Fatalf("deny 小仓首触应内联同步成功: %v", err)
		}
		if strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("deny 不得走快路径: %q", result.DegradedReason)
		}
	})

	t.Run("strict", func(t *testing.T) {
		opts := embedOptions(server.ts.URL, dim, 8, "t1-strict")
		opts.QualityStrict = true
		t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
		t.Setenv("OPENACE_CACHE_NAMESPACE", "t1strict")
		root := newFixtureWorkspace(t)
		e1, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e1.Sync(context.Background(), syncRequest(root)); err != nil {
			t.Fatal(err)
		}
		_ = e1.Close(context.Background())
		e2 := restartedEngine(t, opts)
		result, err := e2.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil {
			t.Fatalf("strict 小仓首触应内联同步成功: %v", err)
		}
		if strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("strict 不得走快路径: %q", result.DegradedReason)
		}
	})

	t.Run("cold", func(t *testing.T) {
		opts := embedOptions(server.ts.URL, dim, 8, "t1-cold")
		t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
		t.Setenv("OPENACE_CACHE_NAMESPACE", "t1cold")
		root := newFixtureWorkspace(t)
		e := restartedEngine(t, opts)
		result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
		if err != nil {
			t.Fatalf("冷仓小仓首触应内联首建成功: %v", err)
		}
		if strings.Contains(result.DegradedReason, "index-refreshing") {
			t.Fatalf("真冷仓不得披露 refreshing: %q", result.DegradedReason)
		}
	})
}

// TestUnchangedWorkspaceSyncIsNoOp 回归 T1-churn 的引擎面:含纯空白文件
// 的工作区,连续两次显式 Sync 必须落在同一 revision(修复前实证:gradle
// 真实仓每次 sync 都发新版,Added=0 也发版)。
func TestUnchangedWorkspaceSyncIsNoOp(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "t1-noop")
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "t1noop")
	root := newFixtureWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "whitespace.gradle"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	first, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if second.IndexRevision != first.IndexRevision {
		t.Fatalf("未变更工作区重复 sync 不得发新版: %s -> %s", first.IndexRevision, second.IndexRevision)
	}
}
