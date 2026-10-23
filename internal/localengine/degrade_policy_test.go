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
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// 降级政策：语义覆盖不足照常作答并写明原因；OPENACE_RERANK_DEGRADE=deny 拒绝
// 精排类缺口（含 token 预算连一条都装不下），OPENACE_QUALITY_STRICT=on 拒绝任何
// 缺口；rerank 未配置只有 strict 拒绝，显式 rerank off 不算缺口。语义路跑不了
// 的情况见 semantic_required_test.go。

const policyQuery = "HandleLogin"

// queryEmbedCalls 统计 fromCall 之后只含查询文本的嵌入请求数。
func queryEmbedCalls(s *embedServer, fromCall int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := fromCall; i < len(s.perCall); i++ {
		if len(s.perCall[i]) == 1 && s.perCall[i][0] == policyQuery {
			n++
		}
	}
	return n
}

// partialCoverageServer 让 util.py 的片段（parse_config）嵌入请求暂时失败，
// 构建发布部分覆盖的 revision；调用方用批大小 1，只有这些片段缺向量。被
// provider 永久拒绝（零向量）的片段不计入覆盖率分母，不能用来制造缺口。
func partialCoverageServer(t *testing.T) *embedServer {
	s := newEmbedServer(t, 8)
	s.setFailWhen(func(texts []string) bool {
		for _, text := range texts {
			if strings.Contains(text, "parse_config") {
				return true
			}
		}
		return false
	})
	return s
}

// TestPartialCoverageAnsweredWithReason：部分 chunk 嵌入失败、覆盖不完整时，
// Sync、SyncBackground 与查询都照常返回并写明 semantic-coverage-partial；查询
// 走 hybrid，正文开头与 uncovered_files 列出缺向量的文件，状态为 ready。
func TestPartialCoverageAnsweredWithReason(t *testing.T) {
	s := partialCoverageServer(t)
	e := newTestEngineWith(t, embedOptions(s.ts.URL, 8, 1, "policy-model"))
	root := newFixtureWorkspace(t)
	ctx := context.Background()

	res, err := e.Sync(ctx, syncRequest(root))
	if err != nil || res.SemanticCoverage == "" || res.SemanticCoverage == "100%" || !strings.Contains(res.DegradedReason, "semantic-coverage-partial") {
		t.Fatalf("覆盖不足的 Sync 应照常返回并写明原因: res=%+v err=%v", res, err)
	}
	if _, bgErr := e.SyncBackground(ctx, syncRequest(root)); bgErr != nil {
		t.Fatalf("SyncBackground 应照常返回: %v", bgErr)
	}
	status, err := e.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || status.Stage != engine.IndexStageReady || status.IndexRevision != res.IndexRevision {
		t.Fatalf("状态应为 ready: status=%+v err=%v", status, err)
	}
	got, err := e.Search(ctx, searchRequest(root, policyQuery))
	if err != nil || got.RetrievalMode != "hybrid" || !strings.Contains(got.DegradedReason, "semantic-coverage-partial") {
		t.Fatalf("覆盖不足的查询应走 hybrid 并写明原因: %+v err=%v", got, err)
	}
	if !containsString(got.UncoveredFiles, "util.py") || !strings.Contains(got.Text, "[COVERAGE] semantic search does not cover") {
		t.Fatalf("应列出缺向量的文件: uncovered=%v text=%q", got.UncoveredFiles, got.Text)
	}
}

// TestLexicalOnlyEngineAnswersNormally：未配置语义时，即使开了精排 deny，
// Sync 与查询也正常返回 lexical，没有任何原因。
func TestLexicalOnlyEngineAnswersNormally(t *testing.T) {
	e := newTestEngineWith(t, Options{RerankDegrade: DegradeDeny})
	root := newFixtureWorkspace(t)
	res, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil || res.RetrievalMode != "lexical" {
		t.Fatalf("纯词法 Sync 应成功: %+v err=%v", res, err)
	}
	got, err := e.Search(context.Background(), searchRequest(root, policyQuery))
	if err != nil || got.RetrievalMode != "lexical" || got.DegradedReason != "" {
		t.Fatalf("纯词法查询应成功: %+v err=%v", got, err)
	}
}

// TestStrictRejectsPartialCoverageEarly：strict 下覆盖不足同样在查询嵌入之前
// 拒绝，Sync 也报错。
func TestStrictRejectsPartialCoverageEarly(t *testing.T) {
	s := partialCoverageServer(t)
	opts := embedOptions(s.ts.URL, 8, 1, "policy-model")
	opts.QualityStrict = true
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err == nil || !strings.Contains(err.Error(), EnvQualityStrict) {
		t.Fatalf("strict 下覆盖不足的 Sync 应报错: %v", err)
	}
	before := s.callCount()
	_, err := e.Search(context.Background(), searchRequest(root, policyQuery))
	if err == nil || !strings.Contains(err.Error(), EnvQualityStrict) || !strings.Contains(err.Error(), "semantic-coverage-partial") || queryEmbedCalls(s, before) != 0 {
		t.Fatalf("strict 应在查询嵌入前拒绝: err=%v query_embeds=%d", err, queryEmbedCalls(s, before))
	}
}

