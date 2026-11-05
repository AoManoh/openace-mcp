package localengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestWorkspaceStatusReportsAbandonedRequests：第一次文档请求超过单次超时被放弃、重试
// 成功时，workspace_status 的 semantic.usage 与 ProviderUsage 都带被放弃的请求数与
// 请求体字节数，引擎的日志函数收到一行放弃记录。
func TestWorkspaceStatusReportsAbandonedRequests(t *testing.T) {
	const dim = 8
	var mu sync.Mutex
	var bodies []int
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, len(raw))
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			select {
			case <-time.After(3 * time.Second):
			case <-release:
			}
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &req)
		items := make([]map[string]any, len(req.Input))
		for i, text := range req.Input {
			items[i] = map[string]any{"index": i, "embedding": fakeVector(dim, text)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items, "usage": map[string]int{"total_tokens": 5 * len(req.Input)}})
	}))
	t.Cleanup(func() { close(release); server.Close() })

	opts := embedOptions(server.URL, dim, 64, "fake-model")
	opts.Embedding.Timeout = 200 * time.Millisecond
	opts.Embedding.MaxRetries = 2
	var logMu sync.Mutex
	var lines []string
	opts.Logf = func(format string, args ...any) {
		logMu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first := int64(bodies[0])
	mu.Unlock()

	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if status.Semantic == nil || status.Semantic.Usage == nil {
		t.Fatalf("状态应带 usage: %+v", status.Semantic)
	}
	usage := *status.Semantic.Usage
	if usage.DocumentAbandonedRequests != 1 || usage.DocumentAbandonedBytes != first || usage.DocumentRequests == 0 {
		t.Fatalf("工作区用量应记 1 次放弃、%d 字节: %+v", first, usage)
	}
	raw, _ := json.Marshal(usage)
	if !strings.Contains(string(raw), `"document_abandoned_requests":1`) || !strings.Contains(string(raw), fmt.Sprintf(`"document_abandoned_bytes":%d`, first)) {
		t.Fatalf("workspace_status 的 usage JSON 应带放弃计数: %s", raw)
	}
	total := e.ProviderUsage()
	if total.DocumentAbandonedRequests != 1 || total.DocumentAbandonedBytes != first {
		t.Fatalf("ProviderUsage 应带放弃计数: %+v", total)
	}
	if raw, _ := json.Marshal(total); !strings.Contains(string(raw), `"document_abandoned_requests":1`) {
		t.Fatalf("daemon_status 的 provider_usage JSON 应带放弃计数: %s", raw)
	}
	logMu.Lock()
	logged := strings.Join(lines, "\n")
	logMu.Unlock()
	if !strings.Contains(logged, "abandoned") || !strings.Contains(logged, fmt.Sprintf("request_bytes=%d", first)) {
		t.Fatalf("引擎日志应有一行放弃记录: %q", logged)
	}
}

// newUsageEmbedServer 是报告用量的受控 embedding 服务：usage.total_tokens =
// 5 × 条数；记录文档与查询各送审了多少条。
func newUsageEmbedServer(t *testing.T, dim int) (*httptest.Server, *int, *int) {
	t.Helper()
	docTexts, queryTexts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, text := range req.Input {
			if strings.HasPrefix(text, "This chunk is from ") {
				docTexts++
			} else {
				queryTexts++
			}
		}
		items := make([]map[string]any, len(req.Input))
		for i, text := range req.Input {
			items[i] = map[string]any{"index": i, "embedding": fakeVector(dim, text)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items, "usage": map[string]int{"total_tokens": 5 * len(req.Input)}})
	}))
	t.Cleanup(server.Close)
	return server, &docTexts, &queryTexts
}

// TestWorkspaceStatusReportsProviderUsage：构建的文档嵌入与检索的查询嵌入
// 用量按服务商报告的数记入 workspace_status 的 semantic.usage，最近一次构建
// 的 tokens 记入 last_embed_tokens；ProviderUsage 给出全部工作区合计。
func TestWorkspaceStatusReportsProviderUsage(t *testing.T) {
	const dim = 8
	server, docTexts, queryTexts := newUsageEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Search(context.Background(), searchRequest(root, "login handler")); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if status.Semantic == nil || status.Semantic.Usage == nil {
		t.Fatalf("状态应带 usage: %+v", status.Semantic)
	}
	usage := *status.Semantic.Usage
	if usage.DocumentTokens != int64(5**docTexts) || usage.DocumentRequests == 0 || usage.QueryTokens != int64(5**queryTexts) || usage.QueryRequests != 1 {
		t.Fatalf("usage 与服务商报告不符: %+v（文档 %d 条、查询 %d 条）", usage, *docTexts, *queryTexts)
	}
	if status.Semantic.LastEmbedTokens != int64(5**docTexts) {
		t.Fatalf("last_embed_tokens=%d，应为 %d", status.Semantic.LastEmbedTokens, 5**docTexts)
	}
	// 没有被放弃的请求时，状态 JSON 里不出现放弃计数的键。
	if raw, _ := json.Marshal(usage); strings.Contains(string(raw), "abandoned") {
		t.Fatalf("没有放弃时 usage JSON 不应带放弃计数: %s", raw)
	}
	total := e.ProviderUsage()
	if total.DocumentTokens != usage.DocumentTokens || total.QueryTokens != usage.QueryTokens || total.EmbeddingModel != "fake-model" || total.Since.IsZero() {
		t.Fatalf("ProviderUsage 合计不符: %+v", total)
	}
}
