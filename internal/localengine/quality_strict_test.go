package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 方案④(2026-08-02 批准):quality-strict profile 与可机读质量字段。

// TestQualityFieldsOnHappyPath:hybrid+rerank 正常路径出参三新字段。
func TestQualityFieldsOnHappyPath(t *testing.T) {
	const dim = 8
	es := newEmbedServer(t, dim)
	defer es.ts.Close()
	rs := newRerankServer(t, func(doc string) float64 {
		if strings.Contains(doc, "HandleLogin") {
			return 0.9
		}
		return 0.1
	}, nil)
	defer rs.Close()
	opts := embedOptions(es.ts.URL, dim, 16, "fake-model")
	opts.Rerank = rerankOptions(rs.URL)
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RetrievalMode != "hybrid+rerank" {
		t.Fatalf("前置:应为 hybrid+rerank: %+v", res)
	}
	if res.RerankSent <= 0 {
		t.Fatalf("rerank_sent 应大于 0: %+v", res)
	}
	if res.QueryEmbedFailed {
		t.Fatalf("query_embed_failed 应为 false: %+v", res)
	}
	if !strings.Contains(res.EmbeddingProfile, "fake-model") || !strings.Contains(res.EmbeddingProfile, "8") {
		t.Fatalf("embedding_profile 应含模型与维度: %q", res.EmbeddingProfile)
	}
}

// TestQueryEmbedFailedFieldAndStrict:查询嵌入失败——allow 下降级并置位
// 字段;strict 下显式报错并指名 env。
func TestQueryEmbedFailedFieldAndStrict(t *testing.T) {
	const dim = 8
	es := newEmbedServer(t, dim)
	defer es.ts.Close()
	opts := embedOptions(es.ts.URL, dim, 16, "fake-model")
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}}); err != nil {
		t.Fatal(err)
	}
	// 构建完成后再让查询嵌入(单文本调用)失败。
	es.mu.Lock()
	es.failWhen = func(texts []string) bool { return len(texts) == 1 }
	es.mu.Unlock()

	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("allow 下应降级放行: %v", err)
	}
	if !res.QueryEmbedFailed {
		t.Fatalf("query_embed_failed 应置位: %+v", res)
	}

	strict := embedOptions(es.ts.URL, dim, 16, "fake-model")
	strict.QualityStrict = true
	es2 := newTestEngineWith(t, strict)
	if _, err := es2.Search(context.Background(), searchRequest(root, "HandleLogin")); err == nil {
		t.Fatal("strict 下查询嵌入失败应显式报错")
	} else if !strings.Contains(err.Error(), EnvQualityStrict) || !strings.Contains(err.Error(), "query-embedding-failed") {
		t.Fatalf("错误应指名 env 与缺口 token: %v", err)
	}
}

// TestStrictRequiresRerankApplied:配置了 rerank 但 provider 故障——
// allow 降级;strict 报错。
func TestStrictRequiresRerankApplied(t *testing.T) {
	const dim = 8
	es := newEmbedServer(t, dim)
	defer es.ts.Close()
	fail := true
	rs := newRerankServer(t, func(string) float64 { return 0.5 }, &fail)
	defer rs.Close()
	opts := embedOptions(es.ts.URL, dim, 16, "fake-model")
	opts.Rerank = rerankOptions(rs.URL)
	opts.QualityStrict = true
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Search(context.Background(), searchRequest(root, "HandleLogin")); err == nil {
		t.Fatal("strict 下 rerank 未生效应显式报错")
	} else if !strings.Contains(err.Error(), EnvQualityStrict) {
		t.Fatalf("错误应指名 env: %v", err)
	}
	// 同配置 strict off:降级放行且 reason 可见。
	loose := embedOptions(es.ts.URL, dim, 16, "fake-model")
	loose.Rerank = rerankOptions(rs.URL)
	e2 := newTestEngineWith(t, loose)
	res, err := e2.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("allow 下应放行: %v", err)
	}
	if !strings.Contains(res.DegradedReason, "rerank-skipped") {
		t.Fatalf("降级原因应可见: %+v", res)
	}
}

// TestStrictHealthyPasses:strict 下全链路健康应正常返回。
func TestStrictHealthyPasses(t *testing.T) {
	const dim = 8
	es := newEmbedServer(t, dim)
	defer es.ts.Close()
	rs := newRerankServer(t, func(string) float64 { return 0.5 }, nil)
	defer rs.Close()
	opts := embedOptions(es.ts.URL, dim, 16, "fake-model")
	opts.Rerank = rerankOptions(rs.URL)
	opts.QualityStrict = true
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	res, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("strict 健康路径应通过: %v", err)
	}
	if res.RetrievalMode != "hybrid+rerank" || res.SemanticCoverage != "100%" {
		t.Fatalf("前置断言: %+v", res)
	}
}

// TestStrictRequiresSemanticConfig:strict 无语义 provider = 配置错误。
func TestStrictRequiresSemanticConfig(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	_, err := New(Options{QualityStrict: true})
	if err == nil || !strings.Contains(err.Error(), EnvQualityStrict) {
		t.Fatalf("strict 无 embedding 配置应在构造期报错: %v", err)
	}
}