// countingRerankServer 是计数的 voyage 形状精排服务，按送审顺序给递减分数。
func countingRerankServer(t *testing.T) (*httptest.Server, *int64) {
	var calls int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, 0, len(req.Documents))
		for i := range req.Documents {
			data = append(data, map[string]any{"index": i, "relevance_score": 1.0 / float64(i+1)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(ts.Close)
	return ts, &calls
}

// TestRerankDenyRejectsZeroTokenBudget：token 预算连第一条候选都装不下时，
// allow 保留融合顺序并带 rerank-skipped(token-budget)；rerank deny 返回写明
// OPENACE_RERANK_DEGRADE 与 OPENACE_RERANK_MAX_TOKENS 的错误。精排请求都是 0 次。
func TestRerankDenyRejectsZeroTokenBudget(t *testing.T) {
	for _, deny := range []bool{false, true} {
		s := newEmbedServer(t, 8)
		rr, calls := countingRerankServer(t)
		opts := embedOptions(s.ts.URL, 8, 16, "policy-model")
		opts.Rerank = rerankOptions(rr.URL)
		opts.Rerank.MaxTokens = 1
		if deny {
			opts.RerankDegrade = DegradeDeny
		}
		e := newTestEngineWith(t, opts)
		root := newFixtureWorkspace(t)
		res, err := e.Search(context.Background(), searchRequest(root, policyQuery))
		if deny {
			if err == nil || !strings.Contains(err.Error(), EnvRerankDegrade) || !strings.Contains(err.Error(), rerank.EnvMaxTokens) {
				t.Fatalf("rerank deny 应拒绝 token-budget: %v", err)
			}
		} else if err != nil || !strings.Contains(res.DegradedReason, "rerank-skipped(token-budget)") || res.RetrievalMode != "hybrid" {
			t.Fatalf("allow 应保留融合顺序并带原因: %+v err=%v", res, err)
		}
		if atomic.LoadInt64(calls) != 0 {
			t.Fatalf("预算为 0 时不应发出精排请求，实际 %d 次", atomic.LoadInt64(calls))
		}
	}
}

// TestRerankDenyAcceptsPartialBudget：预算至少容纳一条候选时是有效精排，
// rerank deny 不把它当作失败。
func TestRerankDenyAcceptsPartialBudget(t *testing.T) {
	for _, budget := range []int{60, 100, 150, 250, 400} {
		s := newEmbedServer(t, 8)
		rr, _ := countingRerankServer(t)
		opts := embedOptions(s.ts.URL, 8, 16, "policy-model")
		opts.Rerank = rerankOptions(rr.URL)
		opts.Rerank.MaxTokens = budget
		opts.RerankDegrade = DegradeDeny
		e := newTestEngineWith(t, opts)
		root := newFixtureWorkspace(t)
		res, err := e.Search(context.Background(), searchRequest(root, policyQuery))
		if err != nil && strings.Contains(err.Error(), "rerank-skipped(token-budget)") {
			// 这个预算连第一条都装不下，属于上一个测试覆盖的零预算情况，
			// rerank deny 拒绝是正确行为；继续试更大的预算。
			continue
		}
		if err != nil {
			t.Fatalf("budget=%d：部分精排不应被拒绝: %v", budget, err)
		}
		if res.RerankSent >= 1 && res.RerankSent < len(res.Hits) {
			if res.RetrievalMode != "hybrid+rerank" || strings.Contains(res.DegradedReason, "rerank") {
				t.Fatalf("部分精排应算生效且没有精排原因: %+v", res)
			}
			t.Logf("budget=%d rerank_sent=%d hits=%d", budget, res.RerankSent, len(res.Hits))
			return
		}
	}
	t.Fatal("没有找到只容纳部分候选的预算，测试前置不成立")
}

// TestRerankUnconfiguredOnlyStrictRejects：配置了语义、没有配置精排时带
// rerank-unconfigured 提示；精排 deny 不拒绝，strict 拒绝。
func TestRerankUnconfiguredOnlyStrictRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(o *Options)
		reject bool
	}{
		{"allow", func(o *Options) {}, false},
		{"rerank-deny", func(o *Options) { o.RerankDegrade = DegradeDeny }, false},
		{"strict", func(o *Options) { o.QualityStrict = true }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newEmbedServer(t, 8)
			opts := embedOptions(s.ts.URL, 8, 16, "policy-model")
			opts.Rerank = rerank.Config{}
			tc.mutate(&opts)
			e := newTestEngineWith(t, opts)
			root := newFixtureWorkspace(t)
			res, err := e.Search(context.Background(), searchRequest(root, policyQuery))
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), EnvQualityStrict) || !strings.Contains(err.Error(), "rerank-unconfigured") {
					t.Fatalf("strict 应拒绝 rerank-unconfigured: %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(res.DegradedReason, "rerank-unconfigured") {
				t.Fatalf("未配置精排只提示不拒绝: %+v err=%v", res, err)
			}
		})
	}
}

// TestRerankExplicitOffIsNotAGap：显式 rerank off 是用户选择，deny 与 strict
// 都不要求精排。
func TestRerankExplicitOffIsNotAGap(t *testing.T) {
	for _, mutate := range []func(o *Options){
		func(o *Options) { o.RerankDegrade = DegradeDeny },
		func(o *Options) { o.QualityStrict = true },
	} {
		s := newEmbedServer(t, 8)
		opts := embedOptions(s.ts.URL, 8, 16, "policy-model")
		mutate(&opts)
		e := newTestEngineWith(t, opts)
		root := newFixtureWorkspace(t)
		res, err := e.Search(context.Background(), searchRequest(root, policyQuery))
		if err != nil || res.DegradedReason != "" || res.RetrievalMode != "hybrid" {
			t.Fatalf("显式 rerank off 不应产生缺口: %+v err=%v", res, err)
		}
	}
}
