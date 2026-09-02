package reliability

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 本文件是 Governor 的单元测试。多数用例注入虚拟时钟：sleep 直接把 now
// 拨快，速率与暂停的等待在测试里不消耗真实时间。窗口阻塞与唤醒的用例
// 使用真实时钟和短暂的真实等待，因为要验证的是 goroutine 之间的信号。

type governorClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newGovernorClock() *governorClock {
	return &governorClock{cur: time.Unix(1_700_000_000, 0)}
}

func (c *governorClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *governorClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
	return nil
}

func governed(maxWindow int) (*Governor, *governorClock) {
	g := NewGovernor(maxWindow)
	clock := newGovernorClock()
	g.now = clock.now
	g.sleep = clock.sleep
	return g, clock
}

// acquireRelease 是测试辅助函数：AcquireIndex 放行后立即以 OutcomeSuccess
// 上报，归还窗口槽位。
func acquireRelease(t *testing.T, g *Governor, tokens int, latency time.Duration) {
	t.Helper()
	if err := g.AcquireIndex(context.Background(), tokens); err != nil {
		t.Fatalf("AcquireIndex: %v", err)
	}
	g.Observe(OutcomeSuccess, tokens, latency, 0)
}

func TestGovernorPreLearningUnthrottled(t *testing.T) {
	g, clock := governed(4)
	start := clock.now()
	for i := 0; i < 200; i++ {
		acquireRelease(t, g, 50_000, time.Second)
	}
	if got := clock.now().Sub(start); got != 0 {
		t.Fatalf("首个 429 之前不得限速,虚拟时钟前进了 %s", got)
	}
	if g.Snapshot().RateLearning {
		t.Fatal("未见 429 不应进入学习态")
	}
}

func TestGovernor429EntersLearningAndPauses(t *testing.T) {
	g, clock := governed(4)
	// 先在一分钟内成功发出 12 批共 600K token，作为实测吞吐。
	for i := 0; i < 12; i++ {
		acquireRelease(t, g, 50_000, time.Second)
	}
	// 收到带 Retry-After 7s 的 429：进入学习态（见过 429 后开始限速），
	// 初始目标速率为实测吞吐乘 0.5，并暂停 7s。
	if err := g.AcquireIndex(context.Background(), 50_000); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeRateLimited, 50_000, 0, 7*time.Second)
	snap := g.Snapshot()
	if !snap.RateLearning {
		t.Fatal("429 后必须进入学习态")
	}
	if snap.TargetTokensPerMin < governorRateFloorTokens {
		t.Fatalf("学习速率不得低于地板: %d", snap.TargetTokensPerMin)
	}
	if snap.TargetTokensPerMin > 600_000 {
		t.Fatalf("学习速率应为实测吞吐×%.1f 量级,得到 %d", governorRateMDFactor, snap.TargetTokensPerMin)
	}
	before := clock.now()
	// 下一次 AcquireIndex 要先等 7s 暂停，再等令牌桶从 0 补到本次需求。
	if err := g.AcquireIndex(context.Background(), 10_000); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeSuccess, 10_000, time.Second, 0)
	waited := clock.now().Sub(before)
	if waited < 7*time.Second {
		t.Fatalf("必须尊重 Retry-After 硬暂停,只等了 %s", waited)
	}
}

func TestGovernorRepeated429HitsFloor(t *testing.T) {
	g, _ := governed(2)
	for i := 0; i < 20; i++ {
		if err := g.AcquireIndex(context.Background(), 1_000); err != nil {
			t.Fatal(err)
		}
		g.Observe(OutcomeRateLimited, 1_000, 0, time.Millisecond)
	}
	if !g.AtRateFloor() {
		t.Fatalf("连续 429 后必须压到速率地板: %+v", g.Snapshot())
	}
}

func TestGovernorOversizedRequestProgressesWithDebt(t *testing.T) {
	g, clock := governed(4)
	// 刚启动就收到 429，没有实测吞吐：学习目标落到下限 40K tokens/min。
	if err := g.AcquireIndex(context.Background(), 1_000); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeRateLimited, 1_000, 0, time.Millisecond)
	if got := g.Snapshot().TargetTokensPerMin; got != governorRateFloorTokens {
		t.Fatalf("冷启动 429 后学习目标应为地板 %d, 得到 %d", governorRateFloorTokens, got)
	}
	// 单批估算 65,536 token 大于下限 40K。桶的上限等于一分钟额度，若要求
	// 余额攒够需求才放行，这一批的需求在任何时刻都大于余额，请求一直
	// 等待。期望行为：桶满即放行，超出部分让余额转负。保护：虚拟等待累计
	// 超过 10 分钟仍未放行就取消 ctx，让用例以失败结束而不是挂住。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var simulated time.Duration
	g.sleep = func(sleepCtx context.Context, d time.Duration) error {
		_ = clock.sleep(sleepCtx, d)
		simulated += d
		if simulated > 10*time.Minute {
			cancel()
			return sleepCtx.Err()
		}
		return nil
	}
	start := clock.now()
	if err := g.AcquireIndex(ctx, 65_536); err != nil {
		t.Fatalf("超大批次必须在桶满后放行并转负债,而不是无限等待: %v", err)
	}
	g.Observe(OutcomeSuccess, 65_536, time.Second, 0)
	if waited := clock.now().Sub(start); waited > 2*time.Minute {
		t.Fatalf("放行等待应为攒满一分钟额度的量级,实际 %s", waited)
	}
	// 余额为负后：下一个 1,000 token 请求要先等余额从 -25,536 补回 0，再
	// 等到 1,000，等待约 (25,536+1,000)/40,000×60s ≈ 40s。长期平均速率
	// 因此不超过目标。
	before := clock.now()
	if err := g.AcquireIndex(context.Background(), 1_000); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeSuccess, 1_000, time.Second, 0)
	if repaid := clock.now().Sub(before); repaid < 30*time.Second || repaid > 90*time.Second {
		t.Fatalf("负债偿还等待不在预期量级(≈40s): %s", repaid)
	}
}

