package reliability

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 同一时刻完成的请求可能以任意顺序归还，不能据此把真实吞吐增益判为下降。
func TestGovernorThroughputCompletionOrder(t *testing.T) {
	for _, fine := range []bool{false, true} {
		for _, cohortFirst := range []bool{false, true} {
			name := "coarse"
			if fine {
				name = "fine"
			}
			if cohortFirst {
				name += "_cohort_first"
			} else {
				name += "_additional_first"
			}
			t.Run(name, func(t *testing.T) {
				g, clock := governed(16)
				g.fine = fine
				first := make([]IndexPermit, 16)
				for i := range first {
					first[i] = mustAcquire(t, g, 100)
				}
				_ = clock.sleep(context.Background(), time.Second)
				inherited := make([]IndexPermit, 15)
				for i := range inherited {
					g.Observe(first[i], OutcomeSuccess, time.Second, 0)
					inherited[i] = mustAcquire(t, g, 100)
				}
				g.Observe(first[15], OutcomeSuccess, time.Second, 0)
				probeWindow := g.Snapshot().Window
				newFirst := make([]IndexPermit, probeWindow-15)
				for i := range newFirst {
					newFirst[i] = mustAcquire(t, g, 100)
				}
				_ = clock.sleep(context.Background(), time.Second)
				newLast := make([]IndexPermit, 15)
				for i, p := range inherited {
					g.Observe(p, OutcomeSuccess, time.Second, 0)
					newLast[i] = mustAcquire(t, g, 100)
				}
				additional := make([]IndexPermit, len(newFirst))
				for i, p := range newFirst {
					g.Observe(p, OutcomeSuccess, time.Second, 0)
					additional[i] = mustAcquire(t, g, 100)
				}
				_ = clock.sleep(context.Background(), time.Second)
				observe := func(ps []IndexPermit) {
					for _, p := range ps {
						g.Observe(p, OutcomeSuccess, time.Second, 0)
					}
				}
				if cohortFirst {
					observe(newLast)
					observe(additional)
				} else {
					observe(additional)
					observe(newLast)
				}
				if s := g.Snapshot(); s.Window <= probeWindow {
					t.Fatalf("每秒可完成%d请求，但回到%d；同时完成时的观察边界仍影响判断", probeWindow, s.Window)
				}
			})
		}
	}
}

// 返回细步试探结束但尚有组外请求未归还的状态，供后续测试补充成功、
// 过载或取消结果。未归还请求的完成量可能改变此次吞吐判断。
func pendingFineSample(t *testing.T, initial int) (*Governor, *governorClock, []IndexPermit) {
	t.Helper()
	g, clock := governed(initial)
	g.fine = true
	first := acquireGroup(t, g, initial)
	_ = clock.sleep(context.Background(), time.Second)
	inherited := make([]IndexPermit, initial-1)
	for i := range inherited {
		g.Observe(first[i], OutcomeSuccess, time.Second, 0)
		inherited[i] = mustAcquire(t, g, 100)
	}
	g.Observe(first[initial-1], OutcomeSuccess, time.Second, 0)
	probeWindow := g.Snapshot().Window
	firstProbe := acquireGroup(t, g, probeWindow-len(inherited))
	_ = clock.sleep(context.Background(), time.Second)
	lastProbe := make([]IndexPermit, len(inherited))
	for i, p := range inherited {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
		lastProbe[i] = mustAcquire(t, g, 100)
	}
	pending := make([]IndexPermit, len(firstProbe))
	for i, p := range firstProbe {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
		pending[i] = mustAcquire(t, g, 100)
	}
	_ = clock.sleep(context.Background(), time.Second)
	for _, p := range lastProbe {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
	}
	if s := g.Snapshot(); s.Window != probeWindow || s.LastAdjustment != "throughput-inconclusive" {
		t.Fatalf("完整反馈到达前不应退回已验证窗口: %+v", s)
	}
	return g, clock, pending
}

