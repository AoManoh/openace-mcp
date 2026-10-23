package reliability

import (
	"context"
	"testing"
	"time"
)

// burst 在没有速率学习的状态下一次登记 n 个各 tokens 的请求，模拟构建开始
// 时按窗口并发投放的一批请求。
func burst(t *testing.T, g *Governor, n, tokens int) []IndexPermit {
	t.Helper()
	ps := make([]IndexPermit, n)
	for i := range ps {
		ps[i] = mustAcquire(t, g, tokens)
	}
	return ps
}

func near(got, want int) bool { return got >= want-1 && got <= want+1 }

// TestGovernorRateLimitBurstCutsOnce：同一批并发请求里 provider 接受了前 11 个
// （仍在处理中）、立即拒绝了后 5 个。5 个 429 属于同一次限速，学到的目标应是
// provider 已接受的 token 量乘 0.7（220,000 × 0.7），不能逐个减半落到下限
// 40,000；之后这 11 个请求陆续成功，也不改变目标。
func TestGovernorRateLimitBurstCutsOnce(t *testing.T) {
	g, _ := governed(16)
	ps := burst(t, g, 16, 20_000)
	for _, p := range ps[11:] {
		g.Observe(p, OutcomeRateLimited, time.Millisecond, 0)
	}
	s := g.Snapshot()
	if !s.RateLearning || !near(s.TargetTokensPerMin, 154_000) {
		t.Fatalf("同一次限速只应按已接受量减速一次（期望 154000）: %+v", s)
	}
	for _, p := range ps[:11] {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	if got := g.Snapshot().TargetTokensPerMin; !near(got, 154_000) {
		t.Fatalf("被接受请求的迟到成功不应改变学到的目标: %d", got)
	}
}

// TestGovernorRateLimiterWaitsForAcceptedBurstToAge：provider 按 60 秒窗口计数
// 时，被接受的那批请求在 60 秒内一直占用额度。学到目标后，新请求要等这批请求
// 移出 60 秒窗口才发出；只等 30 秒的暂停就发，会再次被拒绝。
func TestGovernorRateLimiterWaitsForAcceptedBurstToAge(t *testing.T) {
	g, clock := governed(16)
	start := clock.now()
	ps := burst(t, g, 16, 20_000)
	for _, p := range ps[11:] {
		g.Observe(p, OutcomeRateLimited, time.Millisecond, 0)
	}
	for _, p := range ps[:11] {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	p := mustAcquire(t, g, 20_000)
	if waited := clock.now().Sub(start); waited < time.Minute {
		t.Fatalf("已接受的 220000 token 仍在 60 秒窗口内时不应发出新请求，实际 %s 后就发出", waited)
	}
	g.Observe(p, OutcomeSuccess, time.Second, 0)
}

// TestGovernorRateLimitAfterCutIsNewEvent：减速之后才登记的请求再收到 429，
// 说明按新目标发送仍超限，是新的一次限速：目标再乘 0.7。减速前登记的请求
// 迟到的 429 仍属于上一次，不再减速。
func TestGovernorRateLimitAfterCutIsNewEvent(t *testing.T) {
	g, clock := governed(4)
	ps := burst(t, g, 4, 100_000)
	g.Observe(ps[3], OutcomeRateLimited, time.Millisecond, 0)
	if got := g.Snapshot().TargetTokensPerMin; !near(got, 210_000) {
		t.Fatalf("首次限速目标应为已接受的 300000 × 0.7: %d", got)
	}
	for _, p := range ps[:2] {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	// 那批请求移出 60 秒窗口之后再发，新请求不必等待，目标不因恢复而上调。
	_ = clock.sleep(context.Background(), 61*time.Second)
	late := mustAcquire(t, g, 100_000)
	g.Observe(late, OutcomeRateLimited, time.Millisecond, 0)
	if got := g.Snapshot().TargetTokensPerMin; !near(got, 147_000) {
		t.Fatalf("减速后登记的请求被拒应再减速一次（210000 × 0.7）: %d", got)
	}
	g.Observe(ps[2], OutcomeRateLimited, time.Millisecond, 0)
	if got := g.Snapshot().TargetTokensPerMin; !near(got, 147_000) {
		t.Fatalf("减速前登记的请求迟到的 429 不应再减速: %d", got)
	}
}

// TestGovernorRateRecoversAfterCleanBoundWindow：减速后一分钟内没有新的限速、
// 且有请求因目标速率等待，目标先乘 1.5 回到限速前速率（不超过它），之后每
// 个这样的一分钟乘 1.1。
func TestGovernorRateRecoversAfterCleanBoundWindow(t *testing.T) {
	g, clock := governed(4)
	g.rateLearning, g.targetTokensPerMin, g.rateMax, g.rateChangedAt = true, 100_000, 200_000, clock.now()
	for _, want := range []int{150_000, 200_000, 220_000} {
		// 第一个请求占满目标，第二个要等它移出窗口，等满一分钟时目标上调。
		first := mustAcquire(t, g, g.Snapshot().TargetTokensPerMin)
		g.Observe(first, OutcomeSuccess, time.Second, 0)
		second := mustAcquire(t, g, 1000)
		g.Observe(second, OutcomeSuccess, time.Second, 0)
		if got := g.Snapshot().TargetTokensPerMin; !near(got, want) {
			t.Fatalf("恢复步进错误: got %d want %d", got, want)
		}
		_ = clock.sleep(context.Background(), time.Minute)
	}
}

// TestGovernorRateDoesNotRiseWithoutDemand：一分钟内没有请求因目标速率等待，
// 目标不是瓶颈，不上调。
func TestGovernorRateDoesNotRiseWithoutDemand(t *testing.T) {
	g, clock := governed(4)
	g.rateLearning, g.targetTokensPerMin, g.rateMax, g.rateChangedAt = true, 100_000, 200_000, clock.now()
	p := mustAcquire(t, g, 10_000)
	g.Observe(p, OutcomeSuccess, time.Second, 0)
	_ = clock.sleep(context.Background(), 2*time.Minute)
	p = mustAcquire(t, g, 10_000)
	g.Observe(p, OutcomeSuccess, time.Second, 0)
	if got := g.Snapshot().TargetTokensPerMin; got != 100_000 {
		t.Fatalf("没有请求等待时不应上调目标: %d", got)
	}
}

// TestGovernorCountsRequestsRateLimitsAndRateWait：状态里的累计计数——请求尝试
// 数、429 次数、因限速等待的墙钟时长与成功请求耗时中位数。
func TestGovernorCountsRequestsRateLimitsAndRateWait(t *testing.T) {
	g, clock := governed(4)
	accepted := mustAcquire(t, g, 100_000)
	rejected := mustAcquire(t, g, 100_000)
	g.Observe(rejected, OutcomeRateLimited, time.Millisecond, 5*time.Second)
	g.Observe(accepted, OutcomeSuccess, 1500*time.Millisecond, 0)
	start := clock.now()
	p := mustAcquire(t, g, 10_000)
	waited := clock.now().Sub(start)
	g.Observe(p, OutcomeSuccess, 500*time.Millisecond, 0)
	s := g.Snapshot()
	if waited != time.Minute || s.Requests != 3 || s.RateLimited != 1 || s.RateWait != waited || s.LatencyP50 != 1500*time.Millisecond {
		t.Fatalf("计数错误: waited=%s snapshot=%+v", waited, s)
	}
}

// TestGovernorOversizedRequestWaitsForWindow：单个请求超过目标时，窗口里没有
// 其他请求就发出；之后的请求等它移出 60 秒窗口。
func TestGovernorOversizedRequestWaitsForWindow(t *testing.T) {
	g, clock := governed(1)
	g.rateLearning, g.targetTokensPerMin, g.rateChangedAt = true, 40_000, clock.now()
	start := clock.now()
	p := mustAcquire(t, g, 65_536)
	if clock.now() != start {
		t.Fatal("窗口为空时超过目标的单个请求应立即发出")
	}
	g.Observe(p, OutcomeSuccess, time.Second, 0)
	p = mustAcquire(t, g, 1000)
	if waited := clock.now().Sub(start); waited < time.Minute {
		t.Fatalf("超额请求仍在窗口内时不应发出新请求，实际等了 %s", waited)
	}
	g.Observe(p, OutcomeSuccess, time.Second, 0)
}

// TestGovernorFailedRequestsLeaveRateWindow：以连接失败或过载结束的请求没有被
// provider 接受，不计入第一次 429 时的已接受量；否则目标与限速前速率被估高。
func TestGovernorFailedRequestsLeaveRateWindow(t *testing.T) {
	g, _ := governed(8)
	ps := burst(t, g, 4, 100_000)
	g.Observe(ps[0], OutcomeOverload, time.Millisecond, 0)
	g.Observe(ps[1], OutcomeOther, time.Millisecond, 0)
	g.Observe(ps[3], OutcomeRateLimited, time.Millisecond, 0)
	if got := g.Snapshot().TargetTokensPerMin; !near(got, 70_000) {
		t.Fatalf("已接受量只应包括仍在处理中的 1 个请求（100000 × 0.7）: %d", got)
	}
}
