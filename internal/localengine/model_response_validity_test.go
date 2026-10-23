package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 模型响应有效性：适配器判断响应是否有效。查询嵌入无效时语义路跑不了，
// 引擎返回错误；精排响应无效时按用户配置的精排降级策略决定返回错误还是
// 带原因继续。

const validityQuery = "HandleLogin"

// TestInvalidQueryVectorReturnsError：索引正常建好后，查询嵌入返回维度正确的
// 零向量，默认与 strict 下都返回带 query-embedding-failed(permanent) 的错误，
// 不返回纯词法结果，零向量计入查询熔断器失败。HTTP 503 对照得到 transient。
func TestInvalidQueryVectorReturnsError(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(o *Options)
		zero   bool
		reason string
	}{
		{"默认 零向量", func(o *Options) {}, true, "query-embedding-failed(permanent)"},
		{"strict 零向量", func(o *Options) { o.QualityStrict = true }, true, "query-embedding-failed(permanent)"},
		{"默认 503 对照", func(o *Options) {}, false, "query-embedding-failed(transient)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newEmbedServer(t, 8)
			opts := embedOptions(server.ts.URL, 8, 16, "validity-model")
			tc.mutate(&opts)
			t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
			e := newTestEngineWith(t, opts)
			root := newFixtureWorkspace(t)
			if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
				t.Fatal(err)
			}
			server.mu.Lock()
			if tc.zero {
				server.zeroWhen = func(text string) bool { return text == validityQuery }
			} else {
				server.failWhen = func(texts []string) bool { return len(texts) == 1 && texts[0] == validityQuery }
			}
			server.mu.Unlock()
			res, err := e.Search(context.Background(), searchRequest(root, validityQuery))
			if err == nil || !strings.Contains(err.Error(), "semantic retrieval unavailable") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("应返回带 %s 的错误且不附带结果: res=%+v err=%v", tc.reason, res, err)
			}
			if len(res.Hits) != 0 {
				t.Fatalf("报错时不得附带命中: %+v", res.Hits)
			}
			if tc.zero {
				if state := e.embedClient.QueryCircuitSnapshot().State; state == "healthy" {
					t.Fatalf("零向量应计入查询熔断器失败，实际状态 %s", state)
				}
			}
		})
	}
}

// fieldRerankServer 返回 voyage 形状的精排响应，每条按 item 构造。
func fieldRerankServer(t *testing.T, item func(index int) map[string]any) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, 0, len(req.Documents))
		for i := len(req.Documents) - 1; i >= 0; i-- {
			data = append(data, item(i))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRerankMissingScoreFollowsDegradePolicy：精排响应条数与 index 都正确但
// 没有分数字段。allow 保留融合顺序并带 rerank-skipped(permanent)；rerank deny
// 与 strict 返回错误。显式零分的合法响应照常算精排生效。
func TestRerankMissingScoreFollowsDegradePolicy(t *testing.T) {
	missing := func(index int) map[string]any { return map[string]any{"index": index} }
	zero := func(index int) map[string]any { return map[string]any{"index": index, "relevance_score": 0} }
	cases := []struct {
		name    string
		item    func(int) map[string]any
		mutate  func(o *Options)
		wantErr string
		mode    string
		reason  string
	}{
		{"allow 缺分数", missing, func(o *Options) {}, "", "hybrid", "rerank-skipped(permanent)"},
		{"rerank deny 缺分数", missing, func(o *Options) { o.RerankDegrade = DegradeDeny }, EnvRerankDegrade, "", ""},
		{"strict 缺分数", missing, func(o *Options) { o.QualityStrict = true }, EnvQualityStrict, "", ""},
		{"strict 显式零分", zero, func(o *Options) { o.QualityStrict = true }, "", "hybrid+rerank", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newEmbedServer(t, 8)
			rr := fieldRerankServer(t, tc.item)
			opts := embedOptions(server.ts.URL, 8, 16, "validity-model")
			opts.Rerank = rerankOptions(rr.URL)
			tc.mutate(&opts)
			t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
			e := newTestEngineWith(t, opts)
			root := newFixtureWorkspace(t)
			if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
				t.Fatal(err)
			}
			res, err := e.Search(context.Background(), searchRequest(root, validityQuery))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("应返回带 %s 的错误: mode=%q reason=%q err=%v", tc.wantErr, res.RetrievalMode, res.DegradedReason, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if res.RetrievalMode != tc.mode || (tc.reason != "" && !strings.Contains(res.DegradedReason, tc.reason)) || (tc.reason == "" && res.DegradedReason != "") {
				t.Fatalf("模式与原因不符: mode=%q reason=%q", res.RetrievalMode, res.DegradedReason)
			}
		})
	}
}