func acquireGroup(t *testing.T, g *Governor, count int) []IndexPermit {
	t.Helper()
	ps := make([]IndexPermit, count)
	for i := range ps {
		ps[i] = mustAcquire(t, g, 100)
	}
	return ps
}

func assertGovernorDrained(t *testing.T, g *Governor) {
	t.Helper()
	if s := g.Snapshot(); s.InFlight != 0 || len(g.active) != 0 || len(g.waiters) != 0 || g.inFlightBytes != 0 || g.inFlightTokens != 0 {
		t.Fatalf("全部请求归还后仍有许可、等待者或资源计数: %+v", s)
	}
}

func TestGovernorSettlementDoesNotWaitForNewAdmissions(t *testing.T) {
	g, _, pending := pendingFineSample(t, 16)
	newer := acquireGroup(t, g, 15)
	for _, p := range pending[:2] {
		g.Observe(p, OutcomeSuccess, time.Second, 0)
		newer = append(newer, mustAcquire(t, g, 100))
	}
	g.Observe(pending[2], OutcomeSuccess, time.Second, 0)
	if s := g.Snapshot(); s.Window != 20 || s.LastAdjustment != "throughput-probe" || s.InFlight != 17 {
		t.Fatalf("新准入的请求不应延长前次结算: %+v", s)
	}
	for _, p := range newer {
		g.Observe(p, OutcomeOther, 0, 0)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorSettlementRespondsToAdditionalOverloads(t *testing.T) {
	g, _, pending := pendingFineSample(t, 16)
	g.Observe(pending[0], OutcomeOverload, time.Second, 0)
	if s := g.Snapshot(); s.Window != 18 {
		t.Fatalf("单次过载不能证明容量不足: %+v", s)
	}
	g.Observe(pending[1], OutcomeOverload, time.Second, 0)
	if s := g.Snapshot(); s.Window != 9 || s.LastAdjustment != "overload" {
		t.Fatalf("结算期间连续组外过载必须降低窗口: %+v", s)
	}
	g.Observe(pending[1], OutcomeOverload, time.Second, 0)
	g.Observe(pending[2], OutcomeOverload, time.Second, 0)
	if s := g.Snapshot(); s.Window != 9 {
		t.Fatalf("重复或迟到反馈再次降低窗口: %+v", s)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorSettlementCancellationReleasesAllPermits(t *testing.T) {
	g, _, pending := pendingFineSample(t, 16)
	newer := acquireGroup(t, g, 15)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.AcquireIndex(ctx, 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("已满窗口的等待必须可取消: %v", err)
	}
	for _, p := range append(pending, newer...) {
		g.Observe(p, OutcomeOther, 0, 0)
		g.Observe(p, OutcomeOther, 0, 0)
	}
	assertGovernorDrained(t, g)
	if s := g.Snapshot(); s.Window != 18 || g.verifiedWindow != 16 {
		t.Fatalf("取消不能被记为服务过载或成功吞吐: %+v", s)
	}
}

func TestGovernorSettlementKeepsOverloadObservationCounts(t *testing.T) {
	g, _, pending := pendingFineSample(t, 32)
	for _, p := range pending[:2] {
		g.Observe(p, OutcomeOverload, time.Second, 0)
	}
	// 当前 epoch 已成功完成 36 次，另有旧 epoch 的 31 次只用于吞吐。
	// 2/38 尚未达到既有 10% 条件，结算开始不能把分母清零。
	if s := g.Snapshot(); s.Window != 36 {
		t.Fatalf("结算开始丢弃先前成功次数，导致过早缩窗: %+v", s)
	}
	for _, p := range pending[2:] {
		g.Observe(p, OutcomeOverload, time.Second, 0)
	}
	if s := g.Snapshot(); s.Window != 18 || s.LastAdjustment != "overload" {
		t.Fatalf("第 4 次组外过载达到 4/40 时应缩窗，第 5 次迟到反馈不得再次缩窗: %+v", s)
	}
	assertGovernorDrained(t, g)
}

func TestGovernorQueuedCapacityDoesNotAppearAsThroughputGain(t *testing.T) {
	for _, fine := range []bool{false, true} {
		name := "coarse"
		if fine {
			name = "fine"
		}
		t.Run(name, func(t *testing.T) {
			g, clock := governed(16)
			g.fine = fine
			completeGroup(t, g, clock, time.Second)
			probeWindow := g.Snapshot().Window
			queue := acquireGroup(t, g, probeWindow)
			// 服务持续每 62.5ms 完成一个请求，即每秒 16 个；客户端
			// 立即补充请求，增加在途数只会排队，不会增加完成速率。
			for i := 0; i < 4*probeWindow && g.Snapshot().Window > 16; i++ {
				_ = clock.sleep(context.Background(), time.Second/16)
				p := queue[0]
				queue = queue[1:]
				g.Observe(p, OutcomeSuccess, time.Second, 0)
				s := g.Snapshot()
				if s.Window > probeWindow {
					t.Fatalf("排队被误记为吞吐增益: %+v", s)
				}
				if s.InFlight < s.Window {
					queue = append(queue, mustAcquire(t, g, 100))
				}
			}
			if s := g.Snapshot(); s.Window != 16 || s.LastAdjustment != "no-throughput-gain" {
				t.Fatalf("持续供给下应结束无收益试探: %+v", s)
			}
			for _, p := range queue {
				g.Observe(p, OutcomeOther, 0, 0)
			}
			assertGovernorDrained(t, g)
		})
	}
}

func TestGovernorLongRequestIncludesAdditionalCompletions(t *testing.T) {
	for _, longFirst := range []bool{false, true} {
		name := "short_first"
		if longFirst {
			name = "long_first"
		}
		t.Run(name, func(t *testing.T) {
			g, clock := governed(16)
			g.fine = true
			completeGroup(t, g, clock, time.Second)
			long := mustAcquire(t, g, 100)
			short := acquireGroup(t, g, 17)
			for round := 0; round < 2; round++ {
				_ = clock.sleep(context.Background(), time.Second)
				for i, p := range short {
					g.Observe(p, OutcomeSuccess, time.Second, 0)
					short[i] = mustAcquire(t, g, 100)
				}
			}
			_ = clock.sleep(context.Background(), time.Second)
			if longFirst {
				g.Observe(long, OutcomeSuccess, 3*time.Second, 0)
			}
			for _, p := range short {
				g.Observe(p, OutcomeSuccess, time.Second, 0)
			}
			if !longFirst {
				g.Observe(long, OutcomeSuccess, 3*time.Second, 0)
			}
			// 3 秒实际完成 52 个请求，高于 16 并发时每秒 16 个的
			// 5% 增益门槛。长请求的先后反馈顺序不改变这个结果。
			if s := g.Snapshot(); s.Window != 20 || s.LastAdjustment != "throughput-probe" {
				t.Fatalf("组内长尾掩盖了组外完成量: %+v", s)
			}
			assertGovernorDrained(t, g)
		})
	}
}

func TestGovernorNeverFilledWindowKeepsVerifiedThroughput(t *testing.T) {
	g, clock := governed(16)
	g.fine = true
	completeGroup(t, g, clock, time.Second)
	verifiedRate := g.verifiedRate
	queue := acquireGroup(t, g, 2)
	for i := 0; i < 18; i++ {
		_ = clock.sleep(context.Background(), time.Second)
		p := queue[0]
		queue = queue[1:]
		g.Observe(p, OutcomeSuccess, time.Second, 0)
		if i < 16 {
			queue = append(queue, mustAcquire(t, g, 100))
		}
	}
	if s := g.Snapshot(); s.Window != 18 || s.LastAdjustment != "window-not-filled" || g.verifiedWindow != 16 || g.verifiedRate != verifiedRate {
		t.Fatalf("从未填满的窗口不能否定已有吞吐记录: %+v", s)
	}
	assertGovernorDrained(t, g)
}
