package localengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// startupProbeTimeout 是构造服务时同步维度探测的时限。managed 模式下 wrapper
// 拉起 daemon 后最多等 10 秒（OPENACE_DAEMON_START_TIMEOUT 默认值）看它是否就绪；
// 探测按 provider 超时（默认 60 秒）等下去，daemon 就来不及开始监听，wrapper
// 只能报“daemon 未就绪”，看不到探测失败的原因。超过时限的探测转到后台继续。
const startupProbeTimeout = 5 * time.Second

// dimensionUnresolvedReason 表示维度探测还没有成功，索引子树定不下来，语义
// 检索无从进行。
const dimensionUnresolvedReason = "embedding-dimension-unresolved"

// serviceTiming 是 NewService 的时间参数，测试用它缩短等待。
type serviceTiming struct {
	// startupProbe 是构造时同步探测的时限。
	startupProbe time.Duration
	// retryWait 按探测熔断器的快照返回到下次探测前要等多久。
	retryWait func(reliability.CircuitSnapshot) time.Duration
}

// untilBackoffEnds 等到熔断器的退避截止时刻。
func untilBackoffEnds(snap reliability.CircuitSnapshot) time.Duration {
	return time.Until(snap.BackoffUntil)
}

// NewService 构造 daemon 与 MCP direct 模式使用的引擎服务，logf 接收需要写进
// 进程日志的探测失败与恢复记录。不需要探测维度（未配置 embedding、显式配置了
// 维度、缓存里已有维度且探测成功）时与 New 相同，返回 *Engine。
//
// 配置了 embedding、未设 OPENACE_EMBEDDING_DIMENSION、缓存里也没有该 provider
// 与模型的维度、启动时探测又失败时，返回维度待定的服务（deferredEngine）而不是
// 错误：改动前这种情况让 daemon 进程退出，managed wrapper 每次重连都再拉起一个
// daemon 并阻塞约 10 秒，工具错误只写“daemon 未就绪”；direct 模式每次重连阻塞
// 约 15 秒。配置错误（变量取值非法等）照常返回错误，不受影响。
func NewService(opts Options, logf func(format string, args ...any)) (engine.Service, error) {
	return newService(opts, logf, serviceTiming{startupProbe: startupProbeTimeout, retryWait: untilBackoffEnds})
}

