package embedding

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestIndexBatchClosesUnusedReservationOnce(t *testing.T) {
	var released atomic.Int32
	b := &IndexBatch{release: func() { released.Add(1) }}
	b.Close()
	b.Close()
	if _, err := b.Embed(context.Background(), nil); err == nil {
		t.Fatal("已关闭批次再次执行")
	}
	if released.Load() != 1 {
		t.Fatalf("预留释放次数: %d", released.Load())
	}
}

func TestIndexBatchCloseRacesCanceledExecution(t *testing.T) {
	c, err := NewClient(Config{Enabled: true, ProviderType: ProviderOpenAI, Dimension: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 64; i++ {
		var released atomic.Int32
		b := &IndexBatch{client: c, texts: []string{"source"}, release: func() { released.Add(1) }}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; b.Close() }()
		go func() { defer wg.Done(); <-start; _, _ = b.Embed(ctx, nil) }()
		close(start)
		wg.Wait()
		if released.Load() != 1 {
			t.Fatalf("关闭与执行竞争后释放次数=%d", released.Load())
		}
	}
}

func TestIndexBatchCanceledExecutionReleasesReservation(t *testing.T) {
	cfg := Config{Enabled: true, ProviderType: ProviderOpenAI, Dimension: 8}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var released atomic.Int32
	b := &IndexBatch{client: c, texts: []string{"source"}, release: func() { released.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Embed(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消结果: %v", err)
	}
	b.Close()
	if released.Load() != 1 || c.GovernorSnapshot().InFlight != 0 {
		t.Fatalf("取消后预留未归还: %d %+v", released.Load(), c.GovernorSnapshot())
	}
}
