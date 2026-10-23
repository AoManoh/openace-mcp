package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestStatusReportsEmbedRequestStats：构建的嵌入阶段里第一个文档请求收到 429、
// 重试成功后，workspace_status 给出这次嵌入阶段的请求尝试数（含重试）与 429
// 次数；构建慢时据此区分请求多、被限速还是单个请求慢。
func TestStatusReportsEmbedRequestStats(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	const dim = 8
	var documents atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > 0 && strings.HasPrefix(req.Input[0], docTextPrefix) && documents.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			items = append(items, item{Embedding: fakeVector(dim, text), Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	opts := embedOptions(ts.URL, dim, 64, "stats")
	opts.Embedding.MaxRetries = 1
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("429 后重试成功，同步应成功: %v", err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || status.Semantic == nil {
		t.Fatalf("workspace_status: %+v err=%v", status, err)
	}
	s := status.Semantic
	if s.LastEmbedRequests != 2 || s.LastEmbedRateLimited != 1 || s.Coverage != "100%" || !s.GovernorRateLearning {
		t.Fatalf("嵌入阶段计数不符（期望请求 2 次、429 1 次、覆盖 100%%）: %+v", s)
	}
}