func newService(opts Options, logf func(format string, args ...any), timing serviceTiming) (engine.Service, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.Logf == nil {
		opts.Logf = logf
	}
	if !opts.Embedding.Enabled || !opts.Embedding.DimensionAuto || opts.Embedding.Dimension > 0 {
		return New(opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timing.startupProbe)
	dim, err := probeDimension(ctx, opts.Embedding, logf)
	cancel()
	if err == nil {
		opts.Embedding.Dimension = dim
		return New(opts)
	}
	var probeErr *embedding.DimensionProbeError
	if !errors.As(err, &probeErr) {
		return nil, err
	}
	retryCtx, stop := context.WithCancel(context.Background())
	d := &deferredEngine{
		opts:        opts,
		fingerprint: engineFingerprint(opts),
		logf:        logf,
		retryWait:   timing.retryWait,
		circuit:     reliability.NewCircuit(),
		stop:        stop,
		done:        make(chan struct{}),
		lastErr:     err,
		attempts:    1,
		// 后台第一次探测紧接着开始：启动探测可能只是被 startupProbe 时限截断，
		// provider 其实正常。
		probing: true,
	}
	logf("%v; the service starts anyway: retrieval, sync and repo_map return errors until a background probe succeeds", err)
	go d.probeUntilResolved(retryCtx)
	return d, nil
}

// probeDimension 查缓存，没有则向 provider 发一次探测请求取维度。探测请求在
// provider 客户端内不重试（MaxRetries=0），重试由调用方按熔断器规则安排：
// 客户端内的重试等待期间若 ctx 到期，返回的是 ctx 错误，真实失败原因
// （如 HTTP 503）会丢失。缓存写入失败只记日志：本次已拿到维度，影响的只是
// 下次启动是否再探测一次。
func probeDimension(ctx context.Context, cfg embedding.Config, logf func(format string, args ...any)) (int, error) {
	cfg.MaxRetries = 0
	resolved, err := embedding.ResolveDimension(ctx, cfg, dimensionCacheDir())
	var warn *embedding.CacheWriteWarning
	if errors.As(err, &warn) {
		logf("%v", warn)
		err = nil
	}
	if err != nil {
		return 0, err
	}
	return resolved.Dimension, nil
}

// deferredEngine 是维度待定的服务：维度探测成功之前，索引子树名（含维度）
// 定不下来，引擎无从构造。这期间：
//   - 检索、同步、后台同步、仓库地图与变更检测返回 semantic retrieval
//     unavailable 错误，写明探测失败原因、已失败次数与下次探测时刻（或探测
//     正在进行），不返回纯词法结果，也不建索引；
//   - workspace_status 照常返回，semantic 块给出 provider 状态与原因；
//   - 后台按熔断器的退避规则重试探测：认证与额度错误等 15 分钟，限速取
//     Retry-After（没有时 30 秒），其余从 30 秒起翻倍、最多 5 分钟；
//   - 探测成功后用 New 构造真正的 Engine，之后全部调用转给它，首次检索按
//     正常流程建索引，变更检测也会触发后台同步。
//
// 配置指纹与 Engine 相同（维度未显式配置时指纹只记 auto），wrapper 的复用判定
// 不受维度是否已定影响。
type deferredEngine struct {
	opts        Options
	fingerprint string
	logf        func(format string, args ...any)
	retryWait   func(reliability.CircuitSnapshot) time.Duration
	// circuit 只用来按熔断器规则算下次探测时刻，与 provider 其余请求的退避
	// 规则一致；探测请求本身由每次新建的探测客户端发出。
	circuit *reliability.Circuit
	// ready 在探测成功后指向真正的 Engine。Engine 构造完成后才发布，读到非
	// nil 即可直接调用。
	ready atomic.Pointer[Engine]
	stop  context.CancelFunc
	done  chan struct{}

	mu sync.Mutex
	// lastErr 是最近一次探测失败的错误，attempts 是已失败的探测次数（含
	// 启动时那次），probing 表示有一次探测正在进行。
	lastErr  error
	attempts int
	probing  bool
	closed   bool
}

var (
	_ engine.Service            = (*deferredEngine)(nil)
	_ engine.WorkspaceInspector = (*deferredEngine)(nil)
	_ engine.ChangeDetector     = (*deferredEngine)(nil)
	_ engine.BackgroundSyncer   = (*deferredEngine)(nil)
	_ engine.Lifecycle          = (*deferredEngine)(nil)
	_ engine.RepoMapper         = (*deferredEngine)(nil)
	_ engine.Identifier         = (*deferredEngine)(nil)
	_ engine.ProfileIdentifier  = (*deferredEngine)(nil)
	_ engine.UsageReporter      = (*deferredEngine)(nil)
	_ engine.UsageReporter      = (*Engine)(nil)
)

// probeUntilResolved 反复探测直到成功或服务关闭。关闭时取消 ctx，在途的探测
// 请求随之中止。
func (d *deferredEngine) probeUntilResolved(ctx context.Context) {
	defer close(d.done)
	for {
		eng, dim, err := d.probeOnce(ctx)
		d.mu.Lock()
		d.probing = false
		if err == nil {
			closed := d.closed
			if !closed {
				d.ready.Store(eng)
			}
			d.mu.Unlock()
			if closed {
				_ = eng.Close(context.Background())
				return
			}
			d.logf("embedding dimension probe succeeded (dimension %d); semantic retrieval is available", dim)
			return
		}
		if ctx.Err() != nil {
			d.mu.Unlock()
			return
		}
		d.attempts++
		d.lastErr = err
		d.circuit.RecordFailure(probeCallError(err))
		snap := d.circuit.Snapshot()
		attempts := d.attempts
		d.mu.Unlock()
		d.logf("embedding dimension probe attempt %d failed: %v; next probe at %s", attempts, err, snap.BackoffUntil.UTC().Format(time.RFC3339))
		timer := time.NewTimer(d.retryWait(snap))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		d.mu.Lock()
		d.probing = true
		d.mu.Unlock()
	}
}

func (d *deferredEngine) probeOnce(ctx context.Context) (*Engine, int, error) {
	dim, err := probeDimension(ctx, d.opts.Embedding, d.logf)
	if err != nil {
		return nil, 0, err
	}
	opts := d.opts
	opts.Embedding.Dimension = dim
	eng, err := New(opts)
	return eng, dim, err
}

// probeCallError 取出探测错误里 provider 客户端归类的 CallError，熔断器按它的
// 类别选退避时长；没有归类的错误（如探测超时、响应里没有向量）按短暂故障
// 处理。
func probeCallError(err error) *reliability.CallError {
	var callErr *reliability.CallError
	if errors.As(err, &callErr) && callErr.Class != reliability.ClassBackoff {
		return callErr
	}
	return &reliability.CallError{Class: reliability.ClassTransient, Message: reliability.SanitizeMessage(err.Error())}
}

// unavailable 返回维度待定期间各操作的错误，格式与其他语义路不可用的错误
// 相同。
func (d *deferredEngine) unavailable() error {
	d.mu.Lock()
	cause, attempts, probing, closed := d.lastErr, d.attempts, d.probing, d.closed
	d.mu.Unlock()
	if closed {
		return errors.New("engine 已关闭")
	}
	detail := fmt.Sprintf("%v; %d probe attempts failed so far", cause, attempts)
	snap := d.circuit.Snapshot()
	switch {
	case probing:
		detail += "; a probe request is in progress"
	case !snap.BackoffUntil.IsZero():
		detail += "; next probe at " + snap.BackoffUntil.UTC().Format(time.RFC3339)
	default:
		detail += "; the next probe starts now"
	}
	return semanticUnavailableError([]qualityGap{{reason: dimensionUnresolvedReason, detail: errors.New(detail)}})
}

// pendingError 先按 Engine 的规则校验请求里的工作区（provider_profile_id 非空、
// 目录不存在仍按请求错误返回），再返回维度待定的错误。
func (d *deferredEngine) pendingError(ref engine.WorkspaceRef) error {
	if _, err := pendingRoot(ref); err != nil {
		return err
	}
	return d.unavailable()
}

func pendingRoot(ref engine.WorkspaceRef) (pathutil.WorkspaceRoot, error) {
	if err := rejectProfileID(ref); err != nil {
		return pathutil.WorkspaceRoot{}, err
	}
	root, err := pathutil.ResolveWorkspaceRoot(ref.DirectoryPath)
	if err != nil {
		return pathutil.WorkspaceRoot{}, engine.AsInvalidRequest(err)
	}
	return root, nil
}

// Sync 实现 engine.Service。
func (d *deferredEngine) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	if e := d.ready.Load(); e != nil {
		return e.Sync(ctx, req)
	}
	return engine.Result{}, d.pendingError(req.Workspace)
}

