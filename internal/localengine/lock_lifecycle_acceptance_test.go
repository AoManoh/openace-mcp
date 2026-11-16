package localengine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
)

func TestLostWriterRecoversOnlyAfterOtherOwnerReleases(t *testing.T) {
	ctx := context.Background()
	first := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, key, err := first.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	// 保留引擎里的旧锁对象，模拟其在本次写入前已经失去文件所有权。
	first.locks[key].Release()
	second, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(ctx) })
	writeFixture(t, root, "other.go", "package app\nfunc OtherOwnerChange() {}\n")
	if _, err := second.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Sync(ctx, syncRequest(root)); !errors.Is(err, index.ErrLockLost) {
		t.Fatalf("首次发现旧锁失效必须终止：%v", err)
	}
	if _, err := first.Sync(ctx, syncRequest(root)); !errors.Is(err, index.ErrLockHeld) {
		t.Fatalf("后续同步必须检查当前持有者：%v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "resumed.go", "package app\nfunc ResumedOwnerChange() {}\n")
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("对方释放后应恢复同步：%v", err)
	}
	result, err := first.Search(ctx, searchRequest(root, "OtherOwnerChange ResumedOwnerChange"))
	if err != nil || !strings.Contains(result.Text, "func OtherOwnerChange") || !strings.Contains(result.Text, "func ResumedOwnerChange") {
		t.Fatalf("恢复时必须保留接管者与本轮的新内容：%v, %s", err, result.Text)
	}
}

// 另一个写入口发现失锁后，仍在嵌入的旧构建也不能重新拿锁发布旧快照。
func TestLostInFlightBuildCannotReacquireAfterPlanInvalidatesLock(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var firstChunk atomic.Bool
	var requests atomic.Int64
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		inner.ServeHTTP(w, r)
	})
	// 在首建快照已切分、尚未发布与嵌入时让出执行，接管者可发布完整版本。
	drainParserPoolsHook = func(engine.IndexStage) {
		if firstChunk.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	opts := embedOptions(server.ts.URL, dim, 16, "fake-model")
	opts.Embedding.Timeout = 20 * time.Second
	first := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	done := make(chan error, 1)
	go func() { _, err := first.Sync(ctx, syncRequest(root)); done <- err }()
	t.Cleanup(func() {
		unblock()
		_ = first.Close(ctx)
		drainParserPoolsHook = nil
		server.ts.Close()
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("旧构建未到达中间发布之前的闸门")
	}
	_, key, err := first.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	old := first.locks[key]
	first.mu.Unlock()
	old.Release()
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(ctx) })
	writeFixture(t, root, "takeover.go", "package app\nfunc TakeoverRevisionMustSurvive() {}\n")
	published, err := second.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PlanEmbedJobs(ctx, engineRef(root), func(EmbedJob) error { return nil }); !errors.Is(err, index.ErrLockLost) {
		t.Fatalf("独立写入口应发现旧锁失效：%v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requestsBefore := requests.Load()
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, index.ErrLockLost) {
			t.Fatalf("失锁构建不能重新拿锁并成功发布：%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("旧构建未退出")
	}
	if got := requests.Load(); got != requestsBefore {
		t.Errorf("失锁构建不应继续嵌入：before %d; after %d", requestsBefore, got)
	}
	store, err := first.storeFor(key)
	if err != nil {
		t.Fatal(err)
	}
	active, ok, err := store.ActiveRevision()
	if err != nil || !ok || active != published.IndexRevision {
		t.Fatalf("接管者发布的 revision 被旧构建覆盖：got %s; want %s; err %v", active, published.IndexRevision, err)
	}
}

func TestRecoveredWriterReopensTakeoverJournal(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 16, "fake-model")
	first := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, key, err := first.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	first.locks[key].Release()
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(ctx) })
	writeFixture(t, root, "pending.go", "package app\nfunc PaidBeforeRecovery() {}\n")
	abort := errors.New("接管者已嵌入，发布前中止")
	segmentWriteHook = func(context.Context, semanticOutcome) error { return abort }
	t.Cleanup(func() { segmentWriteHook = nil })
	_, syncErr := second.Sync(ctx, syncRequest(root))
	segmentWriteHook = nil
	if syncErr == nil || !strings.Contains(syncErr.Error(), abort.Error()) {
		t.Fatalf("接管者必须已嵌入但尚未发布：%v", syncErr)
	}
	if _, err := first.Sync(ctx, syncRequest(root)); !errors.Is(err, index.ErrLockLost) {
		t.Fatalf("旧持有者应发现失锁：%v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before := server.callCount()
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("恢复后同步失败：%v", err)
	}
	if calls := server.callCount(); calls != before {
		t.Fatalf("恢复必须重读接管者的已付费 journal，不能重复嵌入：before %d; after %d", before, calls)
	}
}

// 在途嵌入持有旧 journal 时，另一个写入口发现失锁会关闭旧句柄。
// 延后到达的向量必须让旧构建明确失败，不能发布；后续同步仍能保留接管者的数据。
func TestLostInFlightEmbeddingStopsAfterJournalInvalidation(t *testing.T) {
	const dim = 8
	ctx := context.Background()
	server := newEmbedServer(t, dim)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var holdNext atomic.Bool
	inner := server.ts.Config.Handler
	server.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if holdNext.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	opts := embedOptions(server.ts.URL, dim, 16, "fake-model")
	opts.Embedding.Timeout = 30 * time.Second
	first := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_, key, err := first.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "inflight.go", "package app\nfunc BeforeTakeoverOnly() {}\n")
	holdNext.Store(true)
	done := make(chan error, 1)
	go func() { _, err := first.Sync(ctx, syncRequest(root)); done <- err }()
	// 引擎清理前先放行请求，失败断言也不会把清理卡在 HTTP 闸门上。
	t.Cleanup(unblock)
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("旧构建没有发出嵌入请求：%v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("旧构建未到达嵌入闸门")
	}
	first.mu.Lock()
	old := first.locks[key]
	first.mu.Unlock()
	old.Release()
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(ctx) })
	writeFixture(t, root, "inflight.go", "package app\nfunc AfterTakeoverOnly() {}\n")
	takeover, err := second.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PlanEmbedJobs(ctx, engineRef(root), func(EmbedJob) error { return nil }); !errors.Is(err, index.ErrLockLost) {
		t.Fatalf("另一个写入口应发现失锁：%v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("旧 journal 已失效，在途嵌入返回后不应成功发布")
		}
		t.Logf("在途嵌入构建已停止：%v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("关闭旧 journal 后在途构建没有退出")
	}
	active, _ := loadActiveManifest(t, first, root)
	if active.Revision != takeover.IndexRevision {
		t.Fatalf("旧构建改动了接管者的 revision：got %s; want %s", active.Revision, takeover.IndexRevision)
	}
	before := server.callCount()
	if _, err := first.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("在途构建退出后应恢复同步：%v", err)
	}
	if calls := server.callCount(); calls != before {
		t.Fatalf("恢复不应重复嵌入接管者已发布的内容：before %d; after %d", before, calls)
	}
	result, err := first.Search(ctx, searchRequest(root, "AfterTakeoverOnly"))
	if err != nil || !strings.Contains(result.Text, "func AfterTakeoverOnly") || strings.Contains(result.Text, "func BeforeTakeoverOnly") {
		t.Fatalf("恢复后应检索接管者的新内容：%v; %s", err, result.Text)
	}
}
