package reliability

import (
	"context"
	"math"
	"slices"
	"sync"
	"time"
)

const (
	// governorRateMDFactor 是一次限速后目标速率相对限速前速率的倍数。取 0.7，
	// 与 AWS SDK adaptive retry 模式（botocore CubicCalculator 的 _BETA）和
	// TCP CUBIC（RFC 9438 的 β_cubic）相同。
	governorRateMDFactor    = 0.7
	governorRateFloorTokens = 40_000
	governorPauseFallback   = 30 * time.Second
	// governorRateWindow 是速率学习统计已发送 token 的时间窗：provider 的 TPM
	// 限额按分钟计数，一个请求被接受后的一分钟内都占用额度。
	governorRateWindow = time.Minute
	// governorRateRecover 与 governorRateProbe 是恢复系数：一个 governorRateWindow
	// 内没有新的限速、且有请求因目标速率等待时，目标低于限速前速率则乘 1.5
	// （从 0.7 倍一步回到限速前速率，不超过它），已达到则乘 1.1 试探更高的额度。
	governorRateRecover = 1.5
	governorRateProbe   = 1.1
	// governorLatencySamples 是计算请求耗时中位数所保留的最近成功请求数。
	governorLatencySamples = 64
	operationBytes         = 16 << 10
)

type GovernorOutcome int

const (
	OutcomeSuccess GovernorOutcome = iota
	OutcomeRateLimited
	OutcomeOverload
	OutcomeOther
)

// IndexPermit 标识一次已登记预算的索引请求。重复归还不重复释放名额。
type IndexPermit struct {
	owner *Governor
	id    uint64
}
type indexAttempt struct {
	epoch   uint64
	tokens  int
	bytes   int64
	sampled bool
}
type throughputSample struct {
	start                                               time.Time
	usageObservedAt                                     time.Time
	underfilledFor                                      time.Duration
	admitted, outstanding, completed, success, overload int
	measuredTokens                                      int64
	measuredSuccess                                     int
	invalid                                             bool
	cohortSuccess, peakInFlight                         int
	settlementStarted                                   bool
	settleThrough                                       uint64
	settling                                            int
}

// rateEntry 是速率窗口里的一个已登记请求。
type rateEntry struct {
	at     time.Time
	id     uint64
	tokens int
}

// Governor 由全部工作区共享。window 是当前请求窗口，没有固定并发上限。
// 索引请求逐次取得许可，等待和重试不持许可；查询仅共用显式预算。
// 429 沿用独立的速率学习，窗口按完整样本组的吞吐和过载调整。
type Governor struct {
	mu                             sync.Mutex
	window, inFlight, operations   int
	inFlightBytes                  int64
	inFlightTokens                 int64
	nextID, epoch                  uint64
	active                         map[uint64]indexAttempt
	waiters                        []chan struct{}
	changed                        chan struct{}
	sample                         throughputSample
	verifiedWindow                 int
	verifiedRate, verifiedTokens   float64
	fine                           bool
	cooldown                       int
	resource                       resourceSnapshot
	resourceReason, lastAdjustment string
	sampleResources                func() resourceSnapshot
	// 速率学习：第一次收到 429 后开启，进程内不再关闭。targetTokensPerMin
	// 是学到的目标，近 governorRateWindow 内登记的 token 不超过它时才发新
	// 请求。rateMax 是最近一次限速时的速率，恢复时先回到它再往上试探。
	rateLearning       bool
	targetTokensPerMin float64
	rateMax            float64
	// rateLog 是近 governorRateWindow 内登记、已成功或仍在处理中的请求，按
	// 登记时间排列，不论是否学习速率都记录：第一次 429 到达时要据此估计
	// provider 已接受的量。以失败结束的请求移出，provider 没有接受它。
	rateLog []rateEntry
	// rateCutThrough 是最近一次减速时已登记的最大许可编号，编号不大于它的
	// 请求迟到的 429 属于同一次限速。rateCutEstimated 表示那次限速是进入
	// 学习的第一次，目标按已接受量估计，同一次限速后续的 429 继续下调估计。
	rateCutThrough   uint64
	rateCutEstimated bool
	// rateChangedAt 是目标最近一次变化的时刻；rateBound 表示此后有请求因
	// 目标速率等待过，即需求超过目标，上调才有依据。
	rateChangedAt time.Time
	rateBound     bool
	pausedUntil   time.Time
	// 进程内累计的观测计数，见 GovernorSnapshot。rateWaiting 是正在等限速
	// 暂停或目标速率的请求数，rateWaitSince 是它从 0 变为 1 的时刻。
	requests, rateLimited uint64
	rateWaiting           int
	rateWaitSince         time.Time
	rateWait              time.Duration
	latencies             []time.Duration
	latencyNext           int
	now                   func() time.Time
	sleep                 func(context.Context, time.Duration) error
}