// Search 实现 engine.Service。
func (d *deferredEngine) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	if e := d.ready.Load(); e != nil {
		return e.Search(ctx, req)
	}
	return engine.Result{}, d.pendingError(req.Workspace)
}

// SyncBackground 实现 engine.BackgroundSyncer。
func (d *deferredEngine) SyncBackground(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	if e := d.ready.Load(); e != nil {
		return e.SyncBackground(ctx, req)
	}
	return engine.Result{}, d.pendingError(req.Workspace)
}

// RepoMap 实现 engine.RepoMapper。维度待定时没有可读的索引子树。
func (d *deferredEngine) RepoMap(ctx context.Context, req engine.RepoMapRequest) (engine.Result, error) {
	if e := d.ready.Load(); e != nil {
		return e.RepoMap(ctx, req)
	}
	return engine.Result{}, d.pendingError(req.Workspace)
}

// WorkspaceChanged 实现 engine.ChangeDetector。维度待定时返回错误：daemon 的
// 变更监测据此退避重试并在 watch_error 写明原因，维度定下来后的下一次检测
// 按正常规则触发后台同步。
func (d *deferredEngine) WorkspaceChanged(ctx context.Context, ref engine.WorkspaceRef) (bool, error) {
	if e := d.ready.Load(); e != nil {
		return e.WorkspaceChanged(ctx, ref)
	}
	return false, d.pendingError(ref)
}

