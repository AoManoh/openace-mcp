package rerank

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/reliability"
)

// rawRerankServer 对每次请求原样返回 body。
func rawRerankServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRerankResponseRequiresIndexAndScore：三种响应形状里，index 或分数缺失、
// 为 null 时都判为无效响应（permanent），熔断器记一次失败；缺失的 index 不能
// 被当成 0，缺失的分数不能被当成 0 分。
func TestRerankResponseRequiresIndexAndScore(t *testing.T) {
	cases := map[string]struct {
		provider string
		body     string
		field    string
	}{
		"data 缺分数":          {ProviderVoyage, `{"data":[{"index":1},{"index":0}]}`, "relevance_score"},
		"data 分数为 null":     {ProviderVoyage, `{"data":[{"index":0,"relevance_score":null},{"index":1,"relevance_score":0.2}]}`, "relevance_score"},
		"data 缺 index":      {ProviderVoyage, `{"data":[{"relevance_score":0.9},{"index":1,"relevance_score":0.2}]}`, "index"},
		"data index 为 null": {ProviderVoyage, `{"data":[{"index":null,"relevance_score":0.9},{"index":1,"relevance_score":0.2}]}`, "index"},
		"results 缺分数":       {ProviderVoyage, `{"results":[{"index":1},{"index":0}]}`, "relevance_score"},
		"results 缺 index":   {ProviderVoyage, `{"results":[{"relevance_score":0.9},{"index":1,"relevance_score":0.2}]}`, "index"},
		"TEI 缺分数":           {ProviderTEI, `[{"index":0},{"index":1,"score":0.2}]`, "score"},
		"TEI 分数为 null":      {ProviderTEI, `[{"index":0,"score":null},{"index":1,"score":0.2}]`, "score"},
		"TEI 缺 index":       {ProviderTEI, `[{"score":0.9},{"index":1,"score":0.2}]`, "index"},
		"TEI index 为 null":  {ProviderTEI, `[{"index":null,"score":0.9},{"index":1,"score":0.2}]`, "index"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := rawRerankServer(t, http.StatusOK, tc.body)
			cfg := testConfig(ts.URL)
			cfg.ProviderType = tc.provider
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			fastRetry(client)
			hits, _, err := client.Rerank(context.Background(), "q", docs(2))
			callErr := asCallError(err)
			if hits != nil || callErr == nil || callErr.Class != reliability.ClassPermanent ||
				!strings.Contains(callErr.Message, "malformed rerank response") || !strings.Contains(callErr.Message, tc.field) {
				t.Fatalf("缺失或 null 的 %s 应判为无效响应: hits=%v err=%v", tc.field, hits, err)
			}
			if snapshot := client.CircuitSnapshot(); snapshot.State == "healthy" || snapshot.ConsecutiveFailures != 1 {
				t.Fatalf("熔断器应记一次失败: %+v", snapshot)
			}
		})
	}
}

// TestRerankResponseAcceptsExplicitZero：显式的 index=0 与 score=0 是合法值，
// 全部为零分时仍返回完整候选。
func TestRerankResponseAcceptsExplicitZero(t *testing.T) {
	cases := map[string]struct {
		provider string
		body     string
	}{
		"data":    {ProviderVoyage, `{"data":[{"index":0,"relevance_score":0},{"index":1,"relevance_score":0}]}`},
		"results": {ProviderVoyage, `{"results":[{"index":1,"relevance_score":0},{"index":0,"relevance_score":0}]}`},
		"TEI":     {ProviderTEI, `[{"index":0,"score":0},{"index":1,"score":0}]`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := rawRerankServer(t, http.StatusOK, tc.body)
			cfg := testConfig(ts.URL)
			cfg.ProviderType = tc.provider
			client, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			hits, sent, err := client.Rerank(context.Background(), "q", docs(2))
			if err != nil || sent != 2 || len(hits) != 2 || hits[0].Score != 0 || hits[1].Score != 0 {
				t.Fatalf("显式零值应被接受: hits=%v sent=%d err=%v", hits, sent, err)
			}
			if client.CircuitSnapshot().State != "healthy" {
				t.Fatalf("合法响应不应计失败: %+v", client.CircuitSnapshot())
			}
		})
	}
}

// TestRerank503StaysTransient：503 对照仍是 transient 类别。
func TestRerank503StaysTransient(t *testing.T) {
	ts := rawRerankServer(t, http.StatusServiceUnavailable, `{}`)
	client, err := NewClient(testConfig(ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	fastRetry(client)
	_, _, err = client.Rerank(context.Background(), "q", docs(2))
	if callErr := asCallError(err); callErr == nil || callErr.Class != reliability.ClassTransient {
		t.Fatalf("503 应为 transient: %v", err)
	}
}
