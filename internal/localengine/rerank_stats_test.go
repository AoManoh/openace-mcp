package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// flakyRerankServer 第一次请求返回 500，之后按文档顺序给出递减的分数。
func flakyRerankServer(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		items := make([]map[string]any, 0, len(req.Documents))
		for i := range req.Documents {
			items = append(items, map[string]any{"index": i, "relevance_score": 1.0 / float64(i+1)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRerankAttemptsInResult：精排第一次失败、重试成功时，检索结果的阶段耗时带
// 尝试次数与重试等待；改动前只有精排总耗时，分不清是服务商慢还是在重试。
func TestRerankAttemptsInResult(t *testing.T) {
	ts := flakyRerankServer(t)
	cfg := rerankOptions(ts.URL)
	cfg.MaxRetries = 1
	e := newTestEngineWith(t, Options{Rerank: cfg})
	root := newFixtureWorkspace(t)
	result, err := e.Search(context.Background(), searchRequest(root, "login parse_config"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.RetrievalMode != "lexical+rerank" || result.Timings == nil {
		t.Fatalf("retry should still apply rerank: mode=%q timings=%+v", result.RetrievalMode, result.Timings)
	}
	if result.Timings.RerankAttempts != 2 || result.Timings.RerankRetryWaitMs <= 0 {
		t.Fatalf("timings should record 2 attempts and the retry wait: %+v", result.Timings)
	}
	if result.Timings.RerankAttemptMs > result.Timings.RerankMs {
		t.Fatalf("longest attempt cannot exceed total rerank time: %+v", result.Timings)
	}
}

// TestStatusRerankCounters：workspace_status 累计本进程内该工作区的精排调用
// 次数、重试次数与最长单次耗时。
func TestStatusRerankCounters(t *testing.T) {
	ts := flakyRerankServer(t)
	cfg := rerankOptions(ts.URL)
	cfg.MaxRetries = 1
	e := newTestEngineWith(t, Options{Rerank: cfg})
	root := newFixtureWorkspace(t)
	for i := 0; i < 2; i++ {
		if _, err := e.Search(context.Background(), searchRequest(root, "login parse_config")); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Semantic == nil || status.Semantic.RerankCalls != 2 || status.Semantic.RerankRetries != 1 {
		t.Fatalf("status should count 2 rerank calls and 1 retry: %+v", status.Semantic)
	}
}
