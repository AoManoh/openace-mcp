package embedding

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

// slowFirstServer 的第一次请求先停 delay（足以让客户端的单次尝试超时）再返回，之后的
// 请求立即返回 4 维向量与 usage.total_tokens = 7 × 条数；bodies 按到达顺序记录每次
// 请求体的字节数。
type slowFirstServer struct {
	mu     sync.Mutex
	bodies []int
	url    string
}

func newSlowFirstServer(t *testing.T, delay time.Duration) *slowFirstServer {
	t.Helper()
	s := &slowFirstServer{}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, len(raw))
		first := len(s.bodies) == 1
		s.mu.Unlock()
		if first {
			select {
			case <-time.After(delay):
			case <-release:
			}
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &req)
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"total_tokens": 7 * len(req.Input)}})
	}))
	t.Cleanup(func() { close(release); server.Close() })
	s.url = server.URL
	return s
}

func (s *slowFirstServer) requestBodies() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.bodies...)
}

func abandonClient(t *testing.T, url string, timeout, queryTimeout time.Duration) *Client {
	t.Helper()
	client, err := NewClient(Config{
		Enabled: true, ProviderType: ProviderOpenAI, BaseURL: url, Model: "m", Dimension: 4,
		APIKey: "canary-secret-key", BatchSize: 8, InitialConcurrency: 2,
		Timeout: timeout, QueryTimeout: queryTimeout, MaxRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	fastRetry(client)
	return client
}

// TestAbandonedDocumentRequestCounted：文档请求的第一次尝试超过单次超时被放弃，重试
// 成功。放弃的那次记 1 次、字节数等于它的请求体长度，客户端累计与 ctx 计量器都记；
// 成功用量只计重试那次；写一行日志，带种类与字节数，不带 key。
func TestAbandonedDocumentRequestCounted(t *testing.T) {
	s := newSlowFirstServer(t, 2*time.Second)
	client := abandonClient(t, s.url, 100*time.Millisecond, 0)
	var lines []string
	client.SetLogf(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	meter := &reliability.UsageMeter{}
	ctx := reliability.WithUsageMeter(context.Background(), meter)
	if _, err := client.EmbedBatch(ctx, []string{"a", "b"}, InputDocument); err != nil {
		t.Fatalf("第一次尝试超时后重试应成功: %v", err)
	}
	bodies := s.requestBodies()
	if len(bodies) != 2 {
		t.Fatalf("应发出两次请求: %v", bodies)
	}
	want := reliability.UsageCounts{DocumentTokens: 14, DocumentRequests: 1, DocumentAbandonedRequests: 1, DocumentAbandonedBytes: int64(bodies[0])}
	if got := client.UsageSnapshot(); got != want {
		t.Fatalf("客户端累计不符: %+v，期望 %+v", got, want)
	}
	if got := meter.Snapshot(); got != want {
		t.Fatalf("ctx 计量器不符: %+v，期望 %+v", got, want)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "document") || !strings.Contains(lines[0], strconv.Itoa(bodies[0])) || strings.Contains(lines[0], "canary-secret-key") {
		t.Fatalf("应写一行带种类与字节数、不带 key 的日志: %q", lines)
	}
}

// TestAbandonedQueryRequestCounted：查询请求按查询一侧的单次超时被放弃时记到 query 两项。
func TestAbandonedQueryRequestCounted(t *testing.T) {
	s := newSlowFirstServer(t, 2*time.Second)
	client := abandonClient(t, s.url, 5*time.Second, 100*time.Millisecond)
	if _, err := client.EmbedQuery(context.Background(), "q"); err != nil {
		t.Fatalf("第一次尝试超时后重试应成功: %v", err)
	}
	bodies := s.requestBodies()
	want := reliability.UsageCounts{QueryTokens: 7, QueryRequests: 1, QueryAbandonedRequests: 1, QueryAbandonedBytes: int64(bodies[0])}
	if got := client.UsageSnapshot(); len(bodies) != 2 || got != want {
		t.Fatalf("查询放弃计数不符: %+v，期望 %+v（请求体 %v）", got, want, bodies)
	}
}

// TestCallerDeadlineNotCountedAsAbandoned：调用方自己的截止时间先到时不计放弃——调用方
// 已经离开，这次请求不会被重发。
func TestCallerDeadlineNotCountedAsAbandoned(t *testing.T) {
	s := newSlowFirstServer(t, 2*time.Second)
	client := abandonClient(t, s.url, 5*time.Second, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.EmbedBatch(ctx, []string{"a"}, InputDocument); err == nil {
		t.Fatal("调用方截止时间到了应返回错误")
	}
	if got := client.UsageSnapshot(); got != (reliability.UsageCounts{}) {
		t.Fatalf("调用方离开不应计放弃: %+v", got)
	}
}
