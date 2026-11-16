package localengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
)

// 失锁恢复测试。两个引擎实例共用同一 cache 目录，模拟共享同一索引子树的两个
// daemon 进程：同进程的进程号存活、锁文件的心跳时间是新的，取锁判定与另一个
// 存活进程持锁相同。

// useSharedCache 为本测试设置独立的 cache 目录；之后构造的引擎共用这棵目录，
// 也就共用同一把锁文件。
func useSharedCache(t *testing.T) {
	t.Helper()
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
}

// newSharedCacheEngine 在 useSharedCache 设好的 cache 目录上构造引擎，测试结束时关闭。
func newSharedCacheEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// workspaceStore 返回 e 为 root 打开的索引 Store 与 workspaceKey。
func workspaceStore(t *testing.T, e *Engine, root string) (*index.Store, string) {
	t.Helper()
	_, key, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := e.storeFor(key)
	if err != nil {
		t.Fatal(err)
	}
	return store, key
}

func readLockFile(t *testing.T, store *index.Store) string {
	t.Helper()
	content, err := os.ReadFile(store.LockPath())
	if err != nil {
		t.Fatalf("读取锁文件: %v", err)
	}
	return string(content)
}

// TestLockLostRecoveryAfterTakeover：第一个引擎同步后持有写锁；锁文件被移除，
// 第二个引擎取得写锁并为新增文件发布新 revision。第一个引擎此后：
//  1. 第一次同步报 index.ErrLockLost，不改动第二个引擎的锁文件；
//  2. 第二个引擎仍持锁时，下一次同步报 index.ErrLockHeld。改动前引擎一直复用
//     缓存里失效的锁，每次同步都报 ErrLockLost，直到进程重启都无法恢复；
//  3. 第二个引擎关闭、释放写锁后，下一次同步成功，结果基于第二个引擎发布的
//     revision（从磁盘读到的最新 manifest），不是自己早先的 revision；
//  4. 之后的内容变化照常发布，新 revision 接在恢复后的 revision 之后。
func TestLockLostRecoveryAfterTakeover(t *testing.T) {
	useSharedCache(t)
	ctx := context.Background()
	root := newFixtureWorkspace(t)
	first := newSharedCacheEngine(t, Options{})
	initial, err := first.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	store, _ := workspaceStore(t, first, root)

	if err := os.Remove(store.LockPath()); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "takeover.go", "package app\n\n// TakeoverOnly 只在接管者发布的 revision 里出现。\nfunc TakeoverOnly() {}\n")
	second := newSharedCacheEngine(t, Options{})
	takeover, err := second.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("接管者同步: %v", err)
	}
	if takeover.IndexRevision == "" || takeover.IndexRevision == initial.IndexRevision {
		t.Fatalf("接管者应发布新 revision: initial=%s takeover=%s", initial.IndexRevision, takeover.IndexRevision)
	}
	holder := readLockFile(t, store)

	if _, err := first.Sync(ctx, syncRequest(root)); !errors.Is(err, index.ErrLockLost) {
		t.Fatalf("锁被接管后的第一次同步应报 ErrLockLost: %v", err)
	}
	if got := readLockFile(t, store); got != holder {
		t.Fatalf("失锁的一方不得改动接管者的锁文件: before=%q after=%q", holder, got)
	}

	if _, err := first.Sync(ctx, syncRequest(root)); !errors.Is(err, index.ErrLockHeld) {
		t.Fatalf("接管者仍持锁时下一次同步应重新取锁并报 ErrLockHeld，不应继续复用失效的缓存锁: %v", err)
	}
	if got := readLockFile(t, store); got != holder {
		t.Fatalf("取锁失败不得改动接管者的锁文件: before=%q after=%q", holder, got)
	}

	if err := second.Close(ctx); err != nil {
		t.Fatalf("接管者关闭: %v", err)
	}
	if _, err := os.Stat(store.LockPath()); !os.IsNotExist(err) {
		t.Fatalf("接管者关闭后锁文件应已删除: %v", err)
	}
	recovered, err := first.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("接管者释放写锁后同步应恢复: %v", err)
	}
	active, _ := loadActiveManifest(t, first, root)
	if active.Revision != recovered.IndexRevision {
		t.Fatalf("恢复同步的结果应是 active revision: result=%s active=%s", recovered.IndexRevision, active.Revision)
	}
	if recovered.IndexRevision != takeover.IndexRevision && active.PreviousRevision != takeover.IndexRevision {
		t.Fatalf("恢复同步应基于接管者的 revision %s: result=%s previous=%s", takeover.IndexRevision, recovered.IndexRevision, active.PreviousRevision)
	}
	if _, ok := active.Files["takeover.go"]; !ok {
		t.Fatalf("恢复后的 revision %s 应包含接管者索引的 takeover.go", active.Revision)
	}

	writeFixture(t, root, "after.go", "package app\n\n// AfterRecovery 在恢复之后写入。\nfunc AfterRecovery() {}\n")
	next, err := first.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("恢复后的同步: %v", err)
	}
	latest, _ := loadActiveManifest(t, first, root)
	if next.IndexRevision == recovered.IndexRevision || latest.Revision != next.IndexRevision || latest.PreviousRevision != recovered.IndexRevision {
		t.Fatalf("恢复后的内容变化应发布在恢复的 revision 之后: recovered=%s next=%s active=%s previous=%s",
			recovered.IndexRevision, next.IndexRevision, latest.Revision, latest.PreviousRevision)
	}
}

