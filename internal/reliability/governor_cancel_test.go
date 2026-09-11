package reliability

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// 在已被唤醒后的首个 Err 检查取消，确定性覆盖该退出路径。
type cancelOnRecheck struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int32
}

func (c *cancelOnRecheck) Err() error {
	if c.checks.Add(1) == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestGovernorCanceledWakeHandsOffToNextWaiter(t *testing.T) {
	g := NewGovernor(1)
	p := mustAcquire(t, g, 1)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstCtx := &cancelOnRecheck{Context: base, cancel: cancel}
	first := make(chan error, 1)
	go func() { _, err := g.AcquireIndex(firstCtx, 1); first <- err }()
	waitCount := func(n int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			g.mu.Lock()
			count := len(g.waiters)
			g.mu.Unlock()
			if count == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("等待者数量未达到%d", n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitCount(1)
	ctx, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	second := make(chan error, 1)
	go func() {
		p, err := g.AcquireIndex(ctx, 1)
		if err == nil {
			g.Observe(p, OutcomeOther, 0, 0)
		}
		second <- err
	}()
	waitCount(2)
	g.Observe(p, OutcomeOther, 0, 0)
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("第一个等待者未取消: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("取消的已唤醒请求未交接空闲名额: %v", err)
	}
}
