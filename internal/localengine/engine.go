// Package localengine 实现 local-hybrid 检索引擎：扫描工作区（复用
// workspace 包的 AssetPolicy 文件选择与忽略规则）、按 chunk profile 切分、
// 建 Bleve BM25 词法索引，配置了 embedding provider 时再为 chunk 生成向量，
// 然后发布为不可变的 revision（一次发布的索引版本，由若干只读 segment
// 目录与一份 manifest 组成）供检索。词法路径不依赖任何凭据与网络，是
// 完整能力而不是降级：未配置 provider 时结果不带 [DEGRADED] 标记，状态
// 里也没有 semantic 字段。
package localengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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
	// EngineID 是引擎标识，写入 manifest、状态与检索结果。
	EngineID = "local-hybrid"
	// EngineVersion 写入 manifest 的 engine_version 字段，标记索引由哪一代
	// 引擎产出：当前这一代引入了多 segment 的 manifest v2、embedding journal
	// （已付费但尚未进入 revision 的向量暂存文件）与跨进程写锁。v1 manifest
	// 读取时归一为单段的 v2 视图，升级不重建索引。目前没有代码路径按该值
	// 做判断，只供人核对。
	EngineVersion = "stage4"
	// policyHash 写入 manifest 的 policy_hash 字段，标记扫描复用的 workspace
	// AssetPolicy（文件选择与忽略规则）版本。
	policyHash = "workspace-assetpolicy-v1"
	// defaultTopK 是纯词法配置下的词法召回深度。
	defaultTopK = 20
	// hybridRouteTopK 是配置了语义路（向量检索）时词法与向量两路各自的
	// 召回深度，融合前每路最多取这么多候选。深度是评测中固定的常数，
	// 不提供环境变量。
	hybridRouteTopK = 60
	// rerankHeadLimit 是送去精排（rerank：让 provider 对头部候选重新排序）
	// 的候选数上限；实际送审数还受 rerank token 预算截断，窗口外的候选按
	// 融合序跟在后面。
	rerankHeadLimit = 50
)