// TestInterimPublishLockLossAbortsBuild：启用语义的首建在切分结束、发布词法中间
// revision 之前失去写锁：锁文件被移除，第二个引擎取得写锁，按改写后的 main.go
// 完成首建并发布语义完整的 revision，随后关闭并释放写锁。第一个引擎的这次构建：
//  1. 以 index.ErrLockLost 整体终止；
//  2. 失锁之后不再向 embedding provider 发请求。改动前中间发布失锁按一般的中间
//     发布失败放过，构建继续为接管者 revision 里已不存在的旧 main.go 付费嵌入；
//  3. active revision 仍是接管者发布的 revision。此时锁文件已不存在，旧构建若在
//     最终发布处重新取锁会成功，并用基于过时内容的 manifest 覆盖接管者的 revision。
//
// 之后的一次同步重新取得写锁，读到接管者的 revision，内容与向量都已齐全，不再送审。
func TestInterimPublishLockLossAbortsBuild(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	useSharedCache(t)
	ctx := context.Background()
	root := newFixtureWorkspace(t)
	opts := embedOptions(server.ts.URL, dim, 2, "fake-model")
	first := newSharedCacheEngine(t, opts)
	second := newSharedCacheEngine(t, opts)
	store, _ := workspaceStore(t, first, root)

	// 钩子在构建 goroutine 里运行，不能调用 t.Fatal；错误记下来由测试 goroutine 检查。
	// 这些变量在 first.Sync 返回前写完，Sync 等待构建结束，读写之间有先后关系。
	var fired atomic.Bool
	var takeover engine.Result
	var takeoverErr, closeErr error
	callsAfterTakeover := -1
	drainParserPoolsHook = func(engine.IndexStage) {
		// 只在第一个引擎的构建里触发；第二个引擎的构建经过这里时 fired 已置位。
		if !fired.CompareAndSwap(false, true) {
			return
		}
		if err := os.Remove(store.LockPath()); err != nil {
			takeoverErr = err
			return
		}
		// main.go 整体换成别的内容：第一个引擎已切分的旧 main.go 不在接管者的 revision 里，
		// 也不在任何已付费的向量里。
		replaced := "package takeover\n\n// TakeoverEdit 只在接管者发布的 revision 里出现。\nfunc TakeoverEdit() int {\n\treturn 42\n}\n"
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(replaced), 0o600); err != nil {
			takeoverErr = err
			return
		}
		takeover, takeoverErr = second.Sync(ctx, syncRequest(root))
		closeErr = second.Close(ctx)
		callsAfterTakeover = server.callCount()
	}
	t.Cleanup(func() { drainParserPoolsHook = nil })

	_, err := first.Sync(ctx, syncRequest(root))
	drainParserPoolsHook = nil
	if !fired.Load() || takeoverErr != nil || closeErr != nil {
		t.Fatalf("接管步骤没有按预期完成: fired=%v takeover=%v close=%v", fired.Load(), takeoverErr, closeErr)
	}
	if takeover.IndexRevision == "" || takeover.SemanticCoverage != "100%" {
		t.Fatalf("接管者应发布语义完整的 revision: %+v", takeover)
	}
	if !errors.Is(err, index.ErrLockLost) {
		t.Fatalf("中间发布前失锁，整次构建应以 ErrLockLost 终止: %v", err)
	}
	if calls := server.callCount(); calls != callsAfterTakeover {
		t.Fatalf("中间发布失锁后不应继续嵌入: 接管完成时 %d 次请求，构建结束时 %d 次", callsAfterTakeover, calls)
	}
	active, _ := loadActiveManifest(t, first, root)
	if active.Revision != takeover.IndexRevision {
		t.Fatalf("失锁的构建不得覆盖接管者的 revision: active=%s takeover=%s", active.Revision, takeover.IndexRevision)
	}

	callsBefore := server.callCount()
	recovered, err := first.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("接管者释放写锁后同步应恢复: %v", err)
	}
	active, _ = loadActiveManifest(t, first, root)
	if recovered.IndexRevision != takeover.IndexRevision && active.PreviousRevision != takeover.IndexRevision {
		t.Fatalf("恢复同步应基于接管者的 revision %s: result=%s previous=%s", takeover.IndexRevision, recovered.IndexRevision, active.PreviousRevision)
	}
	if recovered.SemanticCoverage != "100%" {
		t.Fatalf("恢复后应语义完整: %+v", recovered)
	}
	if calls := server.callCount(); calls != callsBefore {
		t.Fatalf("接管者已嵌入的内容不应再次送审: before=%d after=%d", callsBefore, calls)
	}
}