func TestGovernorSuccessRaisesLearnedRate(t *testing.T) {
	g, _ := governed(2)
	if err := g.AcquireIndex(context.Background(), 1_000); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeRateLimited, 1_000, 0, time.Millisecond)
	low := g.Snapshot().TargetTokensPerMin
	for i := 0; i < 50; i++ {
		acquireRelease(t, g, 1_000, time.Second)
	}
	if got := g.Snapshot().TargetTokensPerMin; got <= low {
		t.Fatalf("干净期速率必须加性回升: %d -> %d", low, got)
	}
}

func TestGovernorLatencyDivergenceShrinksWindow(t *testing.T) {
	g, _ := governed(16)
	// 用 20 个 1s 延迟样本建立基线，样本数超过 governorMinSamplesForGradient。
	for i := 0; i < 20; i++ {
		acquireRelease(t, g, 1_000, time.Second)
	}
	if g.Snapshot().Window != 16 {
		t.Fatalf("平稳期窗口不应收缩: %d", g.Snapshot().Window)
	}
	// 延迟跳到 5s：短窗均值超过长窗均值的 1.5 倍，窗口应减半。
	for i := 0; i < 6; i++ {
		acquireRelease(t, g, 1_000, 5*time.Second)
	}
	if got := g.Snapshot().Window; got >= 16 {
		t.Fatalf("延迟偏离必须收窗: window=%d", got)
	}
}

func TestGovernorWindowRegrowsAfterCleanStreak(t *testing.T) {
	g, _ := governed(8)
	for i := 0; i < 20; i++ {
		acquireRelease(t, g, 1_000, time.Second)
	}
	if err := g.AcquireIndex(context.Background(), 1_000); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeOverload, 1_000, 0, 0) // 503 或超时：窗口应减半
	shrunk := g.Snapshot().Window
	if shrunk >= 8 {
		t.Fatalf("过载必须收窗: %d", shrunk)
	}
	for i := 0; i < governorWindowGrowEvery*3+3; i++ {
		acquireRelease(t, g, 1_000, time.Second)
	}
	if got := g.Snapshot().Window; got <= shrunk {
		t.Fatalf("干净期窗口必须回升: %d -> %d", shrunk, got)
	}
}

func TestGovernorWindowBlocksAndReleases(t *testing.T) {
	g := NewGovernor(1) // 真实时钟：验证阻塞与唤醒
	if err := g.AcquireIndex(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- g.AcquireIndex(context.Background(), 10)
	}()
	select {
	case <-done:
		t.Fatal("窗口占满时第二个准入不应立即通过")
	case <-time.After(50 * time.Millisecond):
	}
	g.Observe(OutcomeSuccess, 10, time.Second, 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("槽位释放后等待者未被唤醒")
	}
	g.Observe(OutcomeSuccess, 10, time.Second, 0)
}

func TestGovernorAcquireCancellable(t *testing.T) {
	g := NewGovernor(1)
	if err := g.AcquireIndex(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.AcquireIndex(ctx, 10); err == nil {
		t.Fatal("窗口占满+ctx 超时必须返回错误")
	}
	// 取消的等待者已出队，之后释放槽位仍能放行新的 AcquireIndex，队列里
	// 没有残留条目。
	g.Observe(OutcomeSuccess, 10, time.Second, 0)
	if err := g.AcquireIndex(context.Background(), 10); err != nil {
		t.Fatalf("取消者出队后正常准入失败: %v", err)
	}
	g.Observe(OutcomeSuccess, 10, time.Second, 0)
}

func TestGovernorNilSafe(t *testing.T) {
	var g *Governor
	if err := g.AcquireIndex(context.Background(), 10); err != nil {
		t.Fatal("nil 治理器必须直通(逃生门形态)")
	}
	g.Observe(OutcomeSuccess, 10, time.Second, 0)
	if g.AtRateFloor() {
		t.Fatal("nil 治理器不得报告地板")
	}
}
