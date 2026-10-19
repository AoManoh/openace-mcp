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

// 降级政策：OPENACE_RETRIEVAL_DEGRADE=deny 拒绝检索类缺口（含语义覆盖不足），
// OPENACE_RERANK_DEGRADE=deny 拒绝精排类缺口（含 token 预算连一条都装不下），
// OPENACE_QUALITY_STRICT=on 拒绝任何缺口；rerank 未配置只有 strict 拒绝，
// 显式 rerank off 不算缺口。

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

// partialCoverageServer 让 util.py 的片段得到零向量而被永久拒绝，覆盖率 80%。
func partialCoverageServer(t *testing.T) *embedServer {
	s := newEmbedServer(t, 8)
	s.zeroWhen = func(text string) bool { return strings.Contains(text, "util.py") }
	return s
}

// TestRetrievalDenyRejectsPartialCoverage：覆盖不足时 retrieval deny 下 Sync、
// SyncBackground 与查询都返回写明原因与环境变量的错误；revision 照常发布，
// 状态仍是 ready；查询在发出查询嵌入之前就拒绝。
func TestRetrievalDenyRejectsPartialCoverage(t *testing.T) {
	s := partialCoverageServer(t)
	opts := embedOptions(s.ts.URL, 8, 16, "policy-model")
	opts.RetrievalDegrade = DegradeDeny
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	ctx := context.Background()

	res, err := e.Sync(ctx, syncRequest(root))
	if err == nil || !strings.Contains(err.Error(), EnvRetrievalDegrade) || !strings.Contains(err.Error(), "semantic-coverage-partial") || !strings.Contains(err.Error(), "semantic_coverage=80%") {
		t.Fatalf("deny 下覆盖不足的 Sync 应报错: res=%+v err=%v", res, err)
	}
	if res.IndexRevision == "" || res.SemanticCoverage != "80%" {
		t.Fatalf("Sync 报错时仍返回已发布 revision 的结果: %+v", res)
	}
	if _, bgErr := e.SyncBackground(ctx, syncRequest(root)); bgErr == nil || !strings.Contains(bgErr.Error(), EnvRetrievalDegrade) {
		t.Fatalf("SyncBackground 应同样拒绝: %v", bgErr)
	}
	status, err := e.WorkspaceStatus(ctx, engine.WorkspaceRef{DirectoryPath: root})
	if err != nil || status.Stage != engine.IndexStageReady || status.IndexRevision != res.IndexRevision {
		t.Fatalf("拒绝响应不得把已发布的 revision 标成失败: status=%+v err=%v", status, err)
	}

	before := s.callCount()
	_, err = e.Search(ctx, searchRequest(root, policyQuery))
	if err == nil || !strings.Contains(err.Error(), EnvRetrievalDegrade) || !strings.Contains(err.Error(), "semantic-coverage-partial") {
		t.Fatalf("deny 下覆盖不足的查询应报错: %v", err)
	}
	if n := queryEmbedCalls(s, before); n != 0 {
		t.Fatalf("覆盖不足可由 manifest 提前判定，不应发出查询嵌入，实际 %d 次", n)
	}
}

// TestRetrievalDenyAllowsLexicalWithoutSemantic：未配置语义时 deny 不因此
// 拒绝，Sync 与查询正常返回 lexical。
func TestRetrievalDenyAllowsLexicalWithoutSemantic(t *testing.T) {
	e := newTestEngineWith(t, Options{RetrievalDegrade: DegradeDeny, RerankDegrade: DegradeDeny})
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
	opts := embedOptions(s.ts.URL, 8, 16, "policy-model")
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
// rerank-unconfigured 提示；两个 deny 都不拒绝，strict 拒绝。
func TestRerankUnconfiguredOnlyStrictRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(o *Options)
		reject bool
	}{
		{"allow", func(o *Options) {}, false},
		{"deny", func(o *Options) { o.RetrievalDegrade = DegradeDeny; o.RerankDegrade = DegradeDeny }, false},
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
		func(o *Options) { o.RetrievalDegrade = DegradeDeny; o.RerankDegrade = DegradeDeny },
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