// NewGovernor 的参数只确定初始窗口。资源状态未知时不自动扩张并报告原因，
// 过载时仍可降低窗口。
func NewGovernor(initialWindow int) *Governor {
	if initialWindow < 1 {
		initialWindow = 16
	}
	return &Governor{window: initialWindow, active: make(map[uint64]indexAttempt), changed: make(chan struct{}), now: time.Now, sleep: sleepContext, sampleResources: newResourceSampler()}
}
func (g *Governor) AcquireIndex(ctx context.Context, tokens int) (IndexPermit, error) {
	return g.AcquireIndexWithBudget(ctx, tokens, 0, nil)
}

// AcquireIndexWithBudget 同次核对资源、暂停、学习速率、窗口与显式预算。
// 锁顺序为 Governor 后 RateLimiter，等待期间两者均不持锁。
func (g *Governor) AcquireIndexWithBudget(ctx context.Context, tokens int, bytes int64, budget *RateLimiter) (IndexPermit, error) {
	if g == nil {
		return IndexPermit{}, budget.Acquire(ctx, 1, tokens)
	}
	for {
		if err := ctx.Err(); err != nil {
			g.abandonWaiter(nil)
			return IndexPermit{}, err
		}
		resources := g.sampleResources()
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.wakeWaitersLocked()
			g.mu.Unlock()
			return IndexPermit{}, err
		}
		g.resource = resources
		now := g.now()
		g.measureWindowUseLocked(now)
		g.pruneRateLogLocked(now)
		wait := g.pausedUntil.Sub(now)
		rateWait := wait > 0
		if wait <= 0 && !g.resourceFitsLocked(bytes, true) {
			wait = 100 * time.Millisecond
		}
		if wait <= 0 && g.rateLearning {
			g.raiseRateLocked(now)
			if wait = g.rateLimitWaitLocked(now, tokens); wait > 0 {
				g.rateBound, rateWait = true, true
			}
		}
		if wait > 0 {
			if rateWait {
				g.beginRateWaitLocked(now)
			}
			g.wakeWaitersLocked()
			g.mu.Unlock()
			err := g.sleep(ctx, wait)
			if rateWait {
				g.endRateWait()
			}
			if err != nil {
				return IndexPermit{}, err
			}
			continue
		}
		if g.inFlight < g.window {
			wait, err := budget.TryAcquire(ctx, 1, tokens)
			if err != nil {
				g.wakeWaitersLocked()
				g.mu.Unlock()
				return IndexPermit{}, err
			}
			if wait > 0 {
				g.wakeWaitersLocked()
				g.mu.Unlock()
				if err := budget.sleep(ctx, wait); err != nil {
					return IndexPermit{}, err
				}
				continue
			}
			// 完全空闲后的新工作不混入前次尾批与空闲时间。
			if g.inFlight == 0 && g.sample.outstanding == 0 && g.sample.admitted < g.window {
				g.sample = throughputSample{}
			}
			sampled := g.sample.admitted < g.window
			if sampled {
				if g.sample.admitted == 0 {
					g.sample.start = now
					g.sample.usageObservedAt = now
				}
				g.sample.admitted++
				g.sample.outstanding++
			}
			g.nextID++
			p := IndexPermit{owner: g, id: g.nextID}
			g.rateLog = append(g.rateLog, rateEntry{at: now, id: p.id, tokens: tokens})
			g.requests++
			g.active[p.id] = indexAttempt{epoch: g.epoch, tokens: tokens, bytes: bytes, sampled: sampled}
			g.inFlight++
			g.inFlightBytes += bytes
			g.inFlightTokens += int64(tokens)
			g.sample.peakInFlight = max(g.sample.peakInFlight, g.inFlight)
			g.mu.Unlock()
			return p, nil
		}
		ready := make(chan struct{})
		g.waiters = append(g.waiters, ready)
		g.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			g.abandonWaiter(ready)
			return IndexPermit{}, ctx.Err()
		}
	}
}