// Engine 是 local-hybrid 引擎，daemon 进程内只有一个实例：全部索引句柄、
// 构建去重表与 provider 客户端都挂在它上面。客户端若按任务实例化，并发
// 构建会独立计数和限速，收到 429 后各自退避。单例使构建共用索引治理状态，
// 查询与索引共用显式请求预算。启用治理器时两者的熔断器独立；关闭时共用。
type Engine struct {
	profile chunk.Profile
	// storeProfile 是索引子树目录名的 profile 段：未配置 embedding 时是
	// chunk profile（如 default-v8），配置了则追加 "+emb-<ProfileHash>-<模板
	// 版本>"。embedding 身份或模板一变就换子树全量重建，旧子树保留，
	// 不同身份的向量不会混进同一索引。
	storeProfile string
	// fingerprint 是引擎配置指纹（见 Options.Fingerprint），随 ServedBy 广播
	// 给 wrapper：wrapper 只复用指纹一致的 daemon，用户改了 provider 或降级
	// 配置后不会静默连上仍按旧配置运行的 daemon。
	fingerprint string

	embedCfg         embedding.Config
	embedClient      *embedding.Client
	rerankCfg        rerank.Config
	rerankClient     *rerank.Client
	retrievalDegrade DegradeMode
	rerankDegrade    DegradeMode
	// lexWeights 是词法各子句（内容、路径、符号等）的权重，默认
	// lexical.DefaultWeights()；评测程序 openace-bench 经 Options 覆盖做
	// 权重扫描，定值后冻结进 DefaultWeights。
	lexWeights lexical.Weights
	// fusion 是 RRF 融合参数，默认 fusion.DefaultParams()（K=20，词法权重
	// 0.15，向量权重 0.85，四语料评测后冻结）；同样只由评测程序经 Options
	// 覆盖。
	fusion fusion.Params
	// qualityStrict 为 true（OPENACE_QUALITY_STRICT=on）时，语义链路出现
	// 任一缺口都报错而不降级放行。缺口包括覆盖率不足、查询嵌入失败、
	// 配置了 rerank 但未生效、任何降级原因。
	qualityStrict bool
	// queryBuildWait 大于 0 时，查询等待在建索引最多等这么久；0 表示一直
	// 等到构建完成。
	queryBuildWait time.Duration
	// buildWaitSlice 是有界等待的分片长度：每片结束核对一次构建的嵌入 ETA，
	// ETA 超出剩余预算且已有旧 revision 可以应答时提前放弃等待，让查询
	// 用旧索引降级应答。大仓重建的 ETA 是分钟级，等满整个预算再降级只是
	// 增加延迟。测试可以调小。
	buildWaitSlice time.Duration
	// freshnessWindow 大于 0 时，上次成功同步距今不足该时长的查询跳过
	// 内联扫描，直接用现有 revision；该时长只限定跳过扫描的时间，构建失败
	// 或尚未完成时返回的旧索引仍可能更早。
	freshnessWindow time.Duration
	// lexicalFirst 控制首次构建时是否先发布只有词法索引的中间 revision，
	// 生产默认 true；只能经 Options.DisableLexicalFirst 在程序内关闭，不
	// 提供环境变量，避免再多一个需要纳入配置指纹判定的开关。
	lexicalFirst bool
	// fragmentGate 开启实验性的碎片过滤（见 fragment_gate.go）：在精排之后
	// 从结果里去掉行窗口切分产生的纯日期、纯符号碎片块。只能经 Options
	// 在程序内开启，生产没有对应环境变量，默认 false。
	fragmentGate bool

	mu       sync.Mutex
	inflight map[string]*buildCall
	statuses map[string]*wsStatus
	stores   map[string]*index.Store
	handles  map[string]*revisionHandle
	// vectorSegments 是引擎级的向量 segment 缓存，键含目录、维度与校验和。
	// active 与 previous revision 共享同一 segment 时内存里只有一份，按
	// 持有它的句柄数引用计数，归零即释放。
	vectorMu       sync.Mutex
	vectorSegments map[string]*sharedVectorIndex
	// vectorMaxResident 是常驻向量行数上限，0 表示不限（默认：能力默认
	// 不设限，资源约束由用户显式配置）。大于 0 时由
	// OPENACE_VECTOR_MEMORY_BUDGET 的字节预算按 维度×4 字节折算。
	vectorMaxResident int
	// vectorMemoryBudget 是用户配置的原始字节预算，journal 也按它封顶。
	vectorMemoryBudget int64
	// repair 记录查询期发现向量文件损坏或缺失的工作区（键为 workspaceKey），
	// 下次同步强制全量重建向量；构建开始时取走标记。
	repair map[string]bool
	// journals 是每个工作区的 embedding journal（已付费但尚未进入 revision
	// 的向量暂存文件）与持久化拒绝集：构建被取消或进程被杀时已付费批次
	// 不丢；provider 返回零向量或 NaN 的内容记入拒绝集，跨重启不再重复
	// 送审付费。只在配置了 embedding provider 时创建。
	journals map[string]*index.Journal
	// statCaches 是每个工作区一个的扫描 stat 缓存：大小与 mtime 都没变的
	// 文件跳过重新哈希。构建持写锁串行执行，缓存自身仍加锁。
	statCaches map[string]*workspace.StatCache
	// firstTouchKicks 记录正在后台做首触同步的工作区（首触：daemon 启动后
	// 某工作区的第一次查询），每个工作区最多一个后台同步 goroutine。
	firstTouchKicks sync.Map
	// lastSyncOK 是每个工作区最近一次成功同步的完成时刻，只在成功路径
	// 刷新，是新鲜度窗口与首触判定的依据。
	lastSyncOK map[string]time.Time
	// locks 是每个工作区的跨进程写锁，首次构建时获取、Close 时释放。有了
	// 锁，daemon 是唯一索引写入者就从假设变成机制：两个进程共享同一 cache
	// 子树时不会互相覆盖。
	locks  map[string]*index.ProcessLock
	closed bool
}

// 编译期断言：Engine 实现全部引擎契约接口。
var (
	_ engine.Service            = (*Engine)(nil)
	_ engine.WorkspaceInspector = (*Engine)(nil)
	_ engine.ChangeDetector     = (*Engine)(nil)
	_ engine.BackgroundSyncer   = (*Engine)(nil)
	_ engine.Lifecycle          = (*Engine)(nil)
)

