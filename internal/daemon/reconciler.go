package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/reliability"
)

const (
	defaultWatchInterval   = 30 * time.Second
	defaultWatchDebounce   = 2 * time.Second
	defaultWatchTimeout    = 5 * time.Minute
	defaultWatchBackoffMin = 5 * time.Second
	defaultWatchBackoffMax = 2 * time.Minute
	defaultWatchMaxRoots   = 64
	// defaultReconcileConcurrency 是 reconcile worker 数（M10，2026-08-03
	// 批准）：嵌入构建自身已有 provider 侧并发，daemon 级并发 2 的目的是
	// 防单个大仓分钟级构建队头阻塞其余 workspace 的变更监测，而非提吞吐。
	defaultReconcileConcurrency = 2
)

type workspaceReconciler struct {
	service   engine.Service
	detector  engine.ChangeDetector
	bgSyncer  engine.BackgroundSyncer
	inspector engine.WorkspaceInspector

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
	// workCh/workers 是有界并发 worker 池（M10）：run 只做调度分发，
	// reconcile（含同步构建，最长 watchTimeout）由 worker 执行，慢仓
	// 不再队头阻塞其余 workspace 的变更监测。
	workCh  chan watchTarget
	workers sync.WaitGroup

	interval    time.Duration
	debounce    time.Duration
	timeout     time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration
	maxRoots    int
	concurrency int

	mu     sync.Mutex
	states map[string]*watchState
}

type watchState struct {
	directoryPath     string
	providerProfileID string
	pending           bool
	// queued/running 二段置位（M10）：dueWorkspaces 分发时置 queued，
	// worker 领取时才置 running，处理完成清 running——不再批量预置
	// running 造成未处理者状态失真；且 queued||running 期间不会被再次
	// 领取（reconciler 侧去重，与引擎侧 singleflight 双保险）。
	queued               bool
	running              bool
	lastWatchAt          *time.Time
	nextWatchAt          *time.Time
	lastBackgroundSyncAt *time.Time
	lastError            string
	backoff              time.Duration
}

func newWorkspaceReconciler(service engine.Service) (*workspaceReconciler, error) {
	// 非法并发值启动期 fail-fast（M10 批准语义）：静默回退默认值属静默
	// 降级，禁止；watch off 也先校验，配置错误不因功能关闭而漏检。
	// 错误经 Server.reconcileErr 在 ListenAndServe 前置校验拒绝启动
	// (与 M5 authErr 同一 fail-closed 模式)。
	concurrency, err := reconcileConcurrency()
	if err != nil {
		return nil, err
	}
	if watchMode() == "off" {
		return nil, nil
	}
	detector, ok := service.(engine.ChangeDetector)
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reconciler := &workspaceReconciler{
		service:     service,
		detector:    detector,
		ctx:         ctx,
		cancel:      cancel,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
		workCh:      make(chan watchTarget),
		interval:    watchInterval(),
		debounce:    watchDebounce(),
		timeout:     watchTimeout(),
		backoffMin:  watchBackoffMin(),
		backoffMax:  watchBackoffMax(),
		maxRoots:    watchMaxRoots(),
		concurrency: concurrency,
		states:      make(map[string]*watchState),
	}
	if bgSyncer, ok := service.(engine.BackgroundSyncer); ok {
		reconciler.bgSyncer = bgSyncer
	}
	if inspector, ok := service.(engine.WorkspaceInspector); ok {
		reconciler.inspector = inspector
	}
	for i := 0; i < reconciler.concurrency; i++ {
		reconciler.workers.Add(1)
		go reconciler.worker()
	}
	go reconciler.run()
	return reconciler, nil
}

type watchTarget struct {
	root              string
	providerProfileID string
}

func (t watchTarget) key() string {
	return watchKey(t.root, t.providerProfileID)
}

func watchKey(root string, providerProfileID string) string {
	providerProfileID = strings.TrimSpace(providerProfileID)
	if providerProfileID == "" {
		return root
	}
	return providerProfileID + "\x00" + root
}

func (r *workspaceReconciler) Observe(dir string) {
	r.ObserveWithProvider(dir, "")
}

