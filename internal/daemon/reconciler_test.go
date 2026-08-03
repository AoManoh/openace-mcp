package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

func TestWorkspaceReconcilerDisabledByMode(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "off")
	if reconciler, err := newWorkspaceReconciler(newFakeWatchSyncer()); err != nil || reconciler != nil {
		t.Fatal("watch mode off should disable reconciler")
	}
}

func TestWorkspaceReconcilerSyncsChangedSeenWorkspace(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")
	t.Setenv("OPENACE_WATCH_TIMEOUT", "1s")

	syncer := newFakeWatchSyncer()
	syncer.setChanged(true)
	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)

	reconciler.Observe("/tmp/project")
	syncer.waitForBackgroundSync(t)

	status := engine.WorkspaceStatus{DirectoryPath: "/tmp/project"}
	reconciler.Decorate(&status)
	if !status.WatchEnabled {
		t.Fatalf("unexpected watch status after successful background sync: %+v", status)
	}
	if !status.WatchScheduled {
		t.Fatalf("successful background sync should remain scheduled: %+v", status)
	}
	if status.LastWatchAt == nil || status.LastBackgroundSyncAt == nil {
		t.Fatalf("watch status should expose timestamps: %+v", status)
	}
}

func TestWorkspaceReconcilerSkipsBackgroundSyncWhenUnchanged(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")

	syncer := newFakeWatchSyncer()
	syncer.setChanged(false)
	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)

	reconciler.Observe("/tmp/project")
	syncer.waitForChangeCheck(t)
	time.Sleep(20 * time.Millisecond)
	if got := syncer.backgroundSyncCount(); got != 0 {
		t.Fatalf("unchanged workspace should not be background synced, got %d", got)
	}
}

func TestWorkspaceReconcilerBacksOffAfterProbeError(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")
	t.Setenv("OPENACE_WATCH_BACKOFF_MIN", "20ms")
	t.Setenv("OPENACE_WATCH_BACKOFF_MAX", "20ms")

	syncer := newFakeWatchSyncer()
	syncer.setChangeError(errors.New("probe failed"))
	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)

	reconciler.Observe("/tmp/project")
	syncer.waitForChangeCheck(t)

	status := engine.WorkspaceStatus{DirectoryPath: "/tmp/project"}
	reconciler.Decorate(&status)
	if !status.WatchEnabled || !status.WatchScheduled {
		t.Fatalf("failed probe should leave watch pending: %+v", status)
	}
	if !strings.Contains(status.WatchError, "probe failed") {
		t.Fatalf("watch error should be visible: %+v", status)
	}
	if status.NextWatchAt == nil {
		t.Fatalf("failed probe should schedule a retry: %+v", status)
	}
}

func TestWorkspaceReconcilerDefersWhileWorkspaceSyncInFlight(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")

	syncer := newFakeWatchSyncer()
	syncer.setInFlight(true)
	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)

	reconciler.Observe("/tmp/project")
	syncer.waitForStatusCheck(t)
	time.Sleep(20 * time.Millisecond)
	if got := syncer.changeCheckCount(); got != 0 {
		t.Fatalf("in-flight workspace should not be scanned by background watcher, got checks=%d", got)
	}
	if got := syncer.backgroundSyncCount(); got != 0 {
		t.Fatalf("in-flight workspace should not be background synced, got %d", got)
	}
	status := engine.WorkspaceStatus{DirectoryPath: "/tmp/project"}
	reconciler.Decorate(&status)
	if !status.WatchEnabled || !status.WatchScheduled || status.LastBackgroundSyncAt != nil {
		t.Fatalf("in-flight deferral should only reschedule watcher: %+v", status)
	}
}

// canonicalWatchDir 与 reconciler 内部保持同一 key 口径（Observe 会先做
// ResolveWorkspaceRoot，fake 按 DirectoryPath 记账，二者必须一致）。
func canonicalWatchDir(t *testing.T, dir string) string {
	t.Helper()
	root, err := pathutil.ResolveWorkspaceRoot(dir)
	if err != nil {
		t.Fatalf("resolve workspace root %s: %v", dir, err)
	}
	return root.CanonicalPath
}