// New 创建 local-hybrid 引擎。opts 为零值时是纯词法行为：不配置 embedding
// 与 rerank，降级模式为 allow。
//   - OPENACE_QUALITY_STRICT=on 但没有可用的 embedding provider：构造期报错。
//     严格档承诺完整的语义质量，没有 provider 时每次查询都会因缺口失败，
//     在启动时指出配置错误比查询期反复失败更早暴露问题。
//   - 配置了向量内存预算且 embedding 维度已知：按 维度×4 字节把预算折算
//     成常驻行数上限，最少 1 行；journal 在 journalFor 里按同一预算封顶。
//   - 配置了 embedding：创建客户端，索引子树名追加 embedding 身份与模板
//     版本，换模型、换维度或换模板都进入新子树全量重建，旧子树保留。
//   - 配置了 rerank：创建客户端。
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
		vectorSegments:   make(map[string]*sharedVectorIndex),
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
	// 严格档没有 embedding provider 时每次查询都会失败，在构造期按配置
	// 错误拒绝，不等到查询期。
	if opts.QualityStrict && !opts.Embedding.Enabled {
		return nil, fmt.Errorf("%s=on 需要已配置的 embedding provider(语义质量契约无从谈起)", EnvQualityStrict)
	}
	e.qualityStrict = opts.QualityStrict
	e.queryBuildWait = opts.QueryBuildWait
	e.buildWaitSlice = 5 * time.Second
	e.freshnessWindow = opts.FreshnessWindow
	e.lexicalFirst = !opts.DisableLexicalFirst
	e.fragmentGate = opts.FragmentGate
	e.storeProfile = e.profile.ID + "-v" + e.profile.Version
	// TemplateVersion 的唯一来源是 embedTemplateVersion：它同时参与
	// ProfileHash 与索引子树后缀，两处必须取同一常量，否则 wrapper 与
	// daemon 算出的指纹会不一致。
	opts.Embedding.TemplateVersion = embedTemplateVersion
	// 维度未显式配置时在这里探测一次（先查 cache 根目录的缓存，命中则零调用）：
	// 索引身份与向量预算都依赖真实维度。探测失败按配置错误返回，错误文本提示
	// 可设 OPENACE_EMBEDDING_DIMENSION 跳过探测；不会静默退到词法。
	if opts.Embedding.Enabled && opts.Embedding.DimensionAuto {
		cacheDir := ""
		if snap, err := workspace.CurrentCacheSnapshot(); err == nil {
			cacheDir = snap.Dir
		}
		resolved, err := embedding.ResolveDimension(context.Background(), opts.Embedding, cacheDir)
		var warn *embedding.CacheWriteWarning
		if err != nil && !errors.As(err, &warn) {
			return nil, err
		}
		opts.Embedding = resolved
	}
	e.embedCfg = opts.Embedding
	// 常驻向量默认不限。用户配置字节预算时按 维度×4 字节折算行数上限，
	// 只计向量数据本身；条目与元数据另有开销。
	// journalFor 用同一预算封顶 journal 字节数。
	if opts.VectorMemoryBudget > 0 && opts.Embedding.Dimension > 0 {
		rows := opts.VectorMemoryBudget / int64(opts.Embedding.Dimension*4)
		if rows < 1 {
			rows = 1
		}
		e.vectorMaxResident = int(rows)
		e.vectorMemoryBudget = opts.VectorMemoryBudget
	}
	if opts.Embedding.Enabled {
		client, err := embedding.NewClient(opts.Embedding)
		if err != nil {
			return nil, err
		}
		e.embedClient = client
		// 索引子树按 embedding 身份隔离：ProfileHash 覆盖 provider、端点、
		// 模型、维度、dtype 与模板版本，模板版本另以明文缀在后面便于辨认。
		// 任一项变化即换子树全量重建，旧子树保留可回退，不同身份的向量
		// 不会混进同一索引；未配置 embedding 时子树名与纯词法配置完全一致。
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

// EngineProfileFingerprint 实现 engine.ProfileIdentifier，返回配置指纹供
// daemon 复用判定。
func (e *Engine) EngineProfileFingerprint() string {
	return e.fingerprint
}

// semanticEnabled 报告是否配置了 embedding provider，即语义路是否可用。
func (e *Engine) semanticEnabled() bool {
	return e.embedClient != nil
}

// statCacheFor 返回该工作区的扫描 stat 缓存，没有则创建。
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

// fusionParams 返回本实例的 RRF 融合参数。
func (e *Engine) fusionParams() fusion.Params {
	return e.fusion
}

// markVectorRepair 登记查询期发现的向量文件损坏或缺失，下次同步对该工作区
// 强制全量重建向量。
func (e *Engine) markVectorRepair(workspaceKey string) {
	e.mu.Lock()
	e.repair[workspaceKey] = true
	e.mu.Unlock()
}

// consumeVectorRepair 取出并清除修复标记，构建开始时调用。
func (e *Engine) consumeVectorRepair(workspaceKey string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.repair[workspaceKey] {
		delete(e.repair, workspaceKey)
		return true
	}
	return false
}

// vectorRepairPending 只读查询修复标记，WorkspaceChanged 用它判断是否需要
// 同步。
func (e *Engine) vectorRepairPending(workspaceKey string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.repair[workspaceKey]
}

// journalFor 返回该工作区的 embedding journal，没有则打开。用户配置了向量
// 内存预算时 journal 按同一字节数封顶：预算只拦截新的付费写入，不丢弃
// 已有条目。两次加锁之间可能有并发调用先打开了 journal，此时关闭自己
// 打开的、返回已登记的那个。
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
	if e.vectorMemoryBudget > 0 {
		// journal 与常驻向量共用同一字节预算，在付费前拦截。journal 每条
		// 记录另有 78 字节头部，同样字节数下可容条数略少于 revision 的常驻
		// 行数，偏保守。
		journal.LimitBytes(e.vectorMemoryBudget)
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

// EngineID 实现 engine.Identifier，返回引擎标识。
func (e *Engine) EngineID() string {
	return engine.EngineLocalHybrid
}

// rejectProfileID 拒绝已删除的旧引擎 ACE 专用参数 provider_profile_id：
// local-hybrid 没有多 provider profile 路由，收到非空值即报错。
func rejectProfileID(ref engine.WorkspaceRef) error {
	if strings.TrimSpace(ref.ProviderProfileID) != "" {
		// 请求类错误：调用方可修复，daemon 返回 400 而不是 502。
		return engine.AsInvalidRequest(fmt.Errorf("provider_profile_id 仅适用于 legacy ACE 引擎；local-hybrid 不接受该参数（收到 %q）", ref.ProviderProfileID))
	}
	return nil
}

// resolveRoot 规范化工作区路径，返回规范根与索引子树的 workspaceKey：
// 目录名收敛为安全字符后，加上规范路径、路径类型与宿主 OS 的 sha256
// 前 12 位。
func (e *Engine) resolveRoot(dir string) (pathutil.WorkspaceRoot, string, error) {
	root, err := pathutil.ResolveWorkspaceRoot(dir)
	if err != nil {
		// 目录不存在或非法是调用方可修复的输入错误，daemon 返回 400。
		return pathutil.WorkspaceRoot{}, "", engine.AsInvalidRequest(err)
	}
	sum := sha256.Sum256([]byte(root.CanonicalPath + "\x00" + string(root.PathKind) + "\x00" + root.HostOS))
	key := sanitizeKey(filepath.Base(root.CanonicalPath)) + "-" + hex.EncodeToString(sum[:])[:12]
	return root, key, nil
}

// sanitizeKey 把目录名收敛为只含字母、数字、连字符与下划线的路径片段：
// 其余字符替换为下划线，空名用 ws，最长 32 字节。
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

// storeFor 返回该工作区的索引 Store，没有则创建目录结构。残留清理（未
// 完成的 staging 目录，即发布前的临时构建目录；不被任何 manifest 引用的
// segment）不在这里做，推迟到 acquireWriteLock 拿到跨进程写锁之后：没有
// 锁就清理，可能删掉另一个进程正在构建的产物。查询只读路径不触发清理。
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

// acquireWriteLock 获取并缓存该工作区的跨进程写锁。已缓存的锁先复验仍
// 归自己持有：心跳超时后锁可能被其他进程接管。首次拿到锁后以所有者身份
// 清理残留 staging 与孤儿 segment，清理失败释放锁并报错。登记时发现引擎
// 已关闭或已有并发调用登记了锁，释放自己刚拿到的这把。
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

// Sync 实现 engine.Service：同步工作区索引，阻塞到构建完成。
func (e *Engine) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return e.syncWorkspace(ctx, req.Workspace)
}

// SyncBackground 实现 engine.BackgroundSyncer，与 Sync 走同一路径。
func (e *Engine) SyncBackground(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return e.syncWorkspace(ctx, req.Workspace)
}

// buildCall 是同一工作区并发构建请求共享的执行体：同时只有一个构建在跑，
// 后到的调用加入等待并共享结果，多个 client 同时 sync 同一仓库不会把 CPU
// 与磁盘开销放大 N 倍。
type buildCall struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	cancelled bool
	result    engine.Result
	err       error
	// startedAt 是本构建的创建时刻。调用方只加入创建时刻晚于自身到达时刻
	// 的构建：更早创建的构建其扫描可能没有覆盖调用方到达前的写入，等它
	// 结束后重新循环，自建或加入新一轮，保证 Sync 返回的索引包含调用前
	// 的全部写入。
	startedAt time.Time
}

