package reliability

// 吞吐治理器（Governor）是索引批嵌入请求发出前的自适应准入控制：每个
// 请求先经它放行，发出后再把结果交回给它，它据此调整后续放行的节奏。
// 它同时处理两类上游的两种过载表现：
//
//   - 有配额的托管服务（Voyage、OpenAI 等）限制的是每分钟 token 数或
//     请求数，过载信号是 HTTP 429。要调节的量是发送速率：收到 429 时把
//     目标速率乘性减半并按 Retry-After 暂停，之后每个成功请求让速率
//     加性回升一点（即 AIMD：加性增、乘性减。AWS SDK 的 adaptive retry
//     模式用同一做法的客户端令牌桶）。不调节并发数的原因：实际速率等于
//     并发数 × 批大小 × 每批 token ÷ 单请求耗时，而托管服务的单请求耗时
//     波动大（本项目实测 1.3s 到 60s），固定并发数下速率会自行漂移。
//   - 自部署服务（TEI、vLLM 等）没有配额，过载时不返回 429，表现为延迟
//     上升、超时或 503。要调节的量是并发窗口：短期延迟均值明显高于
//     长期基线即判定服务端开始排队，把窗口乘性减半。延迟平稳时窗口
//     加性加一（Netflix 开源库 concurrency-limits 的延迟梯度算法用同一
//     做法，见 github.com/Netflix/concurrency-limits）。
//
// 两个控制器同时生效，请求要同时通过两者才放行。治理器不识别厂商，只看
// 信号：收到 429 调速率，延迟上升调窗口。
//
// 作用范围：只管索引批嵌入（document 类型）请求。交互查询的嵌入请求由
// embedding.Client 直接发出，不经过治理器，所以大批索引不会把交互查询
// 排到队尾。
//
// 与熔断器（Circuit，连续失败后暂停发请求）的分工：收到 429 时治理器先
// 降速继续发，调用方不把这次 429 计入熔断器。只有在目标速率已经降到
// governorRateFloorTokens 之后再收到 429（此时 AtRateFloor 为真），调用方
// 才把失败计入熔断器，熔断器进入退避（暂停发请求的时段），构建停止投放
// 后续批。引入治理器之前 429 直接计入熔断器，连续 429 期间索引吞吐为
// 零。全停这一步仍保留，用于避免在配额已用尽时反复重试、白白付费。

import (
	"context"
	"math"
	"sync"
	"time"
)

// 治理器常数。全部是经测试固定下来的起步值，出处与理由就地注明。没有
// 对应环境变量：项目约定只有出现真实运维需求时才增加环境变量，与熔断器
// 的退避常数同一处理。
const (
	// governorRateMDFactor 是收到 429 时目标速率的乘性下降因子。0.5 与
	// TCP 拥塞控制的 AIMD 以及 AWS adaptive retry 的公开默认值同一量级：
	// 一次限流按当前速率超出配额不到一倍处理。
	governorRateMDFactor = 0.5
	// governorRateAIFraction 是每个成功请求给目标速率的加性回升比例。
	// 2% 意味着约 35 个连续成功后速率回升到原来的两倍（1.02 的 35 次方
	// 约等于 2），与配额按分钟刷新的节奏相当。取更大值会在配额边缘反复
	// 撞 429。
	governorRateAIFraction = 0.02
	// governorRateFloorTokens 是学习到的目标速率允许降到的最低值，单位
	// tokens/min。默认一批 128 条、每条约 300 token，一批约 4 万 token。
	// 下限取一批的量级，是为了让"降到下限仍收到 429"等价于"连一批都发
	// 不出去"，只有这时才值得计入熔断器、停止构建。
	governorRateFloorTokens = 40_000
	// governorPauseFallback 是 429 没有携带 Retry-After 时的暂停时长，
	// 与熔断器同场景的 defaultRateLimitBackoff 取同一值。
	governorPauseFallback = 30 * time.Second
	// governorWindowEWMAShort 与 governorWindowEWMALong 是延迟的两个指数
	// 移动平均的平滑系数。短窗反映当下（约最近 5 个样本），长窗反映基线
	// （约最近 50 个样本）。短窗明显高于长窗即视为服务端开始排队，做法
	// 来自 Netflix concurrency-limits 的 Gradient2 算法，系数取保守值。
	governorWindowEWMAShort = 0.2
	governorWindowEWMALong  = 0.02
	// governorWindowDivergence 是判定排队的短窗/长窗比值阈值。1.5 倍以内
	// 视为正常抖动（托管服务的延迟本身波动大），超过即把窗口减半。
	governorWindowDivergence = 1.5
	// governorWindowGrowEvery 是窗口加性上探的节奏：每连续 8 个延迟正常
	// 的样本，窗口加一。
	governorWindowGrowEvery = 8
	// governorMinSamplesForGradient 是延迟判定生效所需的最少样本数。样本
	// 不足时长窗基线还不可信，不做减窗判定。
	governorMinSamplesForGradient = 10
)

