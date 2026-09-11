package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
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
		BatchSize: 8, InitialConcurrency: 16, Timeout: 5 * time.Second,
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

// 后续请求可越过初始窗口，验证客户端没有保留固定信号量。
func TestEmbedBatchConcurrencyGrowsBeyondInitialWindow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("自动扩窗仅在 Linux 有资源采样；其他平台维持当前窗口")
	}
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
		BatchSize: 8, InitialConcurrency: 2, Timeout: 5 * time.Second,
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
	if client.GovernorSnapshot().ResourceReason == "resource-unknown" {
		t.Skip("当前资源信息不完整，按产品约定不扩窗")
	}
	if got := atomic.LoadInt64(&peak); got <= 2 {
		t.Fatalf("peak in-flight = %d, want > 2 (initial window must not be a fixed ceiling)", got)
	}
}
