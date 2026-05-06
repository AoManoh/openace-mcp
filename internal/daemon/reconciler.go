package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const (
	defaultWatchInterval   = 30 * time.Second
	defaultWatchDebounce   = 2 * time.Second
	defaultWatchTimeout    = 5 * time.Minute
	defaultWatchBackoffMin = 5 * time.Second
	defaultWatchBackoffMax = 2 * time.Minute
	defaultWatchMaxRoots   = 64
)

type workspaceChangeDetector interface {
	WorkspaceChanged(context.Context, string) (bool, error)
}

type backgroundSyncer interface {
	SyncBackground(context.Context, string) (workspace.Result, error)
}

type workspaceReconciler struct {
	syncer   Syncer
	detector workspaceChangeDetector
	bgSyncer backgroundSyncer

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}

	interval   time.Duration
	debounce   time.Duration
	timeout    time.Duration
	backoffMin time.Duration
	backoffMax time.Duration
	maxRoots   int

	mu     sync.Mutex
	states map[string]*watchState
}

type watchState struct {
	directoryPath        string
	pending              bool
	running              bool
	lastWatchAt          *time.Time
	nextWatchAt          *time.Time
	lastBackgroundSyncAt *time.Time
	lastError            string
	backoff              time.Duration
}

func newWorkspaceReconciler(syncer Syncer) *workspaceReconciler {
	if watchMode() == "off" {
		return nil
	}
	detector, ok := syncer.(workspaceChangeDetector)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reconciler := &workspaceReconciler{
		syncer:     syncer,
		detector:   detector,
		ctx:        ctx,
		cancel:     cancel,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		interval:   watchInterval(),
		debounce:   watchDebounce(),
		timeout:    watchTimeout(),
		backoffMin: watchBackoffMin(),
		backoffMax: watchBackoffMax(),
		maxRoots:   watchMaxRoots(),
		states:     make(map[string]*watchState),
	}
	if bgSyncer, ok := syncer.(backgroundSyncer); ok {
		reconciler.bgSyncer = bgSyncer
	}
	go reconciler.run()
	return reconciler
}

func (r *workspaceReconciler) Observe(dir string) {
	if r == nil {
		return
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	next := now.Add(r.debounce)

	r.mu.Lock()
	if _, ok := r.states[root]; !ok && len(r.states) >= r.maxRoots {
		r.mu.Unlock()
		return
	}
	state := r.states[root]
	if state == nil {
		state = &watchState{directoryPath: root}
		r.states[root] = state
	}
	state.pending = true
	state.nextWatchAt = &next
	state.lastError = ""
	r.mu.Unlock()

	r.signal()
}

func (r *workspaceReconciler) Decorate(status *workspace.WorkspaceStatus) {
	if r == nil || status == nil {
		return
	}
	root, err := filepath.Abs(status.DirectoryPath)
	if err != nil {
		return
	}
	r.mu.Lock()
	state := r.states[root]
	if state == nil {
		r.mu.Unlock()
		return
	}
	status.WatchEnabled = true
	status.WatchScheduled = state.pending && state.nextWatchAt != nil
	status.WatchRunning = state.running
	status.WatchError = state.lastError
	status.LastWatchAt = cloneDaemonTime(state.lastWatchAt)
	status.NextWatchAt = cloneDaemonTime(state.nextWatchAt)
	status.LastBackgroundSyncAt = cloneDaemonTime(state.lastBackgroundSyncAt)
	r.mu.Unlock()
}

func (r *workspaceReconciler) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.cancel()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *workspaceReconciler) run() {
	defer close(r.done)
	for {
		timer := time.NewTimer(r.nextWait(time.Now().UTC()))
		select {
		case <-r.ctx.Done():
			stopTimer(timer)
			return
		case <-r.wake:
			stopTimer(timer)
		case <-timer.C:
		}
		for _, root := range r.dueWorkspaces(time.Now().UTC()) {
			r.reconcile(root)
		}
	}
}

func (r *workspaceReconciler) nextWait(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	wait := r.interval
	for _, state := range r.states {
		if state.running || !state.pending || state.nextWatchAt == nil {
			continue
		}
		until := state.nextWatchAt.Sub(now)
		if until <= 0 {
			return 0
		}
		if until < wait {
			wait = until
		}
	}
	if wait <= 0 {
		return r.interval
	}
	return wait
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (r *workspaceReconciler) dueWorkspaces(now time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var roots []string
	for root, state := range r.states {
		if state.running || !state.pending || state.nextWatchAt == nil || now.Before(*state.nextWatchAt) {
			continue
		}
		state.running = true
		roots = append(roots, root)
	}
	return roots
}

func (r *workspaceReconciler) reconcile(root string) {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()

	changed, err := r.detector.WorkspaceChanged(ctx, root)
	if err == nil && changed {
		if r.bgSyncer != nil {
			_, err = r.bgSyncer.SyncBackground(ctx, root)
		} else {
			_, err = r.syncer.Sync(ctx, root)
		}
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[root]
	if state == nil {
		return
	}
	state.running = false
	state.lastWatchAt = &now
	if err != nil {
		state.pending = true
		state.lastError = err.Error()
		state.backoff = nextBackoff(state.backoff, r.backoffMin, r.backoffMax)
		next := now.Add(state.backoff)
		state.nextWatchAt = &next
		return
	}
	state.lastError = ""
	state.backoff = 0
	if changed {
		state.lastBackgroundSyncAt = &now
	}
	state.pending = true
	next := now.Add(r.interval)
	state.nextWatchAt = &next
}

func (r *workspaceReconciler) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func nextBackoff(current time.Duration, min time.Duration, max time.Duration) time.Duration {
	if min <= 0 {
		min = defaultWatchBackoffMin
	}
	if max < min {
		max = min
	}
	if current <= 0 {
		return min
	}
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func watchMode() string {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("OPENACE_WATCH_MODE"))) {
	case "", "seen", "on", "true", "1":
		return "seen"
	case "off", "false", "0":
		return "off"
	default:
		return "seen"
	}
}

func watchInterval() time.Duration {
	return durationEnv("OPENACE_WATCH_INTERVAL", defaultWatchInterval)
}

func watchDebounce() time.Duration {
	return durationEnv("OPENACE_WATCH_DEBOUNCE", defaultWatchDebounce)
}

func watchTimeout() time.Duration {
	return durationEnv("OPENACE_WATCH_TIMEOUT", defaultWatchTimeout)
}

func watchBackoffMin() time.Duration {
	return durationEnv("OPENACE_WATCH_BACKOFF_MIN", defaultWatchBackoffMin)
}

func watchBackoffMax() time.Duration {
	return durationEnv("OPENACE_WATCH_BACKOFF_MAX", defaultWatchBackoffMax)
}

func watchMaxRoots() int {
	value := strings.TrimSpace(os.Getenv("OPENACE_WATCH_MAX_WORKSPACES"))
	if value == "" {
		return defaultWatchMaxRoots
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultWatchMaxRoots
	}
	return parsed
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func cloneDaemonTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}