// Observe 结算一次尝试。旧样本组的迟到结果仍释放名额并计入完成量，
// 但其过载结果不重复缩窗。按许可身份删除，避免重复反馈释放他人的名额。
func (g *Governor) Observe(p IndexPermit, outcome GovernorOutcome, latency, retryAfter time.Duration) {
	if g == nil || p.owner != g {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.active[p.id]
	if !ok {
		return
	}
	now := g.now()
	g.measureWindowUseLocked(now)
	delete(g.active, p.id)
	g.inFlight--
	g.inFlightBytes -= a.bytes
	g.inFlightTokens -= int64(a.tokens)
	if g.sample.settling > 0 && p.id <= g.sample.settleThrough {
		g.sample.settling--
	}
	// 速率窗口只保留成功或仍在处理中的请求：429、连接失败与其他错误的请求
	// 没有被 provider 接受。连接失败的请求留在窗口里，第一次 429 时会把
	// provider 实际接受的量估高（本机受控实验里监听队列满被拒的连接就是
	// 这样），恢复到这个估计时再次超限。
	if outcome != OutcomeSuccess {
		g.removeRateEntryLocked(p.id)
	}
	switch outcome {
	case OutcomeSuccess:
		g.recordLatencyLocked(latency)
	case OutcomeRateLimited:
		g.rateLimited++
		g.pruneRateLogLocked(now)
		g.onRateLimitedLocked(now, p.id, retryAfter)
	}
	// 样本组决定观察何时结束，吞吐必须计算这段时间内的全部完成量。
	// 持续投放时，窗口改变前已开始的请求和组外补充请求仍会成功完成；
	// 忽略它们会在扩窗后的首组低估吞吐，即使服务完全健康也错误回退。
	if g.sample.admitted > 0 {
		if outcome == OutcomeSuccess {
			g.sample.measuredTokens += int64(a.tokens)
			g.sample.measuredSuccess++
		} else {
			g.sample.invalid = true
		}
	}
	if a.epoch == g.epoch {
		if a.sampled {
			g.sample.outstanding--
			if outcome == OutcomeSuccess {
				g.sample.cohortSuccess++
			}
		}
		g.sample.completed++
		switch outcome {
		case OutcomeSuccess:
			g.sample.success++
		case OutcomeOverload:
			g.sample.overload++
		default:
			g.sample.invalid = true
		}
		// 单个暂态错误不足以证明容量不足。至少两次失败后才调整，同组
		// 迟到结果因 epoch 已变化而不会反复将窗口减到 1。
		if g.sample.overload >= 2 && (g.sample.success == 0 || (g.sample.completed >= 10 && g.sample.overload*10 >= g.sample.completed)) {
			g.verifiedWindow, g.verifiedRate = 0, 0
			g.fine, g.cooldown = true, 3
			g.setWindowLocked(max(1, g.window/2), "overload")
		}
	}
	if g.sample.admitted == g.window && g.sample.outstanding == 0 {
		g.finishSampleLocked(now)
	}
	g.wakeWaitersLocked()
}

// 在占用数变化前记录上一段时间，避免把同一时刻逐个归还许可的顺序
// 当作持续供给不足。允许一个名额用于正常完成与补充；多个名额长期
// 空闲说明当时的吞吐没有受当前窗口限制，不能据此比较服务处理容量。
func (g *Governor) measureWindowUseLocked(now time.Time) {
	if g.sample.start.IsZero() {
		return
	}
	if elapsed := now.Sub(g.sample.usageObservedAt); elapsed > 0 && g.inFlight < max(1, g.window-1) {
		g.sample.underfilledFor += elapsed
	}
	g.sample.usageObservedAt = now
}

func (g *Governor) finishSampleLocked(now time.Time) {
	s := g.sample
	duration := now.Sub(s.start).Seconds()
	if s.settling > 0 {
		return
	}
	if s.invalid || s.overload > 0 || s.cohortSuccess != s.admitted || duration <= 0 {
		g.nextSampleLocked()
		return
	}
	// 从未填满的窗口没有提供足够的并发样本。
	if s.peakInFlight < g.window {
		g.lastAdjustment = "window-not-filled"
		g.nextSampleLocked()
		return
	}
	rate, meanTokens := float64(s.measuredTokens)/duration, float64(s.measuredTokens)/float64(s.measuredSuccess)
	if g.verifiedWindow > 0 && g.window > g.verifiedWindow && g.verifiedTokens > 0 && math.Abs(meanTokens-g.verifiedTokens)/g.verifiedTokens <= .25 {
		if rate < g.verifiedRate*1.05 {
			// 末尾已有在途请求可能改变判断时，等它们归还后再结算。
			// 冻结许可边界，之后补充的请求不延长等待；HTTP 仍正常准入，
			// 实际完成量才用于扩张，不把未完成 token 当作成功吞吐。
			if !s.settlementStarted && g.inFlight > 0 &&
				(float64(s.measuredTokens)+float64(g.inFlightTokens))/duration >= g.verifiedRate*1.05 {
				g.sample.settlementStarted = true
				g.sample.settleThrough, g.sample.settling = g.nextID, g.inFlight
				g.lastAdjustment = "throughput-inconclusive"
				return
			}
			// 先收齐可能改变结论的已有反馈，再排除供给不足造成的
			// 低吞吐。提前清空样本会漏掉迟到成功或过载；实际已测得
			// 的吞吐增益仍按原规则处理。
			if s.underfilledFor > now.Sub(s.start)/5 {
				g.lastAdjustment = "window-underutilized"
				g.nextSampleLocked()
				return
			}
			g.fine, g.cooldown = true, 3
			g.setWindowLocked(g.verifiedWindow, "no-throughput-gain")
			return
		}
	}
	g.verifiedWindow, g.verifiedRate, g.verifiedTokens = g.window, rate, meanTokens
	if g.cooldown > 0 {
		g.cooldown--
		g.nextSampleLocked()
		return
	}
	if !g.resource.known() {
		g.resourceReason = "resource-unknown"
		g.nextSampleLocked()
		return
	}
	step := max(1, g.window/2)
	if g.fine {
		step = max(1, g.window/8)
	}
	if g.window > int(^uint(0)>>1)-step {
		g.resourceReason = "integer-range"
		g.nextSampleLocked()
		return
	}
	g.setWindowLocked(g.window+step, "throughput-probe")
}
func (g *Governor) nextSampleLocked() { g.epoch++; g.sample = throughputSample{} }
func (g *Governor) setWindowLocked(window int, reason string) {
	g.window, g.lastAdjustment = window, reason
	g.nextSampleLocked()
	close(g.changed)
	g.changed = make(chan struct{})
}

// Changes 唤醒构建投放循环；收到信号后调用者重新读取窗口。
func (g *Governor) Changes() <-chan struct{} { g.mu.Lock(); defer g.mu.Unlock(); return g.changed }

// TryStartOperation 原子核对并预留逻辑批次内存，同时为至少一次 HTTP
// 尝试留下空间。先预留再启动 goroutine，避免等待批次占满全部可用预算。
func (g *Governor) TryStartOperation(requestBytes int64) (func(), bool) {
	r := g.sampleResources()
	g.mu.Lock()
	g.resource = r
	if requestBytes < 0 || requestBytes > int64(^uint64(0)>>1)-operationBytes || !g.resourceFitsLocked(requestBytes+operationBytes, false) {
		g.resourceReason = "resource-memory"
		g.mu.Unlock()
		return nil, false
	}
	g.operations++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.operations--
			close(g.changed)
			g.changed = make(chan struct{})
			g.mu.Unlock()
		})
	}, true
}