// WorkspaceStatus 实现 engine.WorkspaceInspector。维度待定时返回 idle 状态：
// last_error 与检索错误相同；semantic 块的 provider_state 在探测进行中为
// candidate，等待下次探测时为 backoff 并给出 backoff_until，last_error 是最近
// 一次探测失败的原因。
func (d *deferredEngine) WorkspaceStatus(ctx context.Context, ref engine.WorkspaceRef) (engine.WorkspaceStatus, error) {
	if e := d.ready.Load(); e != nil {
		return e.WorkspaceStatus(ctx, ref)
	}
	if err := ctx.Err(); err != nil {
		return engine.WorkspaceStatus{}, err
	}
	root, err := pendingRoot(ref)
	if err != nil {
		return engine.WorkspaceStatus{}, err
	}
	status := engine.WorkspaceStatus{
		DirectoryPath: root.CanonicalPath,
		PathKind:      string(root.PathKind),
		HostOS:        root.HostOS,
		Engine:        EngineID,
		Stage:         engine.IndexStageIdle,
		LastError:     d.unavailable().Error(),
	}
	d.mu.Lock()
	cause, probing := d.lastErr, d.probing
	d.mu.Unlock()
	cfg := d.opts.Embedding
	semantic := &engine.SemanticStatus{Enabled: true, Provider: cfg.ProviderType, Model: cfg.Model, LastError: cause.Error()}
	snap := d.circuit.Snapshot()
	if !probing && snap.State == "backoff" {
		semantic.ProviderState = "backoff"
		until := snap.BackoffUntil
		semantic.BackoffUntil = &until
	} else {
		semantic.ProviderState = "candidate"
	}
	if d.opts.Rerank.Enabled {
		semantic.RerankProvider = d.opts.Rerank.Identity()
	} else if d.opts.Rerank.DisabledReason != "" {
		semantic.RerankDisabledReason = d.opts.Rerank.DisabledReason
	}
	status.Semantic = semantic
	return status, nil
}

// ListWorkspaceStatuses 实现 engine.WorkspaceInspector。维度待定时没有任何
// 工作区跟踪器。
func (d *deferredEngine) ListWorkspaceStatuses(ctx context.Context) ([]engine.WorkspaceStatus, error) {
	if e := d.ready.Load(); e != nil {
		return e.ListWorkspaceStatuses(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []engine.WorkspaceStatus{}, nil
}

// Close 实现 engine.Lifecycle：停止后台探测（取消在途请求）并等它退出，再
// 关闭已构造的 Engine。等待受调用方 ctx 限制。
func (d *deferredEngine) Close(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.stop()
	select {
	case <-d.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if e := d.ready.Load(); e != nil {
		return e.Close(ctx)
	}
	return nil
}

// EngineID 实现 engine.Identifier。
func (d *deferredEngine) EngineID() string {
	return engine.EngineLocalHybrid
}

// EngineProfileFingerprint 实现 engine.ProfileIdentifier。
func (d *deferredEngine) EngineProfileFingerprint() string {
	return d.fingerprint
}

// WorkspaceInFlight 实现 engine.WorkspaceInFlightProber：维度探测成功前没有任何
// 构建，返回 false；之后转给真正的 Engine。
func (d *deferredEngine) WorkspaceInFlight(ctx context.Context, ref engine.WorkspaceRef) (bool, error) {
	if e := d.ready.Load(); e != nil {
		return e.WorkspaceInFlight(ctx, ref)
	}
	return false, ctx.Err()
}

// EngineProfileComponents 实现 engine.ProfileComponentReporter，口径与
// EngineProfileFingerprint 相同，维度探测成功前后不变。
func (d *deferredEngine) EngineProfileComponents() map[string]string {
	return engineFingerprintComponents(d.opts)
}

// ProviderUsage 实现 engine.UsageReporter。维度尚未探测成功时还没有发出过
// 嵌入或精排请求（探测请求由临时客户端发出、不计入），返回零用量。
func (d *deferredEngine) ProviderUsage() engine.ProviderUsage {
	if e := d.ready.Load(); e != nil {
		return e.ProviderUsage()
	}
	return engine.ProviderUsage{}
}
