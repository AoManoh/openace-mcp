package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// TestRerankUsageRecorded：voyage 形状响应里的 usage.total_tokens 记入客户端
// 累计与 ctx 携带的计量器；没有用量字段的响应只计请求数与 UnreportedRequests。
func TestRerankUsageRecorded(t *testing.T) {
	withUsage := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		body := map[string]any{"data": []item{{Index: 0, RelevanceScore: 0.2}, {Index: 1, RelevanceScore: 0.8}}}
		if withUsage {
			body["usage"] = map[string]int{"total_tokens": 321}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()
	client, err := NewClient(testConfig(ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	meter := &reliability.UsageMeter{}
	if _, _, err := client.Rerank(reliability.WithUsageMeter(context.Background(), meter), "q", docs(2)); err != nil {
		t.Fatal(err)
	}
	withUsage = false
	if _, _, err := client.Rerank(context.Background(), "q", docs(2)); err != nil {
		t.Fatal(err)
	}
	got := client.UsageSnapshot()
	if got.RerankTokens != 321 || got.RerankRequests != 2 || got.UnreportedRequests != 1 {
		t.Fatalf("精排用量累计不符: %+v", got)
	}
	if m := meter.Snapshot(); m.RerankTokens != 321 || m.RerankRequests != 1 {
		t.Fatalf("ctx 计量器只应记经它发出的请求: %+v", m)
	}
}
