package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

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
	total := e.ProviderUsage()
	if total.DocumentTokens != usage.DocumentTokens || total.QueryTokens != usage.QueryTokens || total.EmbeddingModel != "fake-model" || total.Since.IsZero() {
		t.Fatalf("ProviderUsage 合计不符: %+v", total)
	}
}