// GovernorOutcome 是一次索引批嵌入请求结果的分类，调用方通过 Observe
// 上报。
type GovernorOutcome int

const (
	// OutcomeSuccess：请求成功，同时上报延迟样本。
	OutcomeSuccess GovernorOutcome = iota
	// OutcomeRateLimited：收到 429，同时上报 Retry-After（没有则为 0）。
	OutcomeRateLimited
	// OutcomeOverload：超时、5xx、连接失败等 ClassTransient 失败，视为
	// 容量过载信号。
	OutcomeOverload
	// OutcomeOther：其余失败（认证、欠费、永久错误、批次过大等）。这些
	// 不是吞吐信号，治理器不调整任何参数，只释放窗口槽位。错误本身由
	// 调用方按既有分类处理（上抛、拆批或计入熔断器），治理器不参与。
	OutcomeOther
)

// Governor 是索引批嵌入请求的吞吐治理器。它是 daemon 级单例，与
// embedding 客户端同生命周期。多个 workspace 并发构建时共用同一实例，
// 因为它们消耗的是同一个上游配额或同一套算力。
type Governor struct {
	mu sync.Mutex

	// 以下字段属于速率控制器（针对有配额的托管服务）。
	// rateLearning 在收到第一个 429 之前为 false，此时不限速，按全部并发
	// 发送。用户显式设置的 RPM/TPM 预算不在这里，由 RateLimiter 作为硬
	// 上限另行执行。
	rateLearning bool
	// targetTokensPerMin 是学习到的目标速率，只在 rateLearning 为真时有
	// 意义。
	targetTokensPerMin float64
	// bucketTokens 是令牌桶余额，按 targetTokensPerMin 匀速补充，上限为
	// 一分钟的额度。不允许长时间空闲后一次发出超过一分钟额度的量：有
	// 配额的服务按滑动窗口计数，这样发会立刻收到 429。余额可以为负，
	// 见 AcquireIndex。
	bucketTokens float64
	lastRefill   time.Time
	// pausedUntil 是 Retry-After 要求的暂停截止时刻，全部索引批请求都等
	// 到该时刻。
	pausedUntil time.Time
	// recentWindowStart 与 recentWindowTokens 记录最近一分钟实际成功发出
	// 的 token 数（窗口起点加累计值的近似）。收到第一个 429 时，用实测
	// 吞吐乘下降因子作为学习速率的初值，而不是从固定常数猜起。
	recentWindowStart  time.Time
	recentWindowTokens float64

	// 以下字段属于窗口控制器（针对自部署服务）。window 是当前允许的并发
	// 请求数，maxWindow 是其上限（用户配置的 MaxConcurrency），inFlight
	// 是在途请求数。ewmaShort 与 ewmaLong 是延迟的短窗、长窗指数移动
	// 平均，latencySample 是已采样次数，cleanStreak 是连续延迟正常的样本
	// 数。waiters 是等待窗口槽位的请求队列。
	window        int
	maxWindow     int
	inFlight      int
	ewmaShort     float64
	ewmaLong      float64
	latencySample int
	cleanStreak   int
	waiters       []chan struct{}

	// now 与 sleep 可注入，测试用确定性时钟。sleep 默认为 sleepContext，
	// 等待中 ctx 结束即返回。
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewGovernor 创建治理器。maxWindow 是并发窗口上限，取用户配置的
// MaxConcurrency：引入治理器后该配置的含义从"固定并发数"变为"并发数
// 上限"，治理器在 1 到上限之间调节。窗口起点等于上限：先按用户给的
// 并发全速发送，遇到过载信号再收缩，用户对自己服务的配置权保留。
// maxWindow 小于 1 时按 1 处理。
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

// AcquireIndex 是索引批请求的准入：请求发出前调用，返回 nil 表示可以
// 发送，nil 治理器直接放行。tokens 是本请求的 token 估算（每条文本按
// 字节数 ÷ 4 向上取整，与 RateLimiter 同一口径）。依次等待三道条件，
// ctx 结束时立即返回 ctx 错误：
//
//  1. Retry-After 暂停：pausedUntil 未到就等到该时刻。
//  2. 速率令牌：只在学习态（rateLearning 为真，即见过 429 之后）检查。
//     余额不足时按目标速率把缺口折算成等待时长，等完再重新判定。
//  3. 并发窗口槽位：inFlight 小于 window 即占一个槽位返回。否则入队等
//     释放信号，被唤醒后回到循环开头重新判定全部三道条件。
//
// 每次成功准入都必须由一次 Observe 配对，Observe 释放槽位。
func (g *Governor) AcquireIndex(ctx context.Context, tokens int) error {
	if g == nil {
		return nil
	}
	for {
		g.mu.Lock()
		now := g.now()
		// 1) Retry-After 暂停：上游已经明确说了多久以后再来。
		if wait := g.pausedUntil.Sub(now); wait > 0 {
			g.mu.Unlock()
			if err := g.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		// 2) 速率令牌，只在学习态（见过 429 之后）检查。
		if g.rateLearning {
			g.refillLocked(now)
			// 单个请求的估算可以超过一分钟的目标额度：默认满批 128 条、每条
			// 顶格 2KB 的 chunk 估算约 65.5K token，而目标速率的下限是 40K。
			// 桶的上限恒等于一分钟额度，若要求余额攒够需求才放行，这类批次
			// 的需求在任何时刻都大于余额，构建 goroutine 一直等待，状态面上
			// 看不出原因。所以把本次需求先截到桶容量，桶满即放行，再从余额
			// 里扣掉完整需求，余额转负。后续请求要先等余额补回非负、再等到
			// 自己的需求，长期平均速率仍不超过目标。Guava 的 SmoothRateLimiter
			// 用同一做法：当前请求放行，等待由后来的请求承担。
			need := float64(tokens)
			if need > g.targetTokensPerMin {
				need = g.targetTokensPerMin
			}
			if g.bucketTokens < need {
				// 缺口按目标速率折算成等待时长，至少 50ms，避免高频空转。
				// 余额为负时缺口包含先补回负值的部分，等待相应变长。
				deficit := need - g.bucketTokens
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
		}
		// 3) 并发窗口槽位。窗口可能已被延迟判定缩到小于在途数，此时排队
		// 等释放信号，不轮询。令牌只在真正拿到槽位、请求即将发出时扣减：
		// 排队的请求被唤醒后要回到循环开头重走速率检查，若在检查处就扣，
		// 每排队一次就多扣一次，余额低于真实消耗，拥堵时的发送速率会低于
		// 目标速率。速率检查在此只决定"现在能不能放行"，不记账。
		if g.inFlight < g.window {
			if g.rateLearning {
				g.bucketTokens -= float64(tokens)
			}
			g.inFlight++
			g.mu.Unlock()
			return nil
		}
		ready := make(chan struct{})
		g.waiters = append(g.waiters, ready)
		g.mu.Unlock()
		select {
		case <-ready:
			// 被唤醒只表示可能有槽位，没有预占：回到循环开头重新判定暂停、
			// 速率、槽位三道条件，没抢到就再次入队。此时尚未扣过令牌，
			// 重走速率检查不会重复计费。
		case <-ctx.Done():
			g.abandonWaiter(ready)
			return ctx.Err()
		}
	}
}

// Observe 上报一次索引批请求的结果，与 AcquireIndex 一一配对：请求的
// 每条返回路径都必须调用，否则窗口槽位泄漏，可用并发越来越少直到全部
// 请求排队。latency 只在 OutcomeSuccess 时有意义，retryAfter 只在
// OutcomeRateLimited 时有意义（0 表示上游没有声明）。各分类的动作：
//
//   - OutcomeSuccess：把 tokens 计入最近一分钟的实测吞吐。学习态下目标
//     速率加性回升 2%。再把 latency 交给延迟判定，可能减窗或加窗。
//   - OutcomeRateLimited：目标速率乘 governorRateMDFactor（第一个 429
//     以实测吞吐为基数），不低于 governorRateFloorTokens。暂停到当前
//     时刻加 Retry-After（没有则加 governorPauseFallback），只延后不
//     提前。
//   - OutcomeOverload：并发窗口减半。速率不动，因为没有 429 就没有配额
//     超限的证据。
//   - OutcomeOther：不调整任何参数。
//
// 无论哪种结果都释放一个窗口槽位并唤醒等待者。nil 治理器直接返回。
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
			// 加性回升不设上限：真实上限由下一次 429 重新确定。用户显式
			// 设置的 RPM/TPM 预算由 RateLimiter 另行执行。
			g.targetTokensPerMin += g.targetTokensPerMin * governorRateAIFraction
		}
		g.observeLatencyLocked(latency)
	case OutcomeRateLimited:
		g.onRateLimitedLocked(now, retryAfter)
	case OutcomeOverload:
		// 容量过载只减窗口。没有 429 就没有配额超限的证据，速率不动。
		g.shrinkWindowLocked()
	case OutcomeOther:
		// 不是吞吐信号，不调整参数。
	}
}

