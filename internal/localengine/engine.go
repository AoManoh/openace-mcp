// Package localengine 实现 local-hybrid 检索引擎的 Stage 2 形态：
// 本地扫描（复用 workspace AssetPolicy）→ 确定性 chunk → Bleve BM25
// 词法索引 → immutable revision 发布与检索。无任何远程依赖；词法
// 检索是本模式的完整能力，不是降级（阶段计划 D2）。
package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/chunk"
	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/fusion"
	"github.com/AoManoh/openace-mcp/internal/index"
	"github.com/AoManoh/openace-mcp/internal/lexical"
	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/rerank"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const (
	// EngineID 是引擎标识，进入 manifest、状态与检索结果。
	EngineID = "local-hybrid"
	// EngineVersion 随引擎行为不兼容变化而更新（Stage 4：manifest v2
	// 多 segment + journal + 跨进程锁；v1 数据可直读，升级无需重建）。
	EngineVersion = "stage4"
	// policyHash 标识当前复用的 workspace AssetPolicy 版本。
	policyHash = "workspace-assetpolicy-v1"
	// defaultTopK 是纯词法模式的候选深度。
	defaultTopK = 20
	// hybridRouteTopK 是 hybrid 模式下每路召回深度（阶段计划 D7 受测常数）。
	hybridRouteTopK = 60
	// rerankHeadLimit 是送审精排的头部候选上限（D7；再受 token 预算截断）。
	rerankHeadLimit = 50
)

// Engine 是 local-hybrid 引擎；daemon 进程内唯一实例，
// 持有全部索引句柄、构建 singleflight 与 provider 客户端（暗坑 K36）。
type Engine struct {
	profile chunk.Profile
	// storeProfile 是索引子树 profile 段：chunk profile（semantic off，
	// 与 Stage 2 逐字节一致）或追加 +emb-<hash>（阶段计划 D4）。
	storeProfile string
	// fingerprint 是配置指纹，经 ServedBy 广播用于复用判定（暗坑 K29）。
	fingerprint string

	embedCfg         embedding.Config
	embedClient      *embedding.Client
	rerankCfg        rerank.Config
	rerankClient     *rerank.Client
	retrievalDegrade DegradeMode
	rerankDegrade    DegradeMode
	// lexWeights 是词法子句权重（T10a 受测常数；默认 DefaultWeights，
	// 评测 harness 可经 Options 覆盖做权重扫描）。
	lexWeights lexical.Weights
	// fusion 是 RRF 融合参数（T10b 受测常数；默认 DefaultParams=现状
	// 等权 k=60，评测 harness 可经 Options 覆盖做融合扫描）。
	fusion fusion.Params
	// qualityStrict 开启质量严格档(方案④):语义链路任一缺口显式报错。
	qualityStrict bool
	// queryBuildWait>0 时查询等待在建索引有上界(P1 有界化);0=无界。
	queryBuildWait time.Duration
	// buildWaitSlice 是有界等待的切片粒度:每片醒来核对构建 ETA,
	// ETA 超出剩余预算且有旧 revision 可答时提前脱离等待(灰度反馈三
	// C.2:大仓重建 ETA 分钟级时等满整个预算再降级是纯延迟)。测试可
	// 拨小。
	buildWaitSlice time.Duration
	// freshnessWindow>0 时,上次成功同步距今小于窗口的查询跳过内联
	// 扫描(Stage 6 前置;新鲜度上界=窗口)。
	freshnessWindow time.Duration

	mu       sync.Mutex
	inflight map[string]*buildCall
	statuses map[string]*wsStatus
	stores   map[string]*index.Store
	handles  map[string]*revisionHandle
	// repair 记录查询期发现向量不可用的工作区，下次 sync 强制重建自愈
	// （暗坑 K25；键为 workspaceKey，构建开始时消费）。
	repair map[string]bool
	// journals 是 per-workspace 的 embedding 断点续嵌暂存区与持久化
	// 拒绝集（Stage 4 D4：取消/kill 不丢已付费批次；K35 拒绝史跨重启
	// 生效）。仅 semanticEnabled 时创建。
	journals map[string]*index.Journal
	// statCaches 每 workspace 一个扫描 stat 短路缓存(T11;构建持写锁
	// 串行,缓存自身另有锁自卫)。
	statCaches map[string]*workspace.StatCache
	// lastSyncOK 是每 workspace 最近一次成功同步完成时刻(freshness
	// 窗口判据;仅成功路径刷新)。
	lastSyncOK map[string]time.Time
	// locks 是 per-workspace 的跨进程写锁（Stage 4 D6：daemon 是唯一
	// index owner 从假设变为机制），首次构建时获取、Close 时释放。
	locks  map[string]*index.ProcessLock
	closed bool
}