func (g *Governor) resourceFitsLocked(extra int64, needsFD bool) bool {
	limit := g.resource.availableBytes / 2
	if extra < 0 || (g.resource.memoryKnown && (g.inFlightBytes > limit || int64(g.operations) > (limit-g.inFlightBytes)/operationBytes || extra > limit-g.inFlightBytes-int64(g.operations)*operationBytes)) {
		g.resourceReason = "resource-memory"
		return false
	}
	if needsFD && g.resource.fdKnown && int64(g.inFlight+1) > g.resource.availableFD {
		g.resourceReason = "resource-file-descriptors"
		return false
	}
	g.resourceReason = ""
	if !g.resource.known() {
		g.resourceReason = "resource-unknown"
	}
	return true
}
func (g *Governor) AtRateFloor() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rateLearning && g.targetTokensPerMin <= governorRateFloorTokens
}

type GovernorSnapshot struct {
	RateLearning       bool      `json:"rate_learning,omitempty"`
	TargetTokensPerMin int       `json:"target_tokens_per_min,omitempty"`
	PausedUntil        time.Time `json:"paused_until,omitempty"`
	Window             int       `json:"window,omitempty"`
	InFlight           int       `json:"in_flight,omitempty"`
	ResourceReason     string    `json:"resource_reason,omitempty"`
	LastAdjustment     string    `json:"last_adjustment,omitempty"`
	// 进程内累计：Requests 是登记的索引请求尝试数（含重试），RateLimited 是
	// 其中收到 429 的次数，RateWait 是至少有一个索引请求在等限速暂停或学到
	// 的目标速率的墙钟时长（含正在进行的等待）。LatencyP50 是最近至多
	// governorLatencySamples 个成功请求的耗时中位数。
	Requests    uint64        `json:"requests,omitempty"`
	RateLimited uint64        `json:"rate_limited,omitempty"`
	RateWait    time.Duration `json:"rate_wait,omitempty"`
	LatencyP50  time.Duration `json:"latency_p50,omitempty"`
}

