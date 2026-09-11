package reliability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type governorClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newGovernorClock() *governorClock  { return &governorClock{cur: time.Unix(1_700_000_000, 0)} }
func (c *governorClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.cur }
func (c *governorClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cur = c.cur.Add(d)
	c.mu.Unlock()
	return nil
}
func governed(initial int) (*Governor, *governorClock) {
	g := NewGovernor(initial)
	clock := newGovernorClock()
	g.now = clock.now
	g.sleep = clock.sleep
	g.sampleResources = func() resourceSnapshot {
		return resourceSnapshot{memoryKnown: true, fdKnown: true, availableBytes: 1 << 40, availableFD: 1 << 20}
	}
	return g, clock
}
func mustAcquire(t *testing.T, g *Governor, tokens int) IndexPermit {
	t.Helper()
	p, err := g.AcquireIndex(context.Background(), tokens)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func completeGroup(t *testing.T, g *Governor, clock *governorClock, d time.Duration) {
	t.Helper()
	n := g.Snapshot().Window
	ps := make([]IndexPermit, n)
	for i := range ps {
		ps[i] = mustAcquire(t, g, 100)
	}
	if err := clock.sleep(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		g.Observe(p, OutcomeSuccess, d, 0)
	}
}

func TestGovernorPreLearningUnthrottled(t *testing.T) {
	g, _ := governed(4)
	g.sleep = func(context.Context, time.Duration) error { t.Fatal("尚未限流却等待"); return nil }
	for i := 0; i < 200; i++ {
		p := mustAcquire(t, g, 100_000)
		g.Observe(p, OutcomeOther, 0, 0)
	}
	if s := g.Snapshot(); s.RateLearning || s.InFlight != 0 {
		t.Fatalf("状态错误: %+v", s)
	}
}
func TestGovernor429EntersLearningAndPauses(t *testing.T) {
	g, clock := governed(4)
	start := clock.now()
	p := mustAcquire(t, g, 50_000)
	g.Observe(p, OutcomeRateLimited, 0, 7*time.Second)
	s := g.Snapshot()
	if !s.RateLearning || s.TargetTokensPerMin != 40_000 || !s.PausedUntil.Equal(start.Add(7*time.Second)) || s.Window != 4 {
		t.Fatalf("429 状态: %+v", s)
	}
	p = mustAcquire(t, g, 1000)
	if clock.now().Sub(start) < 7*time.Second {
		t.Fatal("未遵守 Retry-After")
	}
	g.Observe(p, OutcomeOther, 0, 0)
}
func TestGovernorRepeated429HitsFloor(t *testing.T) {
	g, _ := governed(1)
	g.rateLearning = true
	g.targetTokensPerMin = 2_000_000
	g.bucketTokens = 2_000_000
	for i := 0; i < 12; i++ {
		p := mustAcquire(t, g, 1000)
		g.Observe(p, OutcomeRateLimited, 0, time.Millisecond)
	}
	if !g.AtRateFloor() {
		t.Fatalf("既有速率规则未生效: %+v", g.Snapshot())
	}
}
func TestGovernorOversizedRequestProgressesWithDebt(t *testing.T) {
	g, clock := governed(1)
	g.rateLearning = true
	g.targetTokensPerMin = 40_000
	g.bucketTokens = 40_000
	g.lastRefill = clock.now()
	p := mustAcquire(t, g, 65_536)
	if g.bucketTokens != -25_536 {
		t.Fatalf("债务错误: %f", g.bucketTokens)
	}
	g.Observe(p, OutcomeOther, 0, 0)
	start := clock.now()
	p = mustAcquire(t, g, 1000)
	if clock.now().Sub(start) < 39*time.Second {
		t.Fatal("未偿还超额 token 债务")
	}
	g.Observe(p, OutcomeOther, 0, 0)
}
func TestGovernorSuccessRaisesLearnedRate(t *testing.T) {
	g, _ := governed(1)
	g.rateLearning = true
	g.targetTokensPerMin = 50_000
	g.bucketTokens = 50_000
	p := mustAcquire(t, g, 1000)
	g.Observe(p, OutcomeSuccess, time.Second, 0)
	if g.Snapshot().TargetTokensPerMin != 51_000 {
		t.Fatalf("速率恢复: %+v", g.Snapshot())
	}
}
func TestGovernorGrowsBeyondInitialWindow(t *testing.T) {
	g, clock := governed(16)
	for i := 0; i < 7; i++ {
		completeGroup(t, g, clock, time.Second)
	}
	if s := g.Snapshot(); s.Window <= 128 || s.InFlight != 0 {
		t.Fatalf("窗口应越过初始值与 128: %+v", s)
	}
}
func TestGovernorSuccessfulLatencyShiftDoesNotCollapseWindow(t *testing.T) {
	g, clock := governed(16)
	completeGroup(t, g, clock, time.Second)
	for i := 0; i < 8; i++ {
		completeGroup(t, g, clock, 8*time.Second)
		if g.Snapshot().Window < 16 {
			t.Fatalf("时延变化使窗口连续缩小: %+v", g.Snapshot())
		}
	}
}
func TestGovernorNoThroughputGainReturnsToVerifiedWindow(t *testing.T) {
	g, clock := governed(16)
	completeGroup(t, g, clock, time.Second)
	completeGroup(t, g, clock, 1500*time.Millisecond)
	if s := g.Snapshot(); s.Window != 16 || s.LastAdjustment != "no-throughput-gain" {
		t.Fatalf("无吞吐收益应返回验证窗口: %+v", s)
	}
}

func TestGovernorCountsCompletionsAcrossSampleBoundary(t *testing.T) {
	g, clock := governed(16)
	first := make([]IndexPermit, 16)
	for i := range first {
		first[i] = mustAcquire(t, g, 100)
	}
	_ = clock.sleep(context.Background(), time.Second)
	inherited := make([]IndexPermit, 15)
	// 完成的请求立即由待处理工作补充；最后一个观测请求完成时，仍有
	// 15 个真实请求在执行。这是持续投放的正常时序。
	for i := range inherited {
		g.Observe(first[i], OutcomeSuccess, time.Second, 0)
		inherited[i] = mustAcquire(t, g, 100)
	}
	g.Observe(first[15], OutcomeSuccess, time.Second, 0)
	if g.Snapshot().Window != 24 {
		t.Fatal("首组应开始 24 并发试探")
	}
	newFirst := make([]IndexPermit, 9)
	for i := range newFirst {
		newFirst[i] = mustAcquire(t, g, 100)
	}
	_ = clock.sleep(context.Background(), time.Second)
	newLast := make([]IndexPermit, 15)
	for i, p := range inherited {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
		newLast[i] = mustAcquire(t, g, 100)
	}
	additional := make([]IndexPermit, 9)
	for i, p := range newFirst {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
		additional[i] = mustAcquire(t, g, 100)
	}
	_ = clock.sleep(context.Background(), time.Second)
	for _, p := range additional {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	for _, p := range newLast {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	// 24 并发期间的 2 秒共完成 48 个请求，吞吐比 16 并发提高 50%。
	// 只计算第二组新登记的 24 个请求会误报为下降 25%。
	if s := g.Snapshot(); s.Window != 36 || s.LastAdjustment != "throughput-probe" {
		t.Fatalf("持续完成的请求漏计导致错误回退: %+v", s)
	}
}
func TestGovernorLateOverloadsDoNotRepeatShrink(t *testing.T) {
	g, _ := governed(16)
	ps := make([]IndexPermit, 16)
	for i := range ps {
		ps[i] = mustAcquire(t, g, 100)
	}
	for _, p := range ps {
		g.Observe(p, OutcomeOverload, time.Second, 0)
	}
	if s := g.Snapshot(); s.Window != 8 || s.InFlight != 0 {
		t.Fatalf("同组失败反复缩窗或名额泄漏: %+v", s)
	}
}
func TestGovernorPermitReleasedOnce(t *testing.T) {
	g, _ := governed(2)
	p1, p2 := mustAcquire(t, g, 1), mustAcquire(t, g, 1)
	g.Observe(p1, OutcomeOther, 0, 0)
	g.Observe(p1, OutcomeOther, 0, 0)
	if g.Snapshot().InFlight != 1 {
		t.Fatal("重复归还释放了别人的名额")
	}
	other, _ := governed(2)
	other.Observe(p2, OutcomeOther, 0, 0)
	if g.Snapshot().InFlight != 1 {
		t.Fatal("外部治理器改变了名额")
	}
	g.Observe(p2, OutcomeOther, 0, 0)
}
func TestGovernorAcquireCancellable(t *testing.T) {
	g := NewGovernor(1)
	p := mustAcquire(t, g, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.AcquireIndex(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("取消结果: %v", err)
	}
	g.Observe(p, OutcomeOther, 0, 0)
	p = mustAcquire(t, g, 1)
	g.Observe(p, OutcomeOther, 0, 0)
	if len(g.waiters) != 0 || g.Snapshot().InFlight != 0 {
		t.Fatal("取消后等待者或名额残留")
	}
}
func TestGovernorResourceWaitConsumesNoBudget(t *testing.T) {
	for _, r := range []resourceSnapshot{
		{memoryKnown: true, fdKnown: true, availableBytes: 1, availableFD: 100},
		{memoryKnown: true, fdKnown: true, availableBytes: 1 << 40, availableFD: 0},
		{memoryKnown: true, availableBytes: 0},
		{fdKnown: true, availableFD: 0},
		{memoryKnown: true, fdKnown: true, incomplete: true, availableBytes: 1024, availableFD: 100},
	} {
		g := NewGovernor(16)
		g.sampleResources = func() resourceSnapshot { return r }
		budget := NewRateLimiter(1, 100)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := g.AcquireIndexWithBudget(ctx, 10, 1024, budget)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || budget.usedReqs != 0 || g.Snapshot().InFlight != 0 || g.Snapshot().ResourceReason == "" {
			t.Fatalf("资源等待状态: err=%v budget=%d snapshot=%+v", err, budget.usedReqs, g.Snapshot())
		}
	}
}
func TestGovernorUnknownResourcesPreventExpansion(t *testing.T) {
	g, clock := governed(4)
	g.sampleResources = func() resourceSnapshot { return resourceSnapshot{} }
	completeGroup(t, g, clock, time.Second)
	if s := g.Snapshot(); s.Window != 4 || s.ResourceReason != "resource-unknown" {
		t.Fatalf("未知资源应可见且不扩张: %+v", s)
	}
}
func TestGovernorNilSafe(t *testing.T) {
	var g *Governor
	p, err := g.AcquireIndex(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	g.Observe(p, OutcomeSuccess, time.Second, 0)
	if g.Snapshot().Window != 0 || g.AtRateFloor() {
		t.Fatal("nil 状态非零")
	}
}