// 编译期断言：Engine 满足全部通用 contract。
var (
	_ engine.Service            = (*Engine)(nil)
	_ engine.WorkspaceInspector = (*Engine)(nil)
	_ engine.ChangeDetector     = (*Engine)(nil)
	_ engine.BackgroundSyncer   = (*Engine)(nil)
	_ engine.Lifecycle          = (*Engine)(nil)
)

// New 创建 local-hybrid 引擎；opts 零值 = Stage 2 词法行为（K32）。
func New(opts Options) (*Engine, error) {
	e := &Engine{
		profile:          chunk.DefaultProfile(),
		retrievalDegrade: normalizeDegrade(opts.RetrievalDegrade),
		rerankDegrade:    normalizeDegrade(opts.RerankDegrade),
		lexWeights:       lexical.DefaultWeights(),
		inflight:         make(map[string]*buildCall),
		statuses:         make(map[string]*wsStatus),
		stores:           make(map[string]*index.Store),
		handles:          make(map[string]*revisionHandle),
		repair:           make(map[string]bool),
		journals:         make(map[string]*index.Journal),
		statCaches:       make(map[string]*workspace.StatCache),
		lastSyncOK:       make(map[string]time.Time),
		locks:            make(map[string]*index.ProcessLock),
	}
	if opts.LexicalWeights != nil {
		e.lexWeights = *opts.LexicalWeights
	}
	e.fusion = fusion.DefaultParams()
	if opts.FusionParams != nil {
		e.fusion = *opts.FusionParams
	}
	// 方案④:strict 是「完整语义质量」契约,无 embedding provider 时
	// 每查询必失败——按配置错误在构造期显式拒绝,防呆。
	if opts.QualityStrict && !opts.Embedding.Enabled {
		return nil, fmt.Errorf("%s=on 需要已配置的 embedding provider(语义质量契约无从谈起)", EnvQualityStrict)
	}
	e.qualityStrict = opts.QualityStrict
	e.queryBuildWait = opts.QueryBuildWait
	e.buildWaitSlice = 5 * time.Second
	e.freshnessWindow = opts.FreshnessWindow
	e.storeProfile = e.profile.ID + "-v" + e.profile.Version
	// 模板版本注入(M9② 单一事实源):ProfileHash 与子树后缀取同一常量。
	// 注入使既有 ProfileHash 变化——随 A2'(23aab91)同一 BREAKING 窗口
	// 落地,升级用户本就经历一次平行重建,无额外迁移成本。
	opts.Embedding.TemplateVersion = embedTemplateVersion
	e.embedCfg = opts.Embedding
	if opts.Embedding.Enabled {
		client, err := embedding.NewClient(opts.Embedding)
		if err != nil {
			return nil, err
		}
		e.embedClient = client
		// 平行 profile 子树：向量身份变化即全量重建，semantic off 路径
		// 与 Stage 2 逐字节一致（阶段计划 D4/K24）。模板版本进子树名
		// (方案①):模板变化 = 嵌入输入语义变化 = 新身份,旧子树保留
		// 供回退。
		e.storeProfile += "+emb-" + opts.Embedding.ProfileHash() + "-" + embedTemplateVersion
	}
	e.rerankCfg = opts.Rerank
	if opts.Rerank.Enabled {
		client, err := rerank.NewClient(opts.Rerank)
		if err != nil {
			return nil, err
		}
		e.rerankClient = client
	}
	e.fingerprint = opts.Fingerprint()
	return e, nil
}

