package localengine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/vector"
)

// recordVectorLoads 记录段缓存里没有、实际执行 vector.Load 的分段目录名，返回读取
// 当前记录的函数。
func recordVectorLoads(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var loaded []string
	loadVectorSegmentHook = func(dir string) {
		mu.Lock()
		loaded = append(loaded, filepath.Base(dir))
		mu.Unlock()
	}
	t.Cleanup(func() { loadVectorSegmentHook = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), loaded...)
	}
}

func cachedVectorSegments(e *Engine) int {
	e.vectorMu.Lock()
	defer e.vectorMu.Unlock()
	return len(e.vectorSegments)
}

// TestDeltaPublishDoesNotReloadSharedVectorSegments：检索加载过向量段之后，后台同步
// 连续发布两个 delta revision（期间没有检索），下一次检索只加载两个新增分段，不
// 重新读取并校验没有变化的基础分段。改动前发布时旧句柄一关闭，基础分段就从段缓存
// 移除，每个新 revision 的第一次检索都要把全部向量文件重新校验一遍。
func TestDeltaPublishDoesNotReloadSharedVectorSegments(t *testing.T) {
	const dim = 8
	loads := recordVectorLoads(t)
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "carry-model"))
	root := newFixtureWorkspace(t)
	ctx := context.Background()

	if _, err := e.Search(ctx, searchRequest(root, "establish user session")); err != nil {
		t.Fatalf("first search: %v", err)
	}
	first := loads()
	if len(first) != 1 {
		t.Fatalf("first search should load exactly the base segment, got %v", first)
	}

	writeFixture(t, root, "extra_one.go", "package app\n\n// ExtraOne 是第一次改动新增的函数。\nfunc ExtraOne() {}\n")
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("first delta sync: %v", err)
	}
	writeFixture(t, root, "extra_two.go", "package app\n\n// ExtraTwo 是第二次改动新增的函数。\nfunc ExtraTwo() {}\n")
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("second delta sync: %v", err)
	}

	result, err := e.Search(ctx, searchRequest(root, "ExtraTwo"))
	if err != nil {
		t.Fatalf("search after two delta publishes: %v", err)
	}
	if result.RetrievalMode != "hybrid" || result.SemanticCoverage != "100%" || result.DegradedReason != "" {
		t.Fatalf("search on the new revision should be a full hybrid result: mode=%q coverage=%q degraded=%q",
			result.RetrievalMode, result.SemanticCoverage, result.DegradedReason)
	}
	if !strings.Contains(result.Text, "ExtraTwo") {
		t.Fatalf("new revision content should be retrievable: %q", result.Text)
	}
	after := loads()[len(first):]
	for _, dir := range after {
		if dir == first[0] {
			t.Fatalf("base segment %s was loaded and verified again after delta publishes; loads after first search: %v", first[0], after)
		}
	}
	if len(after) != 2 {
		t.Fatalf("only the two new delta segments should be loaded, got %v", after)
	}
}

// TestVectorSegmentsReleasedWhenTheyLeaveActiveRevision：发布时只为新 active revision
// 仍引用的向量段保留引用。全量重建后旧分段不在新 manifest 里，保留的引用随这次
// 发布释放，旧的与新的两套完整向量不会同时留在段缓存里。
func TestVectorSegmentsReleasedWhenTheyLeaveActiveRevision(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "carry-model"))
	root := newFixtureWorkspace(t)
	ctx := context.Background()

	if _, err := e.Search(ctx, searchRequest(root, "establish user session")); err != nil {
		t.Fatalf("first search: %v", err)
	}
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "extra_one.go", "package app\n\nfunc ExtraOne() {}\n")
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	if n := cachedVectorSegments(e); n != 1 {
		t.Fatalf("after a delta publish the loaded base segment should stay cached (1), got %d", n)
	}

	// 登记向量修复后，下一次同步走全量重建，发布只含一个新分段的 revision。
	e.markVectorRepair(workspaceKey)
	writeFixture(t, root, "extra_two.go", "package app\n\nfunc ExtraTwo() {}\n")
	rebuilt, err := e.Sync(ctx, syncRequest(root))
	if err != nil {
		t.Fatalf("full rebuild sync: %v", err)
	}
	if !strings.HasPrefix(rebuilt.BuildMode, "full") {
		t.Fatalf("vector repair should force a full rebuild, got build mode %q", rebuilt.BuildMode)
	}
	if n := cachedVectorSegments(e); n != 0 {
		t.Fatalf("segments that left the active revision should be released at publish, %d still cached", n)
	}
	e.vectorMu.Lock()
	retained := len(e.vectorRetained)
	e.vectorMu.Unlock()
	if retained != 0 {
		t.Fatalf("no retained segment refs expected after the rebuild, got %d workspaces", retained)
	}
}

// TestCloseReleasesRetainedVectorSegments：引擎关闭时，发布时保留的向量段引用与
// 句柄持有的引用一起释放，段缓存清空（映射解除、文件关闭）。
func TestCloseReleasesRetainedVectorSegments(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "carry-model"))
	root := newFixtureWorkspace(t)
	ctx := context.Background()

	if _, err := e.Search(ctx, searchRequest(root, "establish user session")); err != nil {
		t.Fatalf("first search: %v", err)
	}
	writeFixture(t, root, "extra_one.go", "package app\n\nfunc ExtraOne() {}\n")
	if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	if n := cachedVectorSegments(e); n != 1 {
		t.Fatalf("setup: the loaded base segment should stay cached after the delta publish, got %d", n)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := cachedVectorSegments(e); n != 0 {
		t.Fatalf("Close should release retained vector segments, %d still cached", n)
	}
	// 关闭之后再有发布收尾（构建 goroutine 晚于 Close 退出）也不再保留引用。
	e.retireHandles("late-workspace", "late-revision", []string{"late-key"})
	if n := cachedVectorSegments(e); n != 0 {
		t.Fatalf("retiring after Close must not retain segments, %d cached", n)
	}
}

