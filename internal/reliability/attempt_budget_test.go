package reliability

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimiterCanceledContextDoesNotConsumeBudget(t *testing.T) {
	l := NewRateLimiter(1, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Acquire(ctx, 1, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消调用仍获预算: %v", err)
	}
	if l.usedReqs != 0 || l.usedTokens != 0 {
		t.Fatalf("已取消调用消耗预算: requests=%d tokens=%d", l.usedReqs, l.usedTokens)
	}
}

func TestGovernorBudgetWaitDoesNotHoldPermit(t *testing.T) {
	g, clock := governed(2)
	l := NewRateLimiter(1, 0)
	l.now = clock.now
	if err := l.Acquire(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	g.rateLearning, g.targetTokensPerMin, g.bucketTokens = true, 60_000, 60_000
	g.lastRefill = clock.now()
	waits := 0
	l.sleep = func(ctx context.Context, d time.Duration) error {
		waits++
		if g.Snapshot().InFlight != 0 || g.bucketTokens != 60_000 {
			t.Fatal("预算等待占了名额或重复扣了已学习速率令牌")
		}
		if d != time.Minute {
			t.Fatalf("预算窗口等待 = %s", d)
		}
		return clock.sleep(ctx, d)
	}
	if err := g.AcquireIndexWithBudget(context.Background(), 100, l); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || l.usedReqs != 1 || g.Snapshot().InFlight != 1 || g.bucketTokens != 59_900 {
		t.Fatalf("跨窗口准入必须只登记一次: waits=%d requests=%d governor=%+v tokens=%f", waits, l.usedReqs, g.Snapshot(), g.bucketTokens)
	}
	g.Observe(OutcomeOther, 100, 0, 0)
}

func TestGovernorRechecksBudgetAfterSlotWaitAcrossMinute(t *testing.T) {
	g, clock := governed(1)
	l := NewRateLimiter(1, 0)
	l.now = clock.now
	if err := g.AcquireIndexWithBudget(context.Background(), 10, l); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budgetWait := make(chan struct{})
	l.sleep = func(ctx context.Context, _ time.Duration) error {
		close(budgetWait)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- g.AcquireIndexWithBudget(ctx, 10, l) }()
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		queued := len(g.waiters) == 1
		g.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("第二个请求未等待并发名额")
		}
		time.Sleep(time.Millisecond)
	}
	_ = clock.sleep(context.Background(), time.Minute)
	// 查询先消耗新窗口的预算，之后才释放旧请求的并发名额。
	if err := l.Acquire(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	g.Observe(OutcomeOther, 10, 0, 0)
	select {
	case <-budgetWait:
	case <-time.After(time.Second):
		t.Fatal("名额释放后没有重新核对新窗口预算")
	}
	if g.Snapshot().InFlight != 0 {
		t.Fatal("等待新窗口预算时仍持有名额")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消结果: %v", err)
	}
	if l.usedReqs != 1 {
		t.Fatalf("取消的等待者消耗预算: %d", l.usedReqs)
	}
}