// EngineProfileFingerprint 实现 engine.ProfileIdentifier（暗坑 K29）。
func (e *Engine) EngineProfileFingerprint() string {
	return e.fingerprint
}

// semanticEnabled 报告语义路是否已配置。
func (e *Engine) semanticEnabled() bool {
	return e.embedClient != nil
}

// statCacheFor 返回(必要时创建)workspace 的扫描 stat 缓存。
func (e *Engine) statCacheFor(workspaceKey string) *workspace.StatCache {
	e.mu.Lock()
	defer e.mu.Unlock()
	cache, ok := e.statCaches[workspaceKey]
	if !ok {
		cache = workspace.NewStatCache()
		e.statCaches[workspaceKey] = cache
	}
	return cache
}

// fusionParams 返回本引擎实例的 RRF 融合参数。
func (e *Engine) fusionParams() fusion.Params {
	return e.fusion
}

// markVectorRepair 登记查询期发现的向量损坏，触发下次 sync 自愈（K25）。
func (e *Engine) markVectorRepair(workspaceKey string) {
	e.mu.Lock()
	e.repair[workspaceKey] = true
	e.mu.Unlock()
}

// consumeVectorRepair 取出并清除自愈标记（构建开始时调用）。
func (e *Engine) consumeVectorRepair(workspaceKey string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.repair[workspaceKey] {
		delete(e.repair, workspaceKey)
		return true
	}
	return false
}

// vectorRepairPending 只读查询自愈标记（WorkspaceChanged 用）。
func (e *Engine) vectorRepairPending(workspaceKey string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.repair[workspaceKey]
}

// journalFor 返回（必要时打开）工作区的 embedding journal（D4）。
func (e *Engine) journalFor(workspaceKey string, store *index.Store) (*index.Journal, error) {
	e.mu.Lock()
	if journal, ok := e.journals[workspaceKey]; ok {
		e.mu.Unlock()
		return journal, nil
	}
	e.mu.Unlock()

	journal, err := index.OpenJournal(store, e.embedCfg.Dimension)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.journals[workspaceKey]; ok {
		_ = journal.Close()
		return existing, nil
	}
	e.journals[workspaceKey] = journal
	return journal, nil
}

// EngineID 实现 engine.Identifier。
func (e *Engine) EngineID() string {
	return engine.EngineLocalHybrid
}

// rejectProfileID 拒绝 legacy ACE 专属的 provider_profile_id（迁移方案 §7.3）。
func rejectProfileID(ref engine.WorkspaceRef) error {
	if strings.TrimSpace(ref.ProviderProfileID) != "" {
		// P7:请求类标记,daemon 面映射 400 而非 502。
		return engine.AsInvalidRequest(fmt.Errorf("provider_profile_id 仅适用于 legacy ACE 引擎；local-hybrid 不接受该参数（收到 %q）", ref.ProviderProfileID))
	}
	return nil
}

// resolveRoot 规范化工作区并返回 store 键身份。
func (e *Engine) resolveRoot(dir string) (pathutil.WorkspaceRoot, string, error) {
	root, err := pathutil.ResolveWorkspaceRoot(dir)
	if err != nil {
		// P7:目录不存在/非法是调用方可修复的输入错误。
		return pathutil.WorkspaceRoot{}, "", engine.AsInvalidRequest(err)
	}
	sum := sha256.Sum256([]byte(root.CanonicalPath + "\x00" + string(root.PathKind) + "\x00" + root.HostOS))
	key := sanitizeKey(filepath.Base(root.CanonicalPath)) + "-" + hex.EncodeToString(sum[:])[:12]
	return root, key, nil
}

// sanitizeKey 把目录名收敛为安全的路径片段。
func sanitizeKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "ws"
	}
	const maxLen = 32
	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}

