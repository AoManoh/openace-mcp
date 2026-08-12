package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEmbedBatchConcurrencyOverlap 验证并发槽允许多请求真实重叠:
// 16 个并发 EmbedBatch 在慢响应服务下的峰值在飞请求数应显著大于 1
// (用户批示 2026-08-12:高吞吐自部署环境索引效率默认拉满)。
func TestEmbedBatchConcurrencyOverlap(t *testing.T) {
	var inflight, peak int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond) // 慢响应迫使重叠
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "m", "usage": map[string]int{"total_tokens": 1}})
		atomic.AddInt64(&inflight, -1)
	}))
	defer server.Close()

	client, err := NewClient(Config{
		Enabled: true, ProviderType: ProviderOpenAI, BaseURL: server.URL,
		APIKey: "test", Model: "m", Dimension: 4,
		BatchSize: 8, MaxConcurrency: 16, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.EmbedBatch(context.Background(), []string{"a", "b"}, InputDocument); err != nil {
				t.Errorf("EmbedBatch: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&peak); got < 8 {
		t.Fatalf("peak in-flight = %d, want >= 8 (concurrency slots must actually overlap)", got)
	}
}

// TestEmbedBatchConcurrencyCappedBySemaphore 验证并发槽同时是硬上限:
// MaxConcurrency=2 时峰值在飞请求数不得超过 2(防误改把限流放飞)。
func TestEmbedBatchConcurrencyCappedBySemaphore(t *testing.T) {
	var inflight, peak int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "m", "usage": map[string]int{"total_tokens": 1}})
		atomic.AddInt64(&inflight, -1)
	}))
	defer server.Close()

	client, err := NewClient(Config{
		Enabled: true, ProviderType: ProviderOpenAI, BaseURL: server.URL,
		APIKey: "test", Model: "m", Dimension: 4,
		BatchSize: 8, MaxConcurrency: 2, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument); err != nil {
				t.Errorf("EmbedBatch: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&peak); got > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2 (semaphore must cap concurrency)", got)
	}
}