// AtRateFloor 报告速率控制器是否已把目标速率降到 governorRateFloorTokens。
// 调用方据此决定一次 429 的去向：未到下限时只交给治理器降速，不计入
// 熔断器。已到下限仍收到 429，说明连一批都发不出去，才计入熔断器，
// 熔断器进入退避后构建停止投放后续批。nil 治理器返回 false。
func (g *Governor) AtRateFloor() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rateLearning && g.targetTokensPerMin <= governorRateFloorTokens
}

// GovernorSnapshot 是治理器某一时刻的只读状态，进入 workspace_status 的
// semantic 块。用户据此能区分"构建慢是因为治理器在降速或暂停"与
// "provider 本身故障"，不必猜测后重启 daemon。
type GovernorSnapshot struct {
	RateLearning       bool      `json:"rate_learning,omitempty"`
	TargetTokensPerMin int       `json:"target_tokens_per_min,omitempty"`
	PausedUntil        time.Time `json:"paused_until,omitempty"`
	Window             int       `json:"window,omitempty"`
	MaxWindow          int       `json:"max_window,omitempty"`
	InFlight           int       `json:"in_flight,omitempty"`
}

// Snapshot 返回当前状态。nil 治理器返回零值。
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

// 以下是速率控制器的内部方法，调用方必须已持有 mu。

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
			// 窗口不满一分钟时按已过时长折算成每分钟速率。
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

// 以下是窗口控制器的内部方法，调用方必须已持有 mu。

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
		// 判定排队：窗口减半，并把短窗均值重置为长窗均值。不重置的话，
		// 已经计入短窗的高延迟样本会让接下来每个样本都再触发一次减半，
		// 窗口一路缩到 1。
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
	// 只发信号，不替被唤醒者预占槽位：它回到 AcquireIndex 循环开头重新
	// 判定，没抢到就再次入队，所以多发几个信号没有副作用。若在这里预占，
	// 被唤醒者重新判定时又占一次，同一个槽位被计两次。
	free := g.window - g.inFlight
	for free > 0 && len(g.waiters) > 0 {
		close(g.waiters[0])
		g.waiters = g.waiters[1:]
		free--
	}
}

// abandonWaiter 把因 ctx 结束而放弃等待的等待者移出队列。队列里找不到
// 时，说明它已被唤醒并出队，只是随后按取消返回了。唤醒不预占槽位，所以
// 这里没有需要归还的东西。
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