// M10 核心断言：A 的后台构建挂起期间，B 的变更监测与后台同步必须照常发生
// （旧串行 run 循环会被 A 队头阻塞，本测试在旧实现上必须失败）。
func TestWorkspaceReconcilerSlowWorkspaceDoesNotBlockOthers(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")
	t.Setenv("OPENACE_WATCH_TIMEOUT", "30s")
	// 不设 OPENACE_RECONCILE_CONCURRENCY：默认并发 2 就应消除队头阻塞。

	dirA := canonicalWatchDir(t, "/tmp/openace-slow-a")
	dirB := canonicalWatchDir(t, "/tmp/openace-fast-b")

	syncer := newFakeWatchSyncer()
	syncer.setChangedFor(dirA, true)
	syncer.setChangedFor(dirB, true)
	started, release := syncer.blockSyncFor(dirA)

	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)
	// LIFO：先放行 A 再执行上面的 Shutdown，避免 fake 阻塞 worker 导致关停超时。
	defer release()

	reconciler.Observe("/tmp/openace-slow-a")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace A background sync did not start")
	}

	// A 已在 worker 手上挂起，此刻 B 的 reconcile 必须仍能在时限内完成。
	reconciler.Observe("/tmp/openace-fast-b")
	syncer.waitForBackgroundSyncOf(t, dirB)

	if got := syncer.backgroundSyncCountFor(dirA); got != 0 {
		t.Fatalf("workspace A should still be blocked in flight, got %d completed syncs", got)
	}
}

// 回归钉：OPENACE_RECONCILE_CONCURRENCY=1 时保持旧串行语义——A 挂起期间
// B 不得被同时 reconcile，A 放行后 B 恢复处理（只串行、不丢失）。
func TestWorkspaceReconcilerConcurrencyOneKeepsSerialBehavior(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")
	t.Setenv("OPENACE_WATCH_TIMEOUT", "30s")
	t.Setenv("OPENACE_RECONCILE_CONCURRENCY", "1")

	dirA := canonicalWatchDir(t, "/tmp/openace-serial-a")
	dirB := canonicalWatchDir(t, "/tmp/openace-serial-b")

	syncer := newFakeWatchSyncer()
	syncer.setChangedFor(dirA, true)
	syncer.setChangedFor(dirB, true)
	started, release := syncer.blockSyncFor(dirA)

	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)
	defer release()

	reconciler.Observe("/tmp/openace-serial-a")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace A background sync did not start")
	}

	reconciler.Observe("/tmp/openace-serial-b")
	time.Sleep(150 * time.Millisecond)
	if got := syncer.backgroundSyncCountFor(dirB); got != 0 {
		t.Fatalf("concurrency=1 must keep serial behavior, workspace B synced %d times while A in flight", got)
	}

	release()
	syncer.waitForBackgroundSyncOf(t, dirB)
}

// running 标志逐个置位：同一轮到期的两个 workspace，只有被 worker 领取的
// 那个才允许 running=true；未领取者必须仍是 false（旧实现 dueWorkspaces
// 批量预置 running，本测试在旧实现上必须失败）。
func TestWorkspaceReconcilerMarksRunningOnClaimOnly(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_INTERVAL", "50ms")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1ms")
	t.Setenv("OPENACE_WATCH_TIMEOUT", "30s")
	// 并发=1 使同轮两个到期 workspace 只有一个能被领取，另一个必须保持未领取态。
	t.Setenv("OPENACE_RECONCILE_CONCURRENCY", "1")

	rawA, rawB := "/tmp/openace-claim-a", "/tmp/openace-claim-b"
	dirA := canonicalWatchDir(t, rawA)
	dirB := canonicalWatchDir(t, rawB)

	syncer := newFakeWatchSyncer()
	syncer.setChangedFor(dirA, true)
	syncer.setChangedFor(dirB, true)
	startedA, releaseA := syncer.blockSyncFor(dirA)
	startedB, releaseB := syncer.blockSyncFor(dirB)

	reconciler, err := newWorkspaceReconciler(syncer)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownReconciler(t, reconciler)
	defer releaseA()
	defer releaseB()

	// 同时 Observe，让 A/B 在同一轮到期（map 迭代序不定，谁先被领取都合法）。
	reconciler.Observe(rawA)
	reconciler.Observe(rawB)

	var claimedRaw, unclaimedRaw string
	select {
	case <-startedA:
		claimedRaw, unclaimedRaw = rawA, rawB
	case <-startedB:
		claimedRaw, unclaimedRaw = rawB, rawA
	case <-time.After(2 * time.Second):
		t.Fatal("neither workspace entered background sync")
	}

	claimed := engine.WorkspaceStatus{DirectoryPath: claimedRaw}
	reconciler.Decorate(&claimed)
	if !claimed.WatchRunning {
		t.Fatalf("claimed workspace should report running: %+v", claimed)
	}
	unclaimed := engine.WorkspaceStatus{DirectoryPath: unclaimedRaw}
	reconciler.Decorate(&unclaimed)
	if unclaimed.WatchRunning {
		t.Fatalf("workspace not yet claimed by a worker must not report running: %+v", unclaimed)
	}
	if !unclaimed.WatchEnabled || !unclaimed.WatchScheduled {
		t.Fatalf("unclaimed workspace should stay scheduled: %+v", unclaimed)
	}
}