// errQueryBuildWait 表示查询等待在建索引超过了 OPENACE_QUERY_BUILD_WAIT
// 上界。构建继续在后台推进；调用方有旧 revision 时降级应答（原因
// index-building），没有则报错。
var errQueryBuildWait = errors.New("query wait for index build exceeded")

// errFirstTouchRefresh 表示走了首触快路径：磁盘上的 revision 立即可服务，
// 真实同步已在后台进行；上层以 index-refreshing 原因用旧索引应答。
var errFirstTouchRefresh = errors.New("first touch refresh in background")

// firstTouchFastPath 判定是否走首触快路径，需要时启动后台同步。返回
// (errFirstTouchRefresh, true) 表示走快路径。条件：本 daemon 生命周期内该
// 工作区还没有成功同步过，且磁盘上已有可服务的 revision。后台同步
// goroutine 每个工作区最多一个（firstTouchKicks 登记），完成后清除登记；
// 同步失败时下一次查询会再启动一次，与内联同步失败后的重试一致，失败
// 原因经状态的 last_error 可见，不会一直静默用旧索引。
func (e *Engine) firstTouchFastPath(ref engine.WorkspaceRef) (error, bool) {
	_, workspaceKey, err := e.resolveRoot(ref.DirectoryPath)
	if err != nil {
		return nil, false
	}
	e.mu.Lock()
	_, synced := e.lastSyncOK[workspaceKey]
	e.mu.Unlock()
	if synced || !e.hasServableRevision(ref) {
		return nil, false
	}
	if _, running := e.firstTouchKicks.LoadOrStore(workspaceKey, struct{}{}); !running {
		go func() {
			defer e.firstTouchKicks.Delete(workspaceKey)
			defer func() {
				// 后台 goroutine 里的 panic（如引擎关闭期间的竞态）若不恢复会
				// 让整个 daemon 进程退出，这里只恢复 panic；同步失败本身不在
				// 此处理，由状态与下一次查询重新触发。
				_ = recover()
			}()
			_, _ = e.syncWorkspaceDetachable(context.Background(), ref, false)
		}()
	}
	return errFirstTouchRefresh, true
}