func (r *workspaceReconciler) ObserveWithProvider(dir string, providerProfileID string) {
	if r == nil {
		return
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	root, err := pathutil.ResolveWorkspaceRoot(dir)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	next := now.Add(r.debounce)
	providerProfileID = strings.TrimSpace(providerProfileID)
	key := watchKey(root.CanonicalPath, providerProfileID)

	r.mu.Lock()
	if _, ok := r.states[key]; !ok && len(r.states) >= r.maxRoots {
		r.mu.Unlock()
		return
	}
	state := r.states[key]
	if state == nil {
		state = &watchState{directoryPath: root.CanonicalPath, providerProfileID: providerProfileID}
		r.states[key] = state
	}
	state.pending = true
	state.nextWatchAt = &next
	state.lastError = ""
	r.mu.Unlock()

	r.signal()
}

func (r *workspaceReconciler) Decorate(status *engine.WorkspaceStatus) {
	if r == nil || status == nil {
		return
	}
	root, err := pathutil.ResolveWorkspaceRoot(status.DirectoryPath)
	if err != nil {
		return
	}
	key := watchKey(root.CanonicalPath, status.ProviderProfileID)
	r.mu.Lock()
	state := r.states[key]
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

// run 是调度循环：只负责计时与把到期 workspace 分发给 worker 池，自身
// 不执行 reconcile。关停时按 LIFO defer 先关 workCh、等 worker 全部退出
// （in-flight reconcile 的 ctx 已被 cancel，会快速返回），最后 close(done)
// ——保持 Shutdown 「取消 in-flight 并等其收尾」的既有契约。
func (r *workspaceReconciler) run() {
	defer close(r.done)
	defer r.workers.Wait()
	defer close(r.workCh)
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
		for _, target := range r.dueWorkspaces(time.Now().UTC()) {
			select {
			case r.workCh <- target:
			case <-r.ctx.Done():
				// 进程正在关停，未送达目标随内存状态一并废弃。
				return
			}
		}
	}
}

// worker 逐个领取到期 workspace 并执行 reconcile；workCh 关闭后退出。
func (r *workspaceReconciler) worker() {
	defer r.workers.Done()
	for target := range r.workCh {
		r.reconcile(target)
	}
}

func (r *workspaceReconciler) nextWait(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	wait := r.interval
	for _, state := range r.states {
		// queued 也要跳过：已在分发管道里的目标 nextWatchAt 停留在过去，
		// 不跳过会让调度循环 0 等待空转。
		if state.queued || state.running || !state.pending || state.nextWatchAt == nil {
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

func (r *workspaceReconciler) dueWorkspaces(now time.Time) []watchTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	var targets []watchTarget
	for _, state := range r.states {
		// queued||running 期间不重复分发：同一 workspace 从分发到处理完成
		// 的全程恰有一段标志为真（转换都在 r.mu 内），杜绝同轮重复领取。
		if state.queued || state.running || !state.pending || state.nextWatchAt == nil || now.Before(*state.nextWatchAt) {
			continue
		}
		state.queued = true
		targets = append(targets, watchTarget{root: state.directoryPath, providerProfileID: state.providerProfileID})
	}
	return targets
}

// claim 由 worker 在领取目标时调用：queued→running 原子转换，running
// 只在实际处理期间为真（Decorate 的 WatchRunning 不再虚报排队中的目标）。
func (r *workspaceReconciler) claim(target watchTarget) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[target.key()]
	if state == nil {
		return false
	}
	state.queued = false
	state.running = true
	return true
}

func (r *workspaceReconciler) reconcile(target watchTarget) {
	if !r.claim(target) {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()

	if r.workspaceInFlight(ctx, target.root, target.providerProfileID) {
		r.deferReconcile(target, time.Now().UTC(), r.debounce)
		return
	}

	changed, err := r.workspaceChanged(ctx, target.root, target.providerProfileID)
	if err == nil && changed {
		_, err = r.syncBackground(ctx, target.root, target.providerProfileID)
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[target.key()]
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

func (r *workspaceReconciler) workspaceInFlight(ctx context.Context, root string, providerProfileID string) bool {
	if r.inspector == nil {
		return false
	}
	status, err := r.inspector.WorkspaceStatus(ctx, engine.WorkspaceRef{
		DirectoryPath:     root,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	})
	return err == nil && status.InFlight
}

func (r *workspaceReconciler) workspaceChanged(ctx context.Context, root string, providerProfileID string) (bool, error) {
	return r.detector.WorkspaceChanged(ctx, engine.WorkspaceRef{
		DirectoryPath:     root,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	})
}

func (r *workspaceReconciler) syncBackground(ctx context.Context, root string, providerProfileID string) (engine.Result, error) {
	req := engine.SyncRequest{Workspace: engine.WorkspaceRef{
		DirectoryPath:     root,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	}}
	if r.bgSyncer != nil {
		return r.bgSyncer.SyncBackground(ctx, req)
	}
	return r.service.Sync(ctx, req)
}

func (r *workspaceReconciler) deferReconcile(target watchTarget, now time.Time, delay time.Duration) {
	if delay <= 0 {
		delay = r.debounce
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[target.key()]
	if state == nil {
		return
	}
	state.running = false
	state.pending = true
	state.lastWatchAt = &now
	state.lastError = ""
	next := now.Add(delay)
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

// reconcileConcurrency 解析 OPENACE_RECONCILE_CONCURRENCY（M10）：默认 2，
// ≥1，1 = 旧串行行为。与其余 watch env 的宽容回退不同，非法值显式报错
// （localengine options 先例：配置错误 fail-fast，不静默吞掉用户意图）。
func reconcileConcurrency() (int, error) {
	return reliability.IntEnv("OPENACE_RECONCILE_CONCURRENCY", defaultReconcileConcurrency, 1)
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
