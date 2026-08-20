package reliability

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 治理器单测(任务 T6/T7)。虚拟时钟:sleep 直接拨快 now,速率/暂停等待
// 在测试里零真实耗时;窗口阻塞用真实小睡验证信号唤醒。

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

// acquire 是测试便捷封装:成功时立即 Observe 成功归还槽位。
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
	// 先积累一分钟内 600K tokens 的实测吞吐。
	for i := 0; i < 12; i++ {
		acquireRelease(t, g, 50_000, time.Second)
	}
	// 429(Retry-After=7s):进入学习态,初始目标=实测×0.5,硬暂停 7s。
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
	// 下一次准入要跨过 7s 硬暂停 + 令牌补充等待(桶从 0 起步)。
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
	// 建立 1s 基线(超过最小样本数)。
	for i := 0; i < 20; i++ {
		acquireRelease(t, g, 1_000, time.Second)
	}
	if g.Snapshot().Window != 16 {
		t.Fatalf("平稳期窗口不应收缩: %d", g.Snapshot().Window)
	}
	// 延迟陡增到 5s:短窗偏离长窗 → 乘性收窗。
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
	g.Observe(OutcomeOverload, 1_000, 0, 0) // 503/超时:收窗
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
	g := NewGovernor(1) // 真实时钟:验证阻塞与信号唤醒
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
	// 取消的等待者出队后,释放仍能唤醒后续等待者(队列无死条目)。
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