// syncWorkspaceForQuery 是查询路径的同步入口，依次尝试：
//   - 新鲜度窗口内：直接用现有 revision 应答，不扫描。
//   - 首触快路径（降级模式不是 deny 且未开严格档）：返回
//     errFirstTouchRefresh，上层用磁盘上的旧 revision 应答，同步在后台
//     进行。
//   - OPENACE_QUERY_BUILD_WAIT 为 0：等到构建完成。
//   - 配置了上界：分片等待，超界后返回 errQueryBuildWait。构建不被取消，
//     错误文本带构建进度与环境变量名。
func (e *Engine) syncWorkspaceForQuery(ctx context.Context, ref engine.WorkspaceRef) (engine.Result, error) {
	// 新鲜度窗口只作用于查询期的内联同步；显式 Sync 每次都真实扫描，
	// 编辑后 sync 立即可见，不受窗口影响。
	if e.freshnessWindow > 0 {
		if res, ok := e.freshnessShortcut(ref); ok {
			return res, nil
		}
	}
	// 首触快路径。进程重启后 lastSyncOK 为空，而磁盘上的 revision 依旧可
	// 服务；改动前首次查询要内联全量扫描，大仓实测阻塞 43.6 秒（gradle
	// 仓库 2.9 万文件）之后才用本就在盘上的 revision 应答。现在：该工作区
	// 尚无成功同步且有可服务 revision 时，立即返回 errFirstTouchRefresh
	// 让上层用旧 revision 应答（原因 index-refreshing，进 [DEGRADED] 横幅），
	// 同时后台启动一次真实同步；同步成功后 lastSyncOK 置位，本路径不再
	// 触发。不走的情况：显式 Sync；没有任何 revision 的工作区；降级模式
	// deny（正确性优先）；严格档（它会把任何降级原因判为违规，走快路径
	// 只会必然报错）。
	if e.retrievalDegrade != DegradeDeny && !e.qualityStrict {
		if err, ok := e.firstTouchFastPath(ref); ok {
			return engine.Result{}, err
		}
	}
	if e.queryBuildWait <= 0 {
		return e.syncWorkspace(ctx, ref)
	}
	// 分片等待：把预算切成 buildWaitSlice 长的片，每片结束核对构建 ETA。
	// ETA 超出剩余预算且有旧 revision 可答时立即返回，让上层用旧索引降级
	// 应答，不再等满整个预算。ETA 未知（首批尚未完成）或没有旧 revision
	// 时与整段等待行为一致。
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
		// waitCtx.Err() 必须在 cancel() 之前读取：cancel 之后它已非 nil，
		// 原写法把非超时的真实构建错误也当成超时进入重试循环，直到预算
		// 耗尽后用 index-building 的措辞包装，路径不存在这类错误要等 40 秒
		// 才返回，而且看不到真实原因。
		waitExpired := waitCtx.Err() != nil
		cancel()
		if err == nil || ctx.Err() != nil || !waitExpired {
			// 拿到结果、调用方取消、或非超时的真实构建错误：原样返回。
			return res, err
		}
		if eta := e.buildETASeconds(ref); eta > 0 && time.Duration(eta)*time.Second > time.Until(deadline) && e.hasServableRevision(ref) {
			return engine.Result{}, e.queryBuildWaitError(ref, fmt.Sprintf("构建 ETA 超出 %s=%s 预算,提前以既有索引应答,构建继续后台推进", EnvQueryBuildWait, e.queryBuildWait))
		}
	}
}