// storeFor 返回（必要时创建）工作区的索引 Store。残留清理（staging/
// 孤儿 segment）延迟到获得跨进程写锁之后执行（D6/S15：无锁清理可能
// 误删其他进程的在飞产物），查询只读路径不触发清理。
func (e *Engine) storeFor(workspaceKey string) (*index.Store, error) {
	e.mu.Lock()
	if store, ok := e.stores[workspaceKey]; ok {
		e.mu.Unlock()
		return store, nil
	}
	e.mu.Unlock()

	cache, err := workspace.CurrentCacheSnapshot()
	if err != nil {
		return nil, err
	}
	store, err := index.NewStore(cache.Dir, cache.Namespace, workspaceKey, e.storeProfile)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.stores[workspaceKey]; ok {
		return existing, nil
	}
	e.stores[workspaceKey] = store
	return store, nil
}

// acquireWriteLock 获取（并缓存）工作区的跨进程写锁；首次获得后以
// owner 身份执行一次残留清理（D6：清理只在确认所有权后进行）。
func (e *Engine) acquireWriteLock(workspaceKey string, store *index.Store) (*index.ProcessLock, error) {
	e.mu.Lock()
	if lock, ok := e.locks[workspaceKey]; ok {
		e.mu.Unlock()
		if err := lock.Verify(); err != nil {
			return nil, err
		}
		return lock, nil
	}
	e.mu.Unlock()

	lock, err := index.AcquireLock(store)
	if err != nil {
		return nil, err
	}
	if err := store.CleanupStaging(); err != nil {
		lock.Release()
		return nil, fmt.Errorf("清理残留 staging: %w", err)
	}
	if err := store.CleanupOrphanSegments(); err != nil {
		lock.Release()
		return nil, fmt.Errorf("清理孤儿 segment: %w", err)
	}
	e.mu.Lock()
	if existing, ok := e.locks[workspaceKey]; ok {
		e.mu.Unlock()
		lock.Release()
		return existing, nil
	}
	if e.closed {
		e.mu.Unlock()
		lock.Release()
		return nil, errors.New("engine 已关闭")
	}
	e.locks[workspaceKey] = lock
	e.mu.Unlock()
	return lock, nil
}

// Sync 实现 engine.Service。
func (e *Engine) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return e.syncWorkspace(ctx, req.Workspace)
}

// SyncBackground 实现 engine.BackgroundSyncer；Stage 2 与 Sync 同路径。
func (e *Engine) SyncBackground(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return e.syncWorkspace(ctx, req.Workspace)
}

// buildCall 是同工作区并发构建的共享执行体（暗坑 K12）。
type buildCall struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	cancelled bool
	result    engine.Result
	err       error
	// startedAt 是本构建的创建时点(单调时钟)。语义收紧(review -15 (1),
	// 裁决 (i)):caller 只加入创建晚于自身到达的构建——更早的构建其扫描
	// 可能未覆盖 caller 到达前的写入,等其结束后重新循环自建/加入新一轮,
	// 保证"Sync 返回的索引包含调用前的全部写入"。
	startedAt time.Time
}

// errQueryBuildWait 标记查询等待在建索引超界(P1 有界化);构建继续
// 后台推进,调用方按 revision 有无决定降级或报错。
var errQueryBuildWait = errors.New("query wait for index build exceeded")

