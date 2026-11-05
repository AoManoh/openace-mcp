package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// TestRerankAbandonedRequestCounted：精排的第一次尝试超过单次超时被放弃，重试成功。
// 放弃的那次记 1 次、字节数等于第一次请求体长度，客户端累计与 ctx 计量器都记；
// 成功用量只计重试那次；写一行带种类与字节数、不带 key 的日志。
func TestRerankAbandonedRequestCounted(t *testing.T) {
	var mu sync.Mutex
	var bodies []int
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, len(raw))
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			select {
			case <-time.After(2 * time.Second):
			case <-release:
			}
		}
		type item struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []item{{Index: 0, RelevanceScore: 0.2}, {Index: 1, RelevanceScore: 0.8}},
			"usage": map[string]int{"total_tokens": 321},
		})
	}))
	defer func() { close(release); ts.Close() }()
	cfg := testConfig(ts.URL)
	cfg.Timeout = 100 * time.Millisecond
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fastRetry(client)
	var lines []string
	client.SetLogf(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	meter := &reliability.UsageMeter{}
	if _, _, err := client.Rerank(reliability.WithUsageMeter(context.Background(), meter), "q", docs(2)); err != nil {
		t.Fatalf("第一次尝试超时后重试应成功: %v", err)
	}
	mu.Lock()
	sent := append([]int(nil), bodies...)
	mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("应发出两次请求: %v", sent)
	}
	want := reliability.UsageCounts{RerankTokens: 321, RerankRequests: 1, RerankAbandonedRequests: 1, RerankAbandonedBytes: int64(sent[0])}
	if got := client.UsageSnapshot(); got != want {
		t.Fatalf("客户端累计不符: %+v，期望 %+v", got, want)
	}
	if got := meter.Snapshot(); got != want {
		t.Fatalf("ctx 计量器不符: %+v，期望 %+v", got, want)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "rerank") || !strings.Contains(lines[0], strconv.Itoa(sent[0])) || strings.Contains(lines[0], "canary-rerank-key") {
		t.Fatalf("应写一行带种类与字节数、不带 key 的日志: %q", lines)
	}
}

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