// TestConcurrentSearchAndPublishKeepVectorRefsBalanced：检索与连续发布并发进行时，
// 段缓存里每个分段的引用数始终等于“持有它的句柄数加上发布时留下的那一个引用”，
// 检索不报错；引擎关闭后段缓存清空。引用多减一次会在检索中途解除映射，少减一次
// 会让映射留到进程退出，这两种情况都会在这里暴露。
func TestConcurrentSearchAndPublishKeepVectorRefsBalanced(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 8, "carry-model"))
	root := newFixtureWorkspace(t)
	ctx := context.Background()
	if _, err := e.Search(ctx, searchRequest(root, "establish user session")); err != nil {
		t.Fatalf("first search: %v", err)
	}

	stop := make(chan struct{})
	errs := make(chan error, 4)
	var searchers sync.WaitGroup
	for i := 0; i < 4; i++ {
		searchers.Add(1)
		go func() {
			defer searchers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				result, err := e.Search(ctx, searchRequest(root, "establish user session"))
				if err == nil && result.RetrievalMode != "hybrid" {
					err = errors.New("search during publishes fell out of hybrid mode: " + result.RetrievalMode + " " + result.DegradedReason)
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}
	for round := 0; round < 10; round++ {
		writeFixture(t, root, fmt.Sprintf("round_%02d.go", round), fmt.Sprintf("package app\n\nfunc Round%02d() {}\n", round))
		if _, err := e.Sync(ctx, syncRequest(root)); err != nil {
			close(stop)
			searchers.Wait()
			t.Fatalf("sync round %d: %v", round, err)
		}
	}
	close(stop)
	searchers.Wait()
	select {
	case err := <-errs:
		t.Fatalf("search failed while revisions were being published: %v", err)
	default:
	}

	// 静止后核对引用数：句柄持有的加上发布时留下的。
	e.mu.Lock()
	e.vectorMu.Lock()
	want := make(map[string]int)
	for _, handle := range e.handles {
		for _, key := range handle.vecKeys {
			want[key]++
		}
	}
	for _, keys := range e.vectorRetained {
		for _, key := range keys {
			want[key]++
		}
	}
	var mismatch []string
	for key, shared := range e.vectorSegments {
		if shared.refs != want[key] {
			mismatch = append(mismatch, fmt.Sprintf("%s refs=%d want=%d", filepath.Base(strings.SplitN(key, "\x00", 2)[0]), shared.refs, want[key]))
		}
		delete(want, key)
	}
	for key, n := range want {
		mismatch = append(mismatch, fmt.Sprintf("%s missing from cache, %d holders", filepath.Base(strings.SplitN(key, "\x00", 2)[0]), n))
	}
	e.vectorMu.Unlock()
	e.mu.Unlock()
	if len(mismatch) > 0 {
		t.Fatalf("vector segment refs out of balance: %v", mismatch)
	}

	if err := e.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := cachedVectorSegments(e); n != 0 {
		t.Fatalf("Close should leave no cached vector segment, got %d", n)
	}
}

// TestRetainSkipsHeapBackedAndLoadingSegments：发布时只保留已经加载完成、以 mmap
// 驻留的向量段。heap 形态的向量是匿名内存，内核不能回收，保持发布即释放；还在
// 加载的段结果未知，不保留。
func TestRetainSkipsHeapBackedAndLoadingSegments(t *testing.T) {
	const dim = 8
	e := newTestEngine(t)

	// 0 行的向量段不建映射，Load 得到 heap 形态的索引。
	heapDir := t.TempDir()
	dataSum, indexSum, err := vector.Write(heapDir, dim, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	heapIx, err := vector.Load(heapDir, dim, dataSum, indexSum)
	if err != nil {
		t.Fatal(err)
	}
	if heapIx.Mapped() {
		t.Fatal("setup: an empty vector segment should not be mmap-backed")
	}
	ready := make(chan struct{})
	close(ready)
	const heapKey, loadingKey = "heap-key", "loading-key"
	e.vectorMu.Lock()
	e.vectorSegments[heapKey] = &sharedVectorIndex{ready: ready, ix: heapIx, refs: 1}
	e.vectorSegments[loadingKey] = &sharedVectorIndex{ready: make(chan struct{}), refs: 1}
	e.vectorMu.Unlock()

	e.retireHandles("workspace", "revision", []string{heapKey, loadingKey, "absent-key"})

	e.vectorMu.Lock()
	heapRefs, loadingRefs := e.vectorSegments[heapKey].refs, e.vectorSegments[loadingKey].refs
	retained := len(e.vectorRetained)
	delete(e.vectorSegments, heapKey)
	delete(e.vectorSegments, loadingKey)
	e.vectorMu.Unlock()
	_ = heapIx.Close()
	if heapRefs != 1 || loadingRefs != 1 || retained != 0 {
		t.Fatalf("heap-backed and still-loading segments must not be retained: heap refs=%d loading refs=%d retained workspaces=%d",
			heapRefs, loadingRefs, retained)
	}
}
