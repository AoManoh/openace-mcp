package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// usageServer 返回 4 维向量；withUsage 为 true 时报告 usage.total_tokens =
// 7 × 条数，short 为 true 时少返回一条（数量不符，客户端整批拒绝）。
func usageServer(t *testing.T, withUsage bool, short bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := len(req.Input)
		if short {
			n--
		}
		data := make([]map[string]any, n)
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		body := map[string]any{"data": data}
		if withUsage {
			body["usage"] = map[string]int{"total_tokens": 7 * len(req.Input)}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func usageClient(t *testing.T, url string) *Client {
	t.Helper()
	client, err := NewClient(Config{
		Enabled: true, ProviderType: ProviderOpenAI, BaseURL: url, Model: "m", Dimension: 4,
		BatchSize: 8, InitialConcurrency: 2, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestUsageRecordedPerKindAndPerContext：成功响应里的 usage.total_tokens 按文档、
// 查询分列记入客户端累计，也记入 ctx 携带的计量器。
func TestUsageRecordedPerKindAndPerContext(t *testing.T) {
	client := usageClient(t, usageServer(t, true, false).URL)
	meter := &reliability.UsageMeter{}
	ctx := reliability.WithUsageMeter(context.Background(), meter)
	if _, err := client.EmbedBatch(ctx, []string{"a", "b"}, InputDocument); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedQuery(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	got := client.UsageSnapshot()
	if got.DocumentTokens != 14 || got.DocumentRequests != 1 || got.QueryTokens != 7 || got.QueryRequests != 1 || got.UnreportedRequests != 0 {
		t.Fatalf("客户端累计不符: %+v", got)
	}
	if m := meter.Snapshot(); m.DocumentTokens != 14 || m.DocumentRequests != 1 || m.QueryRequests != 0 {
		t.Fatalf("ctx 计量器只应记经它发出的文档请求: %+v", m)
	}
}

// TestUsageMissingCountsAsUnreported：成功响应没有用量字段时只计请求数与
// UnreportedRequests，不估算 tokens。
func TestUsageMissingCountsAsUnreported(t *testing.T) {
	client := usageClient(t, usageServer(t, false, false).URL)
	if _, err := client.EmbedBatch(context.Background(), []string{"a"}, InputDocument); err != nil {
		t.Fatal(err)
	}
	got := client.UsageSnapshot()
	if got.DocumentRequests != 1 || got.DocumentTokens != 0 || got.UnreportedRequests != 1 {
		t.Fatalf("未报告用量的响应应只计请求: %+v", got)
	}
}

// TestUsageRecordedWhenBatchRejected：服务商返回 200 后因条数不符被整批拒绝，
// 这次已经计费，用量照样记账。
func TestUsageRecordedWhenBatchRejected(t *testing.T) {
	client := usageClient(t, usageServer(t, true, true).URL)
	if _, err := client.EmbedBatch(context.Background(), []string{"a", "b", "c"}, InputDocument); err == nil {
		t.Fatal("条数不符应整批拒绝")
	}
	if got := client.UsageSnapshot(); got.DocumentTokens != 21 || got.DocumentRequests != 1 {
		t.Fatalf("被拒绝但已计费的响应应记账: %+v", got)
	}
}