// queryBuildWaitError 构造 errQueryBuildWait 族错误：带构建进度快照与
// suffix 里的处置提示；上层按有无 revision 决定降级或报错。
func (e *Engine) queryBuildWaitError(ref engine.WorkspaceRef, suffix string) error {
	progress := ""
	if _, workspaceKey, err := e.resolveRoot(ref.DirectoryPath); err == nil {
		progress = e.buildProgressLabel(workspaceKey)
	}
	return fmt.Errorf("%w: index still building (%s); %s", errQueryBuildWait, progress, suffix)
}

// buildETASeconds 读取在建构建的嵌入 ETA 估算（秒），没有进度信息返回 0。
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

// hasServableRevision 报告该工作区是否有可以应答的 revision。提前放弃等待
// 只在有旧索引可答时发生：没有 revision 时提前放弃只是把错误提前返回，
// 小仓首建本来可能在预算内等到结果。
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

// buildProgressLabel 把构建进度快照格式化为错误文本里的一段：阶段、已嵌
// 与待嵌 chunk 数；有嵌入速率时附速率与 ETA，等待方能看出构建是否在
// 推进以及大约多久完成。
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

// freshnessShortcut 在新鲜度窗口内用当前 revision 直接应答，只供查询期的
// 内联同步使用，显式 Sync 不经此路。判断依据是 lastSyncOK 中最近一次
// 成功同步的时间。失败不会刷新或删除该时间，窗口内仍可能返回旧索引。
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

