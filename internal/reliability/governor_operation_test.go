package reliability

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGovernorOperationReservationIsAtomicAndLeavesRequestSpace(t *testing.T) {
	const requestBytes = 64 << 10
	g := NewGovernor(16)
	g.sampleResources = func() resourceSnapshot {
		return resourceSnapshot{memoryKnown: true, fdKnown: true,
			availableBytes: 2 * (requestBytes + 2*operationBytes), availableFD: 100}
	}
	var wg sync.WaitGroup
	releases := make(chan func(), 64)
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if release, ok := g.TryStartOperation(requestBytes); ok {
				releases <- release
			}
		}()
	}
	close(start)
	wg.Wait()
	close(releases)
	if len(releases) != 2 {
		t.Fatalf("逻辑批次必须原子预留: reserved=%d want=2", len(releases))
	}
	// 等待批次即使占满自身预留，仍有至少一个 HTTP 请求可以开始并释放内存。
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := g.AcquireIndexWithBudget(ctx, 1, requestBytes, nil)
	if err != nil {
		t.Fatalf("等待批次阻塞全部请求: %v", err)
	}
	g.Observe(p, OutcomeOther, 0, 0)
	for release := range releases {
		release()
		release()
	}
	if g.operations != 0 || g.Snapshot().InFlight != 0 {
		t.Fatalf("重复释放破坏预留: operations=%d snapshot=%+v", g.operations, g.Snapshot())
	}
	if release, ok := g.TryStartOperation(requestBytes); !ok {
		t.Fatal("归还后未恢复预留")
	} else {
		release()
	}
}

func TestGovernorOperationReservationRejectsOverflow(t *testing.T) {
	g, _ := governed(16)
	for _, bytes := range []int64{-1, int64(^uint64(0) >> 1)} {
		if release, ok := g.TryStartOperation(bytes); ok {
			release()
			t.Fatalf("溢出预留被接受: %d", bytes)
		}
	}
	if g.operations != 0 {
		t.Fatalf("失败预留残留: %d", g.operations)
	}
}