func (g *Governor) Snapshot() GovernorSnapshot {
	if g == nil {
		return GovernorSnapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rateWait := g.rateWait
	if g.rateWaiting > 0 {
		rateWait += g.now().Sub(g.rateWaitSince)
	}
	var p50 time.Duration
	if n := len(g.latencies); n > 0 {
		sorted := slices.Clone(g.latencies)
		slices.Sort(sorted)
		p50 = sorted[n/2]
	}
	return GovernorSnapshot{RateLearning: g.rateLearning, TargetTokensPerMin: int(g.targetTokensPerMin), PausedUntil: g.pausedUntil, Window: g.window, InFlight: g.inFlight, ResourceReason: g.resourceReason, LastAdjustment: g.lastAdjustment,
		Requests: g.requests, RateLimited: g.rateLimited, RateWait: rateWait, LatencyP50: p50}
}
func (g *Governor) wakeWaitersLocked() {
	free := g.window - g.inFlight
	for free > 0 && len(g.waiters) > 0 {
		close(g.waiters[0])
		g.waiters = g.waiters[1:]
		free--
	}
}
func (g *Governor) abandonWaiter(ready chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, w := range g.waiters {
		if w == ready {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			break
		}
	}
	g.wakeWaitersLocked()
}

// pruneRateLogLocked 移除登记已满 governorRateWindow 的请求：provider 的分钟
// 额度里已不再计入它们。
func (g *Governor) pruneRateLogLocked(now time.Time) {
	expired := 0
	for expired < len(g.rateLog) && now.Sub(g.rateLog[expired].at) >= governorRateWindow {
		expired++
	}
	if expired > 0 {
		g.rateLog = append(g.rateLog[:0], g.rateLog[expired:]...)
	}
}

// acceptedTokensLocked 返回近 governorRateWindow 内登记、且没有收到 429 的
// token 数，包括仍在处理中的请求：provider 接受请求时就计入了额度。
func (g *Governor) acceptedTokensLocked() int {
	total := 0
	for _, entry := range g.rateLog {
		total += entry.tokens
	}
	return total
}

// rateLimitWaitLocked 返回按学到的目标还要等多久才能发出 tokens：近
// governorRateWindow 内已登记的 token 加上本次不超过目标即可发出，否则等
// 最早的那些请求移出窗口。单个请求就超过目标时，等窗口里没有其他请求再发，
// 不会永远发不出去。按滑动窗口计数而不是按速率续充：provider 按分钟计数时，
// 刚被接受的一批请求在一分钟内一直占用额度，续充式的令牌桶在这一分钟里
// 最多会放出两倍目标的 token。
func (g *Governor) rateLimitWaitLocked(now time.Time, tokens int) time.Duration {
	used := g.acceptedTokensLocked()
	if used == 0 || float64(used+tokens) <= g.targetTokensPerMin {
		return 0
	}
	for _, entry := range g.rateLog {
		used -= entry.tokens
		if used == 0 || float64(used+tokens) <= g.targetTokensPerMin {
			return max(entry.at.Add(governorRateWindow).Sub(now), 50*time.Millisecond)
		}
	}
	return 50 * time.Millisecond
}

// raiseRateLocked 在目标上次变化后过了一个 governorRateWindow、期间没有新的
// 限速、且有请求因目标速率等待过时上调目标：低于限速前速率时乘
// governorRateRecover（不超过限速前速率），之后乘 governorRateProbe。没有请求
// 等待说明目标不是瓶颈，上调没有依据。改动前每个成功请求只把目标提高 2%，
// 学到的目标在每分钟两三个请求的低速下要几十分钟才能恢复。
func (g *Governor) raiseRateLocked(now time.Time) {
	if !g.rateBound || now.Sub(g.rateChangedAt) < governorRateWindow {
		return
	}
	if g.targetTokensPerMin < g.rateMax {
		g.targetTokensPerMin = math.Min(g.rateMax, g.targetTokensPerMin*governorRateRecover)
	} else {
		g.targetTokensPerMin *= governorRateProbe
	}
	g.rateChangedAt, g.rateBound = now, false
}

// removeRateEntryLocked 把收到 429 的请求移出速率窗口。
func (g *Governor) removeRateEntryLocked(id uint64) {
	for i, entry := range g.rateLog {
		if entry.id == id {
			g.rateLog = append(g.rateLog[:i], g.rateLog[i+1:]...)
			return
		}
	}
}

func (g *Governor) beginRateWaitLocked(now time.Time) {
	if g.rateWaiting == 0 {
		g.rateWaitSince = now
	}
	g.rateWaiting++
}

func (g *Governor) endRateWait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rateWaiting--
	if g.rateWaiting == 0 {
		g.rateWait += g.now().Sub(g.rateWaitSince)
	}
}