// syncWorkspaceDetachable 是全部同步入口的共同实现。cancelOnLeave=false
// 供有界查询等待使用：等待者超时离开时不递减 waiters，构建不会因为查询
// 放弃等待而被取消；有界等待的意义就是查询先返回、构建继续。
func (e *Engine) syncWorkspaceDetachable(ctx context.Context, ref engine.WorkspaceRef, cancelOnLeave bool) (engine.Result, error) {
	if err := rejectProfileID(ref); err != nil {
		return engine.Result{}, err
	}
	// 路径不存在或不是目录的请求在入口就按请求类错误返回。改动前这类
	// 请求进入构建失败加有界等待，用户等 40 秒后得到 index-building 措辞
	// 的 502，真实原因藏在 stage=failed 里。os.Stat 跟随符号链接，链接指向
	// 不存在的目标同样按不存在处理；没有读权限的目录 stat 能通过，交给
	// 扫描阶段按既有错误处理。
	if info, statErr := os.Stat(ref.DirectoryPath); statErr != nil {
		return engine.Result{}, engine.AsInvalidRequest(fmt.Errorf("workspace directory does not exist: %s", ref.DirectoryPath))
	} else if !info.IsDir() {
		return engine.Result{}, engine.AsInvalidRequest(fmt.Errorf("workspace path is not a directory: %s", ref.DirectoryPath))
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
			// 只加入创建时刻晚于自身到达时刻的构建。更早的构建其扫描可能
			// 没有覆盖调用方到达前的写入：等它结束后重新循环，下一轮构建
			// 的创建时刻必然晚于 arrival（写入早于到达，到达早于新构建，
			// 新构建早于新扫描），写入一定被覆盖；同一波并发调用方在第二
			// 轮正常加入，两轮即收敛，不会无限放大构建次数。已取消的构建
			// 同样等它结束再重试。
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
			// 先在锁内摘除表项（核对确是本 call）再 close(done)：若先
			// close，等待者会看到 done 已关闭而表里仍是本 call，进入忙转。
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

// waitBuild 等待共享构建完成。cancelOnLeave=true 时最后一个等待者取消即
// 中止构建；有界查询等待传 false，离开时不记账，构建不会因为查询超时被
// 取消，Close 仍会强停它。
func (e *Engine) waitBuild(ctx context.Context, workspaceKey string, call *buildCall, cancelOnLeave bool) (engine.Result, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
	}
	// ctx 取消后再查一次 done：构建恰在此刻完成的有效结果不因取消竞态
	// 被丢弃。
	select {
	case <-call.done:
		return call.result, call.err
	default:
	}
	if !cancelOnLeave {
		return engine.Result{}, ctx.Err()
	}
	e.mu.Lock()
	// 递减前核对表里仍是本 call：本 call 已被摘除时不再记账，否则 waiters
	// 变成负数会误取消后续构建。
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

// WorkspaceChanged 实现 engine.ChangeDetector：对比当前文件集与内容身份和
// active manifest 的差异，没有 revision 视为已变化。内容未变时语义缺口也
// 算需要同步：向量待修复，或覆盖不完整且 provider 熔断器不在退避期；
// 退避期内返回未变化，否则 watcher 每个周期都会触发一次注定失败的重建。
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
	// 内容未变：语义缺口也是需要同步的信号，watcher 借此触发向量补齐或
	// 修复；provider 退避期间返回未变化，避免每个周期重复触发重建。
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

// Close 实现 engine.Lifecycle：先标记关闭、取消在途构建，再关闭没有引用的
// 索引句柄。仍被查询持有的句柄标记退役，由 releaseHandle 在引用归零时
// 关闭。随后等待构建 goroutine 退出，避免它继续写 journal 时关闭写句柄。
// 等待受调用方 ctx 限制，超时即返回；构建全部退出后关闭 journal，最后
// 释放跨进程写锁。
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
			handle.releaseVectorIndexes()
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

	// 等待构建 goroutine 退出，等待上限由调用方 ctx 决定；超时即返回，
	// 剩余步骤不再执行。
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
	// journal 在构建退出后再关闭：构建期间可能正持有它的写句柄。
	for _, journal := range journals {
		if err := journal.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// 写锁最后释放，此后其他进程可以立即接管构建所有权。
	for _, lock := range locks {
		lock.Release()
	}
	return firstErr
}
