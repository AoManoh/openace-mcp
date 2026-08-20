package reliability

// 吞吐治理器(架构分析 2026-08-20,任务 T6/T7):嵌入"索引车道"的自适应
// 准入闸。解决两类上游的两种过载形态——
//
//   - 配额型托管(Voyage/OpenAI 等):限的是每分钟 token/请求数,过载信号
//     是 HTTP 429。正确的自适应变量是"速率":429 乘性降速并按 Retry-After
//     暂停,干净期加性回升(AIMD;业界同型=AWS SDK adaptive retry 的客户端
//     令牌桶)。为什么不学并发:速率=并发×批大小×每批token÷单请求耗时,
//     托管服务单请求耗时波动大(实测 1.3-60s),固定并发下速率自漂。
//   - 容量型自部署(TEI/vLLM 等):没有配额、永远不发 429,过载表现为延迟
//     爬升/超时/503。正确变量是"并发窗口":短窗延迟显著偏离长窗基线即
//     判定排队形成,乘性收窗;平稳期加性上探(业界同型=Netflix
//     concurrency-limits 的延迟梯度)。
//
// 两个控制器同时在线,准入取更紧者;厂商无关性由信号驱动达成(收到 429
// 走速率通道,延迟爬升走窗口通道),不需要识别厂商。
//
// 车道边界:治理器只管索引车道(document 批嵌入)。交互查询直通不经此闸
// (embedding.Client 的车道分离负责),避免大批索引把交互查询压在队尾。
//
// 与熔断器的层次:治理器是"油门",熔断器是"保险丝"。429 先降速继续跑
// (变更前行为是熔断即全停,风暴期吞吐归零);只有速率被压到地板仍持续
// 429,调用方才把失败计入熔断器全停——烧钱保护语义保留为最后防线。

import (
	"context"
	"math"
	"sync"
	"time"
)

// 治理器常数。全部为受测起步值,来源与理由就地注明;无真实运维需求
// 不开 env(与既有退避常数同一治理原则)。
const (
	// governorRateMDFactor 是 429 时目标速率的乘性下降因子。0.5 取自
	// TCP AIMD 与 AWS adaptive retry 的公开缺省量级:一次限流即认为
	// 当前速率越界一倍以内。
	governorRateMDFactor = 0.5
	// governorRateAIFraction 是每个干净观测的加性回升步长(当前速率的
	// 比例)。2% 使 35 个连续成功约回升一倍,与分钟级配额刷新节奏匹配;
	// 过大会在配额边缘反复撞 429。
	governorRateAIFraction = 0.02
	// governorRateFloorTokens 是学习速率的地板(tokens/min)。一批默认
	// 128 条×每条约 300 token≈4 万:地板取一批的量级,保证"地板仍 429"
	// 语义上等于"连一批都发不出去",此时才值得熔断全停。
	governorRateFloorTokens = 40_000
	// governorPauseFallback 是 429 未携带 Retry-After 时的暂停时长,
	// 对齐熔断器同场景的 30s 冷却口径。
	governorPauseFallback = 30 * time.Second
	// governorWindowEWMAShort/Long 是延迟双窗口指数均值的平滑系数。
	// 短窗看当下(约最近 5 个样本),长窗看基线(约最近 50 个);两窗
	// 偏离即排队信号(Netflix Gradient2 的双窗思想,系数取保守值)。
	governorWindowEWMAShort = 0.2
	governorWindowEWMALong  = 0.02
	// governorWindowDivergence 是判定排队形成的短/长窗比值阈。1.5 倍
	// 容忍正常抖动(托管服务延迟本就波动),超过即乘性收窗。
	governorWindowDivergence = 1.5
	// governorWindowGrowEvery 是加性上探节奏:每 N 个干净样本窗口 +1。
	governorWindowGrowEvery = 8
	// governorMinSamplesForGradient 是延迟梯度生效的最小样本数,样本
	// 不足时长窗基线还不可信,不做收窗判定。
	governorMinSamplesForGradient = 10
)