// syncWorkspaceForQuery 是查询路径的同步入口:配置了 QueryBuildWait 时
// 以其为上界等待在建构建;超界即脱离等待(不取消构建,phantom waiter
// 保证后续 leave 也不会取消),错误带构建进度与 env 名(可行动)。
func (e *Engine) syncWorkspaceForQuery(ctx context.Context, ref engine.WorkspaceRef) (engine.Result, error) {
	// freshness 窗口短路只属于查询期内联同步(F2,review 2026-08-06):
	// 显式 Sync 永远真实扫描——"编辑后 sync 即可见"不受窗口影响。
	if e.freshnessWindow > 0 {
		if res, ok := e.freshnessShortcut(ref); ok {
			return res, nil
		}
	}
	if e.queryBuildWait <= 0 {
		return e.syncWorkspace(ctx, ref)
	}
	// 切片等待(灰度反馈三 C.2):预算切成 buildWaitSlice 粒度,每片
	// 醒来核对构建 ETA——ETA 超出剩余预算且有旧 revision 可答时提前
	// 脱离,立即让上层以旧索引降级应答,不再等满整个预算(大仓重建
	// ETA 分钟级时,等满 90s 再降级是纯延迟)。ETA 未知(首批未完成)
	// 或无旧 revision 时行为与整段等待一致。
	deadline := time.Now().Add(e.queryBuildWait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return engine.Result{}, e.queryBuildWaitError(ref, fmt.Sprintf("%s=%s 超界,构建继续后台推进", EnvQueryBuildWait, e.queryBuildWait))
		}
		slice := e.buildWaitSlice
		if slice <= 0 || slice > remaining {
			slice = remaining
		}
		waitCtx, cancel := context.WithTimeout(ctx, slice)
		res, err := e.syncWorkspaceDetachable(waitCtx, ref, false)
		cancel()
		if err == nil || ctx.Err() != nil || waitCtx.Err() == nil {
			// 真结果 / 调用方取消 / 非超时的真实构建错误:原样返回。
			return res, err
		}
		if eta := e.buildETASeconds(ref); eta > 0 && time.Duration(eta)*time.Second > time.Until(deadline) && e.hasServableRevision(ref) {
			return engine.Result{}, e.queryBuildWaitError(ref, fmt.Sprintf("构建 ETA 超出 %s=%s 预算,提前以既有索引应答,构建继续后台推进", EnvQueryBuildWait, e.queryBuildWait))
		}
	}
}

// queryBuildWaitError 构造有界等待脱离错误(errQueryBuildWait 族,上层
// 按 revision 有无降级或报错),携带实时进度与可行动后缀。
func (e *Engine) queryBuildWaitError(ref engine.WorkspaceRef, suffix string) error {
	progress := ""
	if _, workspaceKey, err := e.resolveRoot(ref.DirectoryPath); err == nil {
		progress = e.buildProgressLabel(workspaceKey)
	}
	return fmt.Errorf("%w: index still building (%s); %s", errQueryBuildWait, progress, suffix)
}

// buildETASeconds 读取在建构建的嵌入 ETA 估算(秒);无进度返回 0。
func (e *Engine) buildETASeconds(ref engine.WorkspaceRef) int {
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return 0
	}
	e.mu.Lock()
	tracker := e.statuses[workspaceKey]
	e.mu.Unlock()
	if tracker == nil {
		return 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	_, eta := tracker.embedRateETALocked(time.Now().UTC())
	return eta
}

// hasServableRevision 报告 workspace 是否已有可应答的 revision(提前
// 脱离等待只在旧索引可答时发生;无 revision 时提前脱离只会把可行动
// 错误提前,反而缩短小仓首建等到结果的机会)。
func (e *Engine) hasServableRevision(ref engine.WorkspaceRef) bool {
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return false
	}
	handle, err := e.acquireHandle(workspaceKey)
	if err != nil {
		return false
	}
	e.releaseHandle(handle)
	return true
}