// TestLockLostCloseRace：缓存的写锁已被其他持有者接管时，多个取锁调用、一次同步
// 与 Close 并发执行。所有调用都要在期限内返回且全部失败（失锁、锁被持有、构建被
// Close 取消或引擎已关闭），不得 panic 或死锁；接管者的锁文件保持原样；Close 之后
// 取锁仍被拒绝，引擎不再登记写锁。
func TestLockLostCloseRace(t *testing.T) {
	useSharedCache(t)
	root := newFixtureWorkspace(t)
	const rounds = 20
	const acquirers = 3
	for round := 0; round < rounds; round++ {
		e, err := New(Options{})
		if err != nil {
			t.Fatal(err)
		}
		store, key := workspaceStore(t, e, root)
		if _, err := e.acquireWriteLock(key, store); err != nil {
			t.Fatalf("round %d 取锁: %v", round, err)
		}
		// 接管者改写锁文件：身份换成别人，进程号存活，心跳时间是新的。
		foreign := fmt.Sprintf("pid=%d rand=takeover%02d\n", os.Getpid(), round)
		if err := os.WriteFile(store.LockPath(), []byte(foreign), 0o600); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		results := make(chan error, acquirers+1)
		var wg sync.WaitGroup
		for i := 0; i < acquirers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.acquireWriteLock(key, store)
				results <- err
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.Sync(context.Background(), syncRequest(root))
			results <- err
		}()
		closeDone := make(chan error, 1)
		go func() {
			<-start
			closeDone <- e.Close(context.Background())
		}()
		close(start)
		finished := make(chan struct{})
		go func() {
			wg.Wait()
			close(finished)
		}()
		deadline := time.After(30 * time.Second)
		select {
		case <-finished:
		case <-deadline:
			t.Fatalf("round %d: 取锁或同步与 Close 并发时没有返回", round)
		}
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("round %d Close: %v", round, err)
			}
		case <-deadline:
			t.Fatalf("round %d: Close 没有返回", round)
		}
		close(results)
		for err := range results {
			if err == nil {
				t.Fatalf("round %d: 锁已被接管，取锁或同步不应成功", round)
			}
			if !errors.Is(err, index.ErrLockLost) && !errors.Is(err, index.ErrLockHeld) &&
				!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "已关闭") {
				t.Fatalf("round %d: 意外的错误: %v", round, err)
			}
		}
		if got := readLockFile(t, store); got != foreign {
			t.Fatalf("round %d: 接管者的锁文件被改动: %q", round, got)
		}
		if _, err := e.acquireWriteLock(key, store); err == nil {
			t.Fatalf("round %d: Close 之后取锁应被拒绝", round)
		}
		e.mu.Lock()
		registered := len(e.locks)
		e.mu.Unlock()
		if registered != 0 {
			t.Fatalf("round %d: Close 之后不应再登记写锁: %d", round, registered)
		}
		// 本轮测试自己写入的锁文件，下一轮重新取锁前删除。
		if err := os.Remove(store.LockPath()); err != nil {
			t.Fatal(err)
		}
	}
}