func (g *Governor) recordLatencyLocked(latency time.Duration) {
	if len(g.latencies) < governorLatencySamples {
		g.latencies = append(g.latencies, latency)
		return
	}
	g.latencies[g.latencyNext] = latency
	g.latencyNext = (g.latencyNext + 1) % governorLatencySamples
}

// onRateLimitedLocked 处理一次 429，被拒的请求已由 Observe 移出速率窗口。
// 一次限速只减速一次，做法与 TCP NewReno 对同一窗口内的多处丢包只减一次
// 窗口相同（RFC 6582）：
//   - 第一次 429（进入速率学习）：限速前的速率取近一分钟 provider 已接受的
//     token（含仍在处理中的请求），目标取它的 governorRateMDFactor 倍。构建
//     开始时整批并发请求同时发出，429 往往先于任何成功返回；改动前按已完成
//     请求估计，得到 0，目标直接落到下限。
//   - 减速之后才登记的请求被拒：按新目标发送仍超限，是新的一次限速。限速前
//     速率记为当前目标，目标再乘 governorRateMDFactor。
//   - 减速之前登记的请求迟到的 429：属于同一次限速，不再减速。改动前每个
//     429 都把目标减半，一批并发请求同时被拒时连续减半，几次就到下限。若
//     这次限速是进入学习的那次，按移出被拒请求后的已接受量下调估计：第一个
//     429 到达时，同批其余将被拒的请求还算在已接受量里。
//
// 目标不低于 governorRateFloorTokens。暂停取 Retry-After，没有时取
// governorPauseFallback，多个 429 只把暂停延到最晚的那个。
func (g *Governor) onRateLimitedLocked(now time.Time, id uint64, retryAfter time.Duration) {
	switch {
	case !g.rateLearning:
		g.rateLearning, g.rateCutEstimated = true, true
		g.rateMax = float64(g.acceptedTokensLocked())
		g.targetTokensPerMin = math.Max(g.rateMax*governorRateMDFactor, governorRateFloorTokens)
		g.rateCutThrough, g.rateChangedAt, g.rateBound = g.nextID, now, false
	case id > g.rateCutThrough:
		g.rateCutEstimated = false
		g.rateMax = g.targetTokensPerMin
		g.targetTokensPerMin = math.Max(g.targetTokensPerMin*governorRateMDFactor, governorRateFloorTokens)
		g.rateCutThrough, g.rateChangedAt, g.rateBound = g.nextID, now, false
	case g.rateCutEstimated:
		if accepted := float64(g.acceptedTokensLocked()); accepted < g.rateMax {
			g.rateMax = accepted
			g.targetTokensPerMin = math.Max(accepted*governorRateMDFactor, governorRateFloorTokens)
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