// buildProgressLabel 读取构建期进度快照(D8 可见性字段的错误文案投影)。
// 有嵌入进度时附速率与 ETA(灰度反馈 2026-08-07:等待方需要"多久能好")。
func (e *Engine) buildProgressLabel(workspaceKey string) string {
	e.mu.Lock()
	tracker := e.statuses[workspaceKey]
	e.mu.Unlock()
	if tracker == nil {
		return "stage=unknown"
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	label := fmt.Sprintf("stage=%s embedded=%d pending=%d", tracker.stage, tracker.embedDone, tracker.embedPending)
	if rate, eta := tracker.embedRateETALocked(time.Now().UTC()); rate > 0 {
		label += fmt.Sprintf(" rate=%d/min", rate)
		if eta > 0 {
			label += fmt.Sprintf(" eta=%s", (time.Duration(eta) * time.Second).Round(time.Second))
		}
	}
	return label
}

func (e *Engine) syncWorkspace(ctx context.Context, ref engine.WorkspaceRef) (engine.Result, error) {
	return e.syncWorkspaceDetachable(ctx, ref, true)
}

// freshnessShortcut 在窗口内以现役 revision 直接应答(仅查询期内联同步
// 使用;F2 后显式 Sync 不经此路)。失败过的 workspace 不短路(lastSyncOK
// 仅成功刷新),保证故障不被窗口掩盖。
func (e *Engine) freshnessShortcut(ref engine.WorkspaceRef) (engine.Result, bool) {
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return engine.Result{}, false
	}
	e.mu.Lock()
	last, ok := e.lastSyncOK[workspaceKey]
	e.mu.Unlock()
	if !ok || time.Since(last) >= e.freshnessWindow {
		return engine.Result{}, false
	}
	handle, herr := e.acquireHandle(workspaceKey)
	if herr != nil {
		return engine.Result{}, false
	}
	manifest := handle.manifest
	e.releaseHandle(handle)
	return engine.Result{
		Engine: EngineID, IndexRevision: manifest.Revision,
		FileCount: manifest.Counts.Files,
	}, true
}

// syncWorkspaceDetachable 的 cancelOnLeave=false 供有界查询等待使用:
// 等待者超时脱离时不递减 waiters(留 phantom waiter),构建不因此被
// 取消——有界化的全部意义就是"查询先走,构建继续"。
func (e *Engine) syncWorkspaceDetachable(ctx context.Context, ref engine.WorkspaceRef, cancelOnLeave bool) (engine.Result, error) {
	if err := rejectProfileID(ref); err != nil {
		return engine.Result{}, err
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return engine.Result{}, err
	}
	arrival := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return engine.Result{}, err
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return engine.Result{}, errors.New("local-hybrid 引擎已关闭")
		}
		if call, ok := e.inflight[workspaceKey]; ok {
			// 语义收紧(review -15 (1),裁决 (i)):只加入创建晚于自身
			// 到达的构建。更早的构建其扫描可能未覆盖 caller 到达前的
			// 写入——等其结束后重循环,下一轮的构建创建时点必然晚于
			// arrival(写入 < 到达 ≤ 新 startedAt < 新扫描),写入必被
			// 覆盖;同波次并发 caller 在第二轮正常 join,singleflight
			// 不放大(两轮收敛)。cancelled 构建沿用等待重试语义。
			if call.cancelled || call.startedAt.Before(arrival) {
				done := call.done
				e.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return engine.Result{}, ctx.Err()
				}
			}
			call.waiters++
			e.mu.Unlock()
			return e.waitBuild(ctx, workspaceKey, call, cancelOnLeave)
		}
		runCtx, cancel := context.WithCancel(context.Background())
		call := &buildCall{done: make(chan struct{}), cancel: cancel, waiters: 1, startedAt: time.Now()}
		e.inflight[workspaceKey] = call
		e.mu.Unlock()

		go func() {
			result, err := e.runBuild(runCtx, root, workspaceKey)
			if err == nil {
				e.mu.Lock()
				e.lastSyncOK[workspaceKey] = time.Now()
				e.mu.Unlock()
			}
			call.result, call.err = result, err
			// 对齐 legacy 语义（review S9）：先在锁内摘除表项（带身份
			// 校验）再 close(done)，消除"看到 done 但表内仍是本 call"
			// 的忙转窗口。
			e.mu.Lock()
			if e.inflight[workspaceKey] == call {
				delete(e.inflight, workspaceKey)
			}
			e.mu.Unlock()
			close(call.done)
		}()
		return e.waitBuild(ctx, workspaceKey, call, cancelOnLeave)
	}
}