// 非法 OPENACE_RECONCILE_CONCURRENCY 必须启动期报错（fail-fast，禁止静默
// 回退默认值），错误信息需点名环境变量。
func TestWorkspaceReconcilerRejectsInvalidConcurrency(t *testing.T) {
	for _, value := range []string{"0", "-1", "abc"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OPENACE_WATCH_MODE", "seen")
			t.Setenv("OPENACE_RECONCILE_CONCURRENCY", value)
			reconciler, err := newWorkspaceReconciler(newFakeWatchSyncer())
			if reconciler != nil {
				// 不报错误创建成功时先回收 goroutine 再 Fatal。
				shutdownReconciler(t, reconciler)
			}
			if err == nil {
				t.Fatalf("invalid OPENACE_RECONCILE_CONCURRENCY %q should fail startup", value)
			}
			if !strings.Contains(err.Error(), "OPENACE_RECONCILE_CONCURRENCY") {
				t.Fatalf("startup error should name the env var: %v", err)
			}
		})
	}
}

func shutdownReconciler(t *testing.T, reconciler *workspaceReconciler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reconciler.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown reconciler: %v", err)
	}
}

type fakeWatchSyncer struct {
	mu              sync.Mutex
	changed         bool
	changedByDir    map[string]bool
	changeErr       error
	checks          int
	backgroundSyncs int
	syncsByDir      map[string]int
	statusChecks    int
	checkCh         chan struct{}
	syncCh          chan struct{}
	dirSyncCh       chan string
	statusCh        chan struct{}
	inFlight        bool
	syncBlocks      map[string]*fakeSyncBlock
}

