package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func TestWorkspaceReconcilerDisabledByMode(t *testing.T) {
	t.Setenv("OPENACE_WATCH_MODE", "off")
	if reconciler := newWorkspaceReconciler(newFakeWatchSyncer()); reconciler != nil {
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
	reconciler := newWorkspaceReconciler(syncer)
	defer shutdownReconciler(t, reconciler)

	reconciler.Observe("/tmp/project")
	syncer.waitForBackgroundSync(t)

	status := workspace.WorkspaceStatus{DirectoryPath: "/tmp/project"}
	reconciler.Decorate(&status)
	if !status.WatchEnabled {
		t.Fatalf("unexpected watch status after successful background sync: %+v", status)
	}
	if !status.WatchScheduled || status.WatchRunning {
		t.Fatalf("successful background sync should be scheduled but not running: %+v", status)
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
	reconciler := newWorkspaceReconciler(syncer)
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
	reconciler := newWorkspaceReconciler(syncer)
	defer shutdownReconciler(t, reconciler)

	reconciler.Observe("/tmp/project")
	syncer.waitForChangeCheck(t)

	status := workspace.WorkspaceStatus{DirectoryPath: "/tmp/project"}
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
	reconciler := newWorkspaceReconciler(syncer)
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
	status := workspace.WorkspaceStatus{DirectoryPath: "/tmp/project"}
	reconciler.Decorate(&status)
	if !status.WatchEnabled || !status.WatchScheduled || status.LastBackgroundSyncAt != nil {
		t.Fatalf("in-flight deferral should only reschedule watcher: %+v", status)
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
	changeErr       error
	checks          int
	backgroundSyncs int
	statusChecks    int
	checkCh         chan struct{}
	syncCh          chan struct{}
	statusCh        chan struct{}
	inFlight        bool
}

func newFakeWatchSyncer() *fakeWatchSyncer {
	return &fakeWatchSyncer{
		checkCh:  make(chan struct{}, 8),
		syncCh:   make(chan struct{}, 8),
		statusCh: make(chan struct{}, 8),
	}
}

func (s *fakeWatchSyncer) WorkspaceChanged(context.Context, string) (bool, error) {
	s.mu.Lock()
	s.checks++
	changed := s.changed
	err := s.changeErr
	s.mu.Unlock()
	s.signal(s.checkCh)
	return changed, err
}

func (s *fakeWatchSyncer) ListWorkspaceStatuses(context.Context) ([]workspace.WorkspaceStatus, error) {
	return nil, nil
}

func (s *fakeWatchSyncer) WorkspaceStatus(ctx context.Context, dir string) (workspace.WorkspaceStatus, error) {
	s.mu.Lock()
	s.statusChecks++
	inFlight := s.inFlight
	s.mu.Unlock()
	s.signal(s.statusCh)
	return workspace.WorkspaceStatus{DirectoryPath: dir, InFlight: inFlight}, nil
}

func (s *fakeWatchSyncer) SyncBackground(context.Context, string) (workspace.Result, error) {
	s.mu.Lock()
	s.backgroundSyncs++
	s.changed = false
	s.mu.Unlock()
	s.signal(s.syncCh)
	return workspace.Result{CheckpointID: "checkpoint-background", FileCount: 1}, nil
}

func (s *fakeWatchSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	return s.SyncBackground(ctx, dir)
}

func (s *fakeWatchSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	result, err := s.Sync(ctx, dir)
	if err != nil {
		return workspace.Result{}, err
	}
	result.Text = "retrieved"
	return result, nil
}

func (s *fakeWatchSyncer) setChanged(changed bool) {
	s.mu.Lock()
	s.changed = changed
	s.mu.Unlock()
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

func (s *fakeWatchSyncer) signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