// GovernorOutcome 是一次索引车道请求的观测结果分类。
type GovernorOutcome int

const (
	// OutcomeSuccess 请求成功(携带延迟样本)。
	OutcomeSuccess GovernorOutcome = iota
	// OutcomeRateLimited 收到 429(携带可选 Retry-After)。
	OutcomeRateLimited
	// OutcomeOverload 超时/503 类容量过载信号。
	OutcomeOverload
	// OutcomeOther 其余失败(认证/永久等)——不属于吞吐信号,治理器
	// 不动作,由调用方按既有分类外抛与熔断。
	OutcomeOther
)

// Governor 是索引车道的吞吐治理器。daemon 级单例,与 embedding 客户端
// 同生命周期(多 workspace 并发构建天然共享同一实例,对应同一个上游
// 配额/同一套算力)。
type Governor struct {
	mu sync.Mutex

	// —— 配额型速率控制器(T6) ——
	// rateLearning 在首个 429 前为 false:此前不限速("拉满起步",用户
	// 显式 TPM 预算除外——那由既有 RateLimiter 作为硬顶另行执行)。
	rateLearning bool
	// targetTokensPerMin 是学习到的目标速率;仅 rateLearning 后有意义。
	targetTokensPerMin float64
	// bucketTokens 是令牌桶余额;按 targetTokensPerMin 匀速补充,封顶
	// 一分钟额度(不允许长静默后爆发透支,配额型服务按滑窗计数)。
	bucketTokens float64
	lastRefill   time.Time
	// pausedUntil 是 Retry-After 指示的硬暂停(全部索引车道请求等待)。
	pausedUntil time.Time
	// recentTokens 记录最近一分钟实际成功吞吐(环形近似:窗口起点+累计),
	// 用于首个 429 时把学习速率初始化为"实测吞吐×下降因子",而不是
	// 从任意常数猜起。
	recentWindowStart  time.Time
	recentWindowTokens float64

	// —— 容量型窗口控制器(T7) ——
	window        int
	maxWindow     int
	inFlight      int
	ewmaShort     float64
	ewmaLong      float64
	latencySample int
	cleanStreak   int
	waiters       []chan struct{}

	// now/sleep 可注入,测试用确定性时钟(sleep 缺省为 ctx 感知计时器)。
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewGovernor 创建治理器。maxWindow 是并发窗口上限(用户 MaxConcurrency,
// 语义从"固定值"升级为"上限");窗口起点=上限("拉满到遇见问题",用户
// 对自己服务的配置权保留)。
func NewGovernor(maxWindow int) *Governor {
	if maxWindow < 1 {
		maxWindow = 1
	}
	return &Governor{
		window:    maxWindow,
		maxWindow: maxWindow,
		now:       time.Now,
		sleep:     sleepContext,
	}
}

// AcquireIndex 是索引车道的请求准入:依次等待 Retry-After 硬暂停、
// 速率令牌(仅学习态)与并发窗口槽位。ctx 取消时立即返回其错误。
// tokens 是本请求的保守 token 估算(bytes/4,与预算口径一致)。
func (g *Governor) AcquireIndex(ctx context.Context, tokens int) error {
	if g == nil {
		return nil
	}
	for {
		g.mu.Lock()
		now := g.now()
		// 1) Retry-After 硬暂停:上游明说了"多久以后再来",照办。
		if wait := g.pausedUntil.Sub(now); wait > 0 {
			g.mu.Unlock()
			if err := g.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		// 2) 速率令牌(仅在学习态,即见过 429 之后)。
		if g.rateLearning {
			g.refillLocked(now)
			if g.bucketTokens < float64(tokens) {
				// 缺口按当前目标速率折算成等待时长;至少 50ms 防忙转。
				deficit := float64(tokens) - g.bucketTokens
				wait := time.Duration(deficit / g.targetTokensPerMin * float64(time.Minute))
				if wait < 50*time.Millisecond {
					wait = 50 * time.Millisecond
				}
				g.mu.Unlock()
				if err := g.sleep(ctx, wait); err != nil {
					return err
				}
				continue
			}
			g.bucketTokens -= float64(tokens)
		}
		// 3) 并发窗口槽位。窗口可能被延迟梯度收小于在飞数,此时排队等
		// 释放信号(不忙轮询)。
		if g.inFlight < g.window {
			g.inFlight++
			g.mu.Unlock()
			return nil
		}
		ready := make(chan struct{})
		g.waiters = append(g.waiters, ready)
		g.mu.Unlock()
		select {
		case <-ready:
			// 纯信号唤醒:回到循环顶部全量重竞争(暂停/速率/槽位)。
			// 竞争失败会重新入队——虚假唤醒安全,无槽位预占。
		case <-ctx.Done():
			g.abandonWaiter(ready)
			return ctx.Err()
		}
	}
}

// Observe 上报一次索引车道请求的结果(与 AcquireIndex 一一配对,任何
// 出口都必须调用,否则窗口槽位泄漏)。latency 仅 OutcomeSuccess 有意义;
// retryAfter 仅 OutcomeRateLimited 有意义(0=上游未声明)。
func (g *Governor) Observe(outcome GovernorOutcome, tokens int, latency time.Duration, retryAfter time.Duration) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer func() {
		g.releaseSlotLocked()
		g.mu.Unlock()
	}()
	now := g.now()
	switch outcome {
	case OutcomeSuccess:
		g.trackThroughputLocked(now, tokens)
		if g.rateLearning {
			// 加性回升,封顶=地板与用户预算之间无上界——真实天花板由
			// 上游 429 再教育;这里只保证单调回升不失速。
			g.targetTokensPerMin += g.targetTokensPerMin * governorRateAIFraction
		}
		g.observeLatencyLocked(latency)
	case OutcomeRateLimited:
		g.onRateLimitedLocked(now, retryAfter)
	case OutcomeOverload:
		// 容量过载:乘性收窗。速率控制器不动(没有 429 就没有配额证据)。
		g.shrinkWindowLocked()
	case OutcomeOther:
		// 非吞吐信号,不动作。
	}
}

// AtRateFloor 报告速率控制器是否已压到地板——调用方以此决定是否把
// 持续 429 升级为熔断全停(保险丝语义,任务 T6 验收 G3)。
func (g *Governor) AtRateFloor() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rateLearning && g.targetTokensPerMin <= governorRateFloorTokens
}

// GovernorSnapshot 是状态面视图(进 workspace_status 语义块,可观测)。
type GovernorSnapshot struct {
	RateLearning       bool      `json:"rate_learning,omitempty"`
	TargetTokensPerMin int       `json:"target_tokens_per_min,omitempty"`
	PausedUntil        time.Time `json:"paused_until,omitempty"`
	Window             int       `json:"window,omitempty"`
	MaxWindow          int       `json:"max_window,omitempty"`
	InFlight           int       `json:"in_flight,omitempty"`
}

// Snapshot 返回当前治理状态。
func (g *Governor) Snapshot() GovernorSnapshot {
	if g == nil {
		return GovernorSnapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return GovernorSnapshot{
		RateLearning:       g.rateLearning,
		TargetTokensPerMin: int(g.targetTokensPerMin),
		PausedUntil:        g.pausedUntil,
		Window:             g.window,
		MaxWindow:          g.maxWindow,
		InFlight:           g.inFlight,
	}
}

// —— 内部:速率控制器 ——

func (g *Governor) refillLocked(now time.Time) {
	if g.lastRefill.IsZero() {
		g.lastRefill = now
		return
	}
	elapsed := now.Sub(g.lastRefill)
	if elapsed <= 0 {
		return
	}
	g.bucketTokens += g.targetTokensPerMin * elapsed.Minutes()
	// 封顶一分钟额度:配额型服务按滑窗记账,长静默攒出的"历史余额"
	// 一次性倾泻只会立刻撞 429。
	if g.bucketTokens > g.targetTokensPerMin {
		g.bucketTokens = g.targetTokensPerMin
	}
	g.lastRefill = now
}

func (g *Governor) trackThroughputLocked(now time.Time, tokens int) {
	if g.recentWindowStart.IsZero() || now.Sub(g.recentWindowStart) > time.Minute {
		g.recentWindowStart = now
		g.recentWindowTokens = 0
	}
	g.recentWindowTokens += float64(tokens)
}

func (g *Governor) onRateLimitedLocked(now time.Time, retryAfter time.Duration) {
	if !g.rateLearning {
		// 首个 429:进入学习态。初始目标=实测最近吞吐×下降因子——
		// 从证据出发,而不是从常数猜起;无实测(冷启动即 429)退到地板。
		g.rateLearning = true
		observed := g.recentWindowTokens
		if elapsed := now.Sub(g.recentWindowStart); elapsed > time.Second && elapsed < time.Minute {
			// 不足一分钟的窗口外推到分钟速率。
			observed = g.recentWindowTokens / elapsed.Minutes()
		}
		g.targetTokensPerMin = math.Max(observed*governorRateMDFactor, governorRateFloorTokens)
		g.bucketTokens = 0
		g.lastRefill = now
	} else {
		g.targetTokensPerMin = math.Max(g.targetTokensPerMin*governorRateMDFactor, governorRateFloorTokens)
		if g.bucketTokens > g.targetTokensPerMin {
			g.bucketTokens = g.targetTokensPerMin
		}
	}
	pause := retryAfter
	if pause <= 0 {
		pause = governorPauseFallback
	}
	if until := g.now().Add(pause); until.After(g.pausedUntil) {
		g.pausedUntil = until
	}
}

// —— 内部:窗口控制器 ——

func (g *Governor) observeLatencyLocked(latency time.Duration) {
	if latency <= 0 {
		return
	}
	sample := latency.Seconds()
	g.latencySample++
	if g.latencySample == 1 {
		g.ewmaShort, g.ewmaLong = sample, sample
	} else {
		g.ewmaShort += governorWindowEWMAShort * (sample - g.ewmaShort)
		g.ewmaLong += governorWindowEWMALong * (sample - g.ewmaLong)
	}
	if g.latencySample < governorMinSamplesForGradient {
		return
	}
	if g.ewmaShort > g.ewmaLong*governorWindowDivergence {
		// 排队形成:乘性收窗,并把短窗基线拉回长窗——否则同一批陈旧
		// 高延迟样本会连环触发收窗直到地板。
		g.shrinkWindowLocked()
		g.ewmaShort = g.ewmaLong
		return
	}
	g.cleanStreak++
	if g.cleanStreak >= governorWindowGrowEvery && g.window < g.maxWindow {
		g.window++
		g.cleanStreak = 0
		g.wakeWaitersLocked()
	}
}

func (g *Governor) shrinkWindowLocked() {
	g.cleanStreak = 0
	next := g.window / 2
	if next < 1 {
		next = 1
	}
	g.window = next
}

func (g *Governor) releaseSlotLocked() {
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.wakeWaitersLocked()
}

func (g *Governor) wakeWaitersLocked() {
	// 只发信号不预占槽位:被唤醒者回到 AcquireIndex 循环顶部全量重竞争,
	// 竞争失败自行重新入队。多唤醒是安全的(虚假唤醒语义),预占才会
	// 与重竞争的占位双计。
	free := g.window - g.inFlight
	for free > 0 && len(g.waiters) > 0 {
		close(g.waiters[0])
		g.waiters = g.waiters[1:]
		free--
	}
}

// abandonWaiter 把取消的等待者移出队列。不在队列=已被唤醒但按取消
// 退出——无预占语义,无需归还。
func (g *Governor) abandonWaiter(ready chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, w := range g.waiters {
		if w == ready {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			return
		}
	}
}
