package reliability

import (
	"context"
	"math"
	"sync"
	"time"
)

const (
	governorRateMDFactor    = 0.5
	governorRateAIFraction  = 0.02
	governorRateFloorTokens = 40_000
	governorPauseFallback   = 30 * time.Second
	operationBytes          = 16 << 10
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
	admitted, outstanding, completed, success, overload int
	measuredTokens                                      int64
	measuredSuccess                                     int
	invalid                                             bool
}

// Governor 由全部工作区共享。window 是当前请求窗口，没有固定并发上限。
// 索引请求逐次取得许可，等待和重试不持许可；查询仅共用显式预算。
// 429 沿用独立的速率学习，窗口按完整样本组的吞吐和过载调整。
type Governor struct {
	mu                                         sync.Mutex
	window, inFlight, operations               int
	inFlightBytes                              int64
	nextID, epoch                              uint64
	active                                     map[uint64]indexAttempt
	waiters                                    []chan struct{}
	changed                                    chan struct{}
	sample                                     throughputSample
	verifiedWindow                             int
	verifiedRate, verifiedTokens               float64
	fine                                       bool
	cooldown                                   int
	resource                                   resourceSnapshot
	resourceReason, lastAdjustment             string
	sampleResources                            func() resourceSnapshot
	rateLearning                               bool
	targetTokensPerMin, bucketTokens           float64
	lastRefill, pausedUntil, recentWindowStart time.Time
	recentWindowTokens                         float64
	now                                        func() time.Time
	sleep                                      func(context.Context, time.Duration) error
}

// NewGovernor 的参数只确定初始窗口。资源状态未知时维持当前窗口并报告原因。
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
		wait := g.pausedUntil.Sub(now)
		if wait <= 0 && !g.resourceFitsLocked(bytes, true) {
			wait = 100 * time.Millisecond
		}
		if wait <= 0 && g.rateLearning {
			g.refillLocked(now)
			need := math.Min(float64(tokens), g.targetTokensPerMin)
			if g.bucketTokens < need {
				wait = time.Duration((need - g.bucketTokens) / g.targetTokensPerMin * float64(time.Minute))
				if wait < 50*time.Millisecond {
					wait = 50 * time.Millisecond
				}
			}
		}
		if wait > 0 {
			g.wakeWaitersLocked()
			g.mu.Unlock()
			if err := g.sleep(ctx, wait); err != nil {
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
			if g.rateLearning {
				g.bucketTokens -= float64(tokens)
			}
			// 完全空闲后的新工作不混入前次尾批与空闲时间。
			if g.inFlight == 0 && g.sample.outstanding == 0 && g.sample.admitted < g.window {
				g.sample = throughputSample{}
			}
			sampled := g.sample.admitted < g.window
			if sampled {
				if g.sample.admitted == 0 {
					g.sample.start = now
				}
				g.sample.admitted++
				g.sample.outstanding++
			}
			g.nextID++
			p := IndexPermit{owner: g, id: g.nextID}
			g.active[p.id] = indexAttempt{epoch: g.epoch, tokens: tokens, bytes: bytes, sampled: sampled}
			g.inFlight++
			g.inFlightBytes += bytes
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

// Observe 结算一次尝试。旧样本组的迟到结果仍释放名额并参与速率统计，
// 但不再次改变当前窗口。按许可身份删除，避免重复反馈释放他人的名额。
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
	delete(g.active, p.id)
	g.inFlight--
	g.inFlightBytes -= a.bytes
	now := g.now()
	switch outcome {
	case OutcomeSuccess:
		g.trackThroughputLocked(now, a.tokens)
		if g.rateLearning {
			g.targetTokensPerMin *= 1 + governorRateAIFraction
		}
	case OutcomeRateLimited:
		g.onRateLimitedLocked(now, retryAfter)
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
	if a.epoch == g.epoch && a.sampled {
		g.sample.outstanding--
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
		} else if g.sample.admitted == g.window && g.sample.outstanding == 0 {
			g.finishSampleLocked(now)
		}
	}
	g.wakeWaitersLocked()
}

func (g *Governor) finishSampleLocked(now time.Time) {
	s := g.sample
	duration := now.Sub(s.start).Seconds()
	if s.invalid || s.overload > 0 || s.success != s.admitted || duration <= 0 {
		g.nextSampleLocked()
		return
	}
	rate, meanTokens := float64(s.measuredTokens)/duration, float64(s.measuredTokens)/float64(s.measuredSuccess)
	if g.verifiedWindow > 0 && g.window > g.verifiedWindow && g.verifiedTokens > 0 && math.Abs(meanTokens-g.verifiedTokens)/g.verifiedTokens <= .25 {
		if rate < g.verifiedRate*1.05 {
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
}

func (g *Governor) Snapshot() GovernorSnapshot {
	if g == nil {
		return GovernorSnapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return GovernorSnapshot{RateLearning: g.rateLearning, TargetTokensPerMin: int(g.targetTokensPerMin), PausedUntil: g.pausedUntil, Window: g.window, InFlight: g.inFlight, ResourceReason: g.resourceReason, LastAdjustment: g.lastAdjustment}
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
	// 余额上限为一分钟的额度。有配额的服务按滑动窗口计数，长时间空闲攒
	// 出的余额一次发出去会立刻收到 429。
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
		// 第一个 429：进入学习态。初始目标取实测最近吞吐乘下降因子，从
		// 实测出发而不是从常数猜起。没有实测数据（刚启动就收到 429）时
		// 落到下限 governorRateFloorTokens。
		g.rateLearning = true
		observed := g.recentWindowTokens
		if elapsed := now.Sub(g.recentWindowStart); elapsed > time.Second && elapsed < time.Minute {
			// 窗口已过 1s 到 1min 之间时按已过时长折算成每分钟速率；不满 1s
			// 不折算，避免用极短时长做除数得到虚高的速率。
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
