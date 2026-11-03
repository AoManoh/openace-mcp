package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRerankReportsAttempts：前两次返回 503、第三次成功时，RerankWithStats 报告
// 3 次尝试、重试等待大于 0 与每次尝试的耗时；全部失败时同样报告尝试次数。
// 这些数字进入检索诊断，用来区分“服务商单次响应慢”与“客户端在重试”。
func TestRerankReportsAttempts(t *testing.T) {
	var calls atomic.Int32
	failFirst := int32(2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= atomic.LoadInt32(&failFirst) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 0, "relevance_score": 0.9}, {"index": 1, "relevance_score": 0.1},
		}})
	}))
	defer ts.Close()
	client, err := NewClient(testConfig(ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	fastRetry(client)

	hits, sent, stats, err := client.RerankWithStats(context.Background(), "q", docs(2))
	if err != nil || sent != 2 || len(hits) != 2 {
		t.Fatalf("third attempt should succeed: hits=%d sent=%d err=%v", len(hits), sent, err)
	}
	if stats.Attempts != 3 || len(stats.AttemptDurations) != 3 || stats.RetryWait <= 0 {
		t.Fatalf("stats should record 3 attempts and the waits between them: %+v", stats)
	}
	if stats.LongestAttempt() <= 0 {
		t.Fatalf("longest attempt should be positive: %+v", stats)
	}

	calls.Store(0)
	atomic.StoreInt32(&failFirst, 100)
	_, _, stats, err = client.RerankWithStats(context.Background(), "q", docs(2))
	if err == nil {
		t.Fatal("all attempts failing should return an error")
	}
	if stats.Attempts != 3 {
		t.Fatalf("failed call should still report attempts (MaxRetries=2 → 3 attempts): %+v", stats)
	}
}