// fakeSyncBlock 是可注入的同步闸门：SyncBackground 进入即发 started 信号，
// 然后阻塞到 release 被关闭——模拟"大仓分钟级构建"的慢 workspace。
type fakeSyncBlock struct {
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newFakeWatchSyncer() *fakeWatchSyncer {
	return &fakeWatchSyncer{
		changedByDir: make(map[string]bool),
		syncsByDir:   make(map[string]int),
		checkCh:      make(chan struct{}, 8),
		syncCh:       make(chan struct{}, 8),
		dirSyncCh:    make(chan string, 32),
		statusCh:     make(chan struct{}, 8),
		syncBlocks:   make(map[string]*fakeSyncBlock),
	}
}

func (s *fakeWatchSyncer) WorkspaceChanged(_ context.Context, ref engine.WorkspaceRef) (bool, error) {
	s.mu.Lock()
	s.checks++
	changed := s.changed
	if value, ok := s.changedByDir[ref.DirectoryPath]; ok {
		changed = value
	}
	err := s.changeErr
	s.mu.Unlock()
	s.signal(s.checkCh)
	return changed, err
}

func (s *fakeWatchSyncer) ListWorkspaceStatuses(context.Context) ([]engine.WorkspaceStatus, error) {
	return nil, nil
}

func (s *fakeWatchSyncer) WorkspaceStatus(ctx context.Context, ref engine.WorkspaceRef) (engine.WorkspaceStatus, error) {
	s.mu.Lock()
	s.statusChecks++
	inFlight := s.inFlight
	s.mu.Unlock()
	s.signal(s.statusCh)
	return engine.WorkspaceStatus{DirectoryPath: ref.DirectoryPath, InFlight: inFlight}, nil
}

func (s *fakeWatchSyncer) SyncBackground(_ context.Context, req engine.SyncRequest) (engine.Result, error) {
	dir := req.Workspace.DirectoryPath
	s.mu.Lock()
	block := s.syncBlocks[dir]
	s.mu.Unlock()
	if block != nil {
		block.startedOnce.Do(func() { close(block.started) })
		// 不持锁阻塞：其余 workspace 的 WorkspaceChanged/记账不受影响。
		<-block.release
	}
	s.mu.Lock()
	s.backgroundSyncs++
	s.syncsByDir[dir]++
	if _, ok := s.changedByDir[dir]; ok {
		s.changedByDir[dir] = false
	} else {
		s.changed = false
	}
	s.mu.Unlock()
	s.signal(s.syncCh)
	s.signalDir(dir)
	return engine.Result{CheckpointID: "checkpoint-background", FileCount: 1}, nil
}

func (s *fakeWatchSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return s.SyncBackground(ctx, req)
}

func (s *fakeWatchSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	result, err := s.Sync(ctx, engine.SyncRequest{Workspace: req.Workspace})
	if err != nil {
		return engine.Result{}, err
	}
	result.Text = "retrieved"
	return result, nil
}

func (s *fakeWatchSyncer) setChanged(changed bool) {
	s.mu.Lock()
	s.changed = changed
	s.mu.Unlock()
}

// setChangedFor 按 workspace 覆盖变更判定（后台同步后只清本目录标记，
// 不影响其他 workspace——多 workspace 并发测试需要独立记账）。
func (s *fakeWatchSyncer) setChangedFor(dir string, changed bool) {
	s.mu.Lock()
	s.changedByDir[dir] = changed
	s.mu.Unlock()
}

// blockSyncFor 给指定 workspace 装同步闸门；release 幂等可重复调用。
func (s *fakeWatchSyncer) blockSyncFor(dir string) (started <-chan struct{}, release func()) {
	block := &fakeSyncBlock{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	s.mu.Lock()
	s.syncBlocks[dir] = block
	s.mu.Unlock()
	return block.started, func() {
		block.releaseOnce.Do(func() { close(block.release) })
	}
}

func (s *fakeWatchSyncer) setChangeError(err error) {
	s.mu.Lock()
	s.changeErr = err
	s.mu.Unlock()
}

func (s *fakeWatchSyncer) setInFlight(inFlight bool) {
	s.mu.Lock()
	s.inFlight = inFlight
	s.mu.Unlock()
}

func (s *fakeWatchSyncer) changeCheckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks
}

func (s *fakeWatchSyncer) backgroundSyncCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backgroundSyncs
}

func (s *fakeWatchSyncer) backgroundSyncCountFor(dir string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncsByDir[dir]
}

func (s *fakeWatchSyncer) waitForChangeCheck(t *testing.T) {
	t.Helper()
	select {
	case <-s.checkCh:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace change check did not run")
	}
}

func (s *fakeWatchSyncer) waitForStatusCheck(t *testing.T) {
	t.Helper()
	select {
	case <-s.statusCh:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace status check did not run")
	}
}

func (s *fakeWatchSyncer) waitForBackgroundSync(t *testing.T) {
	t.Helper()
	select {
	case <-s.syncCh:
	case <-time.After(2 * time.Second):
		t.Fatal("background sync did not run")
	}
}

// waitForBackgroundSyncOf 等待指定 workspace 完成一次后台同步；
// 其他 workspace 的完成事件会被跳过。
func (s *fakeWatchSyncer) waitForBackgroundSyncOf(t *testing.T, dir string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-s.dirSyncCh:
			if got == dir {
				return
			}
		case <-deadline:
			t.Fatalf("background sync of %s did not run", dir)
		}
	}
}

func (s *fakeWatchSyncer) signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *fakeWatchSyncer) signalDir(dir string) {
	select {
	case s.dirSyncCh <- dir:
	default:
	}
}