// waitBuild 等待共享构建完成；cancelOnLeave 时最后一个等待者取消即中止
// 构建(K12 原语义);有界查询等待用 cancelOnLeave=false——脱离不记账
// (phantom waiter),构建永不因查询超时被取消,Close 仍兜底强停。
func (e *Engine) waitBuild(ctx context.Context, workspaceKey string, call *buildCall, cancelOnLeave bool) (engine.Result, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
	}
	// ctx 取消后复查 done（review S9，对齐 legacy）：已完成的有效结果
	// 不因取消竞态被丢弃。
	select {
	case <-call.done:
		return call.result, call.err
	default:
	}
	if !cancelOnLeave {
		return engine.Result{}, ctx.Err()
	}
	e.mu.Lock()
	// 递减前校验身份（review S9）：本 call 已被摘除时不再记账，
	// 防 waiters 负数误伤后继构建。
	if e.inflight[workspaceKey] == call {
		call.waiters--
		if call.waiters <= 0 && !call.cancelled {
			call.cancelled = true
			call.cancel()
		}
	}
	e.mu.Unlock()
	return engine.Result{}, ctx.Err()
}

// WorkspaceChanged 实现 engine.ChangeDetector：对比当前文件内容身份
// 与 active manifest 的差异；无 revision 时视为已变化。
func (e *Engine) WorkspaceChanged(ctx context.Context, ref engine.WorkspaceRef) (bool, error) {
	if err := rejectProfileID(ref); err != nil {
		return false, err
	}
	root, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return false, err
	}
	store, err := e.storeFor(workspaceKey)
	if err != nil {
		return false, err
	}
	manifest, _, err := store.ResolveUsable()
	if err != nil {
		if errors.Is(err, index.ErrNoUsableRevision) {
			return true, nil
		}
		return false, err
	}
	assets, err := workspace.FileAssetSource{Cache: e.statCacheFor(workspaceKey)}.Load(ctx, root.CanonicalPath)
	if err != nil {
		return false, err
	}
	if len(assets) != len(manifest.Files) {
		return true, nil
	}
	for _, asset := range assets {
		entry, ok := manifest.Files[asset.RelPath]
		if !ok || entry.ContentHash != asset.BlobName {
			return true, nil
		}
	}
	// 内容未变：语义缺口也是"需要同步"的信号——watcher 借此触发向量
	// 补齐/自愈；provider 退避期间如实返回未变化，防重建风暴（D10/K30）。
	if e.semanticEnabled() {
		if e.vectorRepairPending(workspaceKey) {
			return true, nil
		}
		if !manifest.SemanticComplete() && e.embedClient.CircuitSnapshot().State != "backoff" {
			return true, nil
		}
	}
	return false, nil
}

// Close 实现 engine.Lifecycle：取消并等待在飞构建（review S6——provider
// 时代构建可长达分钟级，daemon 关停不得遗留出网调用与 staging 写入）、
// 关闭全部索引句柄，拒绝后续请求。
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	var inflight []*buildCall
	for _, call := range e.inflight {
		if !call.cancelled {
			call.cancelled = true
			call.cancel()
		}
		inflight = append(inflight, call)
	}
	var firstErr error
	for revision, handle := range e.handles {
		handle.retired = true
		if handle.refs == 0 {
			if err := handle.lex.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			handle.closeContentFiles()
			delete(e.handles, revision)
		}
	}
	journals := make([]*index.Journal, 0, len(e.journals))
	for key, journal := range e.journals {
		journals = append(journals, journal)
		delete(e.journals, key)
	}
	locks := make([]*index.ProcessLock, 0, len(e.locks))
	for key, lock := range e.locks {
		locks = append(locks, lock)
		delete(e.locks, key)
	}
	e.mu.Unlock()

	// 等待构建 goroutine 退出（有界：调用方 ctx 支配等待上限）。
	for _, call := range inflight {
		select {
		case <-call.done:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return firstErr
		}
	}
	// journal 在构建退出后关闭（构建期间可能持有写句柄）。
	for _, journal := range journals {
		if err := journal.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// 写锁最后释放（此后其他进程可立即接管构建所有权）。
	for _, lock := range locks {
		lock.Release()
	}
	return firstErr
}
