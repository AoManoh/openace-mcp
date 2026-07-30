package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/rerank"
	"github.com/AoManoh/openace-mcp/internal/vector"
)

// magicEmbedServer 在 embedServer 基础上支持定向向量：查询与目标 chunk
// 共向（e1），其余按文本 hash 生成——构造"词法必落空、语义必命中"。
func newMagicEmbedServer(t *testing.T, dim int, magicWhen func(text string) bool) *embedServer {
	t.Helper()
	server := &embedServer{dim: dim}
	server.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		server.mu.Lock()
		server.calls++
		server.perCall = append(server.perCall, req.Input)
		fail := server.failWhen != nil && server.failWhen(req.Input)
		server.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			var vec []float32
			if magicWhen(text) {
				vec = make([]float32, dim)
				vec[0] = 1
			} else {
				vec = fakeVector(dim, text)
			}
			items = append(items, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(server.ts.Close)
	return server
}

// lexicalMissQuery 与 fixture 无任何 token 重叠。
const lexicalMissQuery = "qqxx zzyy warpfield"

// TestHybridHitsSemanticOnlyTarget 是北极星 (a) 的引擎级形态：
// 概念查询词法落空、语义命中目标 chunk。
func TestHybridHitsSemanticOnlyTarget(t *testing.T) {
	const dim = 8
	server := newMagicEmbedServer(t, dim, func(text string) bool {
		return strings.Contains(text, "establishSession") || strings.Contains(text, "qqxx")
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)

	result, err := e.Search(context.Background(), searchRequest(root, lexicalMissQuery))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(result.Text, "establishSession") || !strings.Contains(result.Text, "main.go") {
		t.Fatalf("语义路应命中词法盲区目标: %q", result.Text)
	}
	if result.RetrievalMode != "hybrid" {
		t.Fatalf("mode 应为 hybrid: %q", result.RetrievalMode)
	}
	if result.DegradedReason != "" || strings.Contains(result.Text, "[DEGRADED]") {
		t.Fatalf("完整链路不应降级: %+v", result)
	}
	if result.SemanticCoverage != "100%" {
		t.Fatalf("覆盖应为 100%%: %q", result.SemanticCoverage)
	}
}

// TestQueryEmbeddingFailureAllowDegradesToLexical 是 D8(a) allow 路径。
func TestQueryEmbeddingFailureAllowDegradesToLexical(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// 全覆盖后仅查询 embedding 失败。
	server.setFailWhen(func(texts []string) bool {
		return len(texts) == 1 && strings.Contains(texts[0], "HandleLogin-query")
	})
	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin-query HandleLogin"))
	if err != nil {
		t.Fatalf("allow 模式不得报错: %v", err)
	}
	if !strings.HasPrefix(result.Text, "[DEGRADED] query-embedding-failed(") {
		t.Fatalf("首行应为 DEGRADED 横幅: %q", result.Text)
	}
	if result.RetrievalMode != "lexical" || !strings.Contains(result.DegradedReason, "query-embedding-failed") {
		t.Fatalf("透明性字段不符: %+v", result)
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("词法结果必须仍然可用（核心理念 3）: %q", result.Text)
	}
	if !strings.Contains(result.Text, "mode=lexical") || !strings.Contains(result.Text, "semantic_coverage=100%") {
		t.Fatalf("横幅应含 mode 与 coverage: %q", strings.SplitN(result.Text, "\n", 2)[0])
	}
}

// TestQueryEmbeddingFailureDenyErrors 是 D8(a) deny 路径（K33）。
func TestQueryEmbeddingFailureDenyErrors(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	opts := embedOptions(server.ts.URL, dim, 16, "fake-model")
	opts.RetrievalDegrade = DegradeDeny
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	server.setFailWhen(func(texts []string) bool { return len(texts) == 1 })
	_, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err == nil {
		t.Fatalf("deny 模式应报错")
	}
	message := err.Error()
	if !strings.Contains(message, "OPENACE_RETRIEVAL_DEGRADE") || !strings.Contains(message, "allow") {
		t.Fatalf("错误应指明恢复路径（K33）: %v", err)
	}
}

// TestCoveragePartialBanneredHybrid 是 D8(c)：hybrid 正常执行但覆盖不完整。
func TestCoveragePartialBanneredHybrid(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	// util.py 内容永远返回零向量（K35 拒绝）→ 覆盖缺口但 circuit 健康。
	server.zeroWhen = func(text string) bool { return strings.Contains(text, "parse_config") }
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)

	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.RetrievalMode != "hybrid" {
		t.Fatalf("向量部分可用时应仍走 hybrid: %+v", result)
	}
	if !strings.Contains(result.DegradedReason, "semantic-coverage-partial") ||
		result.SemanticCoverage == "100%" || result.SemanticCoverage == "" {
		t.Fatalf("覆盖缺口必须如实上报（决策 11 第三层）: %+v", result)
	}
	if !strings.Contains(result.Text, "[DEGRADED]") || !strings.Contains(result.Text, "semantic_coverage=") {
		t.Fatalf("横幅应携带覆盖率: %q", strings.SplitN(result.Text, "\n", 2)[0])
	}
}

// rerankServer 构造可编程 fake rerank provider（voyage 形状）。
func newRerankServer(t *testing.T, scoreFor func(doc string) float64, fail *bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && *fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		items := make([]item, 0, len(req.Documents))
		for i, doc := range req.Documents {
			items = append(items, item{Index: i, RelevanceScore: scoreFor(doc)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func rerankOptions(url string) rerank.Config {
	return rerank.Config{
		Enabled: true, ProviderType: rerank.ProviderVoyage, BaseURL: url,
		APIKey: "fake-key", Model: "fake-rerank", MaxTokens: 200000,
		Timeout: 2 * time.Second, MaxRetries: 0,
	}
}

// TestRerankReordersLexicalHead 是 lexical+rerank 组合（semantic off）。
func TestRerankReordersLexicalHead(t *testing.T) {
	ts := newRerankServer(t, func(doc string) float64 {
		if strings.Contains(doc, "util.py") {
			return 0.99
		}
		return 0.1
	}, nil)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	root := newFixtureWorkspace(t)

	// "parse_config" 词法命中 util.py；"login" 命中 main.go/README。
	result, err := e.Search(context.Background(), searchRequest(root, "login parse_config"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if result.RetrievalMode != "lexical+rerank" {
		t.Fatalf("mode 应为 lexical+rerank: %q", result.RetrievalMode)
	}
	firstBlock := strings.SplitN(result.Text, "\n", 2)[0]
	if !strings.Contains(firstBlock, "util.py") {
		t.Fatalf("精排应把 util.py 提到首位: %q", firstBlock)
	}
	if result.DegradedReason != "" || strings.Contains(result.Text, "[DEGRADED]") {
		t.Fatalf("正常精排不是降级: %+v", result)
	}
}

// TestRerankFailureAllowKeepsOrderAndBanners 是 D8(b) allow：
// 候选集与顺序完整保留，仅标记 rerank-skipped。
func TestRerankFailureAllowKeepsOrderAndBanners(t *testing.T) {
	fail := true
	ts := newRerankServer(t, func(string) float64 { return 0 }, &fail)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	root := newFixtureWorkspace(t)

	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("allow 模式不得报错: %v", err)
	}
	if !strings.Contains(result.DegradedReason, "rerank-skipped") ||
		!strings.HasPrefix(result.Text, "[DEGRADED] rerank-skipped(") {
		t.Fatalf("应标记 rerank-skipped: %+v", result)
	}
	if result.RetrievalMode != "lexical" {
		t.Fatalf("失败时 mode 不得声称 rerank: %q", result.RetrievalMode)
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("候选不得丢失: %q", result.Text)
	}
}

// TestRerankTokenBudgetKeepsTailComplete 是 K28 截断语义 + 自审对齐修复：
// 送审数被 token 预算截到 sent<head 时，候选集不重复、不丢失。
func TestRerankTokenBudgetKeepsTailComplete(t *testing.T) {
	ts := newRerankServer(t, func(doc string) float64 {
		if strings.Contains(doc, "util.py") {
			return 0.99
		}
		return 0.1
	}, nil)
	query := "login parse_config demo application"

	baseline := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	baseResult, err := baseline.Search(context.Background(), searchRequest(root, query))
	if err != nil {
		t.Fatal(err)
	}
	baseHeaders := headerSet(t, baseResult.Text)
	if len(baseHeaders) < 2 {
		t.Fatalf("基线应命中多个块: %v", baseHeaders)
	}

	cfg := rerankOptions(ts.URL)
	cfg.MaxTokens = 40 // 只够送审 1-2 个文档
	reranked := newTestEngineWith(t, Options{Rerank: cfg})
	rerankResult, err := reranked.Search(context.Background(), searchRequest(root, query))
	if err != nil {
		t.Fatal(err)
	}
	if rerankResult.RetrievalMode != "lexical+rerank" {
		t.Fatalf("应实际执行精排: %+v", rerankResult)
	}
	gotHeaders := headerSet(t, rerankResult.Text)
	if len(gotHeaders) != len(baseHeaders) {
		t.Fatalf("截断精排不得增删候选（K28）: base=%v got=%v", baseHeaders, gotHeaders)
	}
	for header := range baseHeaders {
		if !gotHeaders[header] {
			t.Fatalf("候选 %q 丢失: %v", header, gotHeaders)
		}
	}
}

// headerSet 收集渲染文本中的 `## path:span` 头并断言无重复。
func headerSet(t *testing.T, text string) map[string]bool {
	t.Helper()
	headers := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			if headers[line] {
				t.Fatalf("渲染出现重复块: %q", line)
			}
			headers[line] = true
		}
	}
	return headers
}

// TestRerankFailureDenyErrors 是 D8(b) deny。
func TestRerankFailureDenyErrors(t *testing.T) {
	fail := true
	ts := newRerankServer(t, func(string) float64 { return 0 }, &fail)
	opts := Options{Rerank: rerankOptions(ts.URL), RerankDegrade: DegradeDeny}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	_, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err == nil || !strings.Contains(err.Error(), "OPENACE_RERANK_DEGRADE") {
		t.Fatalf("deny 应报错并指明恢复路径: %v", err)
	}
}

// TestVectorCorruptionDegradesThenSelfHeals 是暗坑 K25 的端到端闭环。
func TestVectorCorruptionDegradesThenSelfHeals(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	manifest, store := loadActiveManifest(t, e, root)
	dataPath := filepath.Join(store.SegmentPath(manifest), vector.DataFileName)
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xFF
	if err := os.WriteFile(dataPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// 查询：词法照常 + 显式降级 + 登记自愈。
	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("向量损坏不得影响词法可用性（K25）: %v", err)
	}
	if !strings.Contains(result.DegradedReason, "vector-data-unavailable") || result.RetrievalMode != "lexical" {
		t.Fatalf("应显式降级: %+v", result)
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("词法结果应可用: %q", result.Text)
	}

	// 下一次 sync 自愈：发布新 revision 且覆盖完整。
	healed, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if healed.IndexRevision == manifest.Revision {
		t.Fatalf("自愈应发布新 revision")
	}
	after, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil || after.DegradedReason != "" || after.RetrievalMode != "hybrid" {
		t.Fatalf("自愈后应恢复 hybrid: %+v err=%v", after, err)
	}
}

// TestStaleIndexServedOnSyncFailure 是 D8(d)/review S23。
func TestStaleIndexServedOnSyncFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受权限位约束，无法注入扫描失败")
	}
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	// 修改文件内容并去掉读权限：扫描 hash 读取失败 → sync 失败。
	target := filepath.Join(root, "util.py")
	writeFixture(t, root, "util.py", fixtureUtilPy+"\n# changed\n")
	if err := os.Chmod(target, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o600) })

	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatalf("allow 模式应以旧索引服务: %v", err)
	}
	if !strings.HasPrefix(result.Text, "[DEGRADED] stale-index; mode=lexical") {
		t.Fatalf("应显式标记 stale-index: %q", strings.SplitN(result.Text, "\n", 2)[0])
	}
	if !strings.Contains(result.Text, "HandleLogin") {
		t.Fatalf("旧索引结果应可用: %q", result.Text)
	}
	if result.DegradedReason != "stale-index" || result.RetrievalMode != "lexical" {
		t.Fatalf("透明性字段不符: %+v", result)
	}
}

// TestStaleIndexDeniedWhenConfigured 是 D8(d) deny。
func TestStaleIndexDeniedWhenConfigured(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受权限位约束，无法注入扫描失败")
	}
	e := newTestEngineWith(t, Options{RetrievalDegrade: DegradeDeny})
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "util.py")
	writeFixture(t, root, "util.py", fixtureUtilPy+"\n# changed\n")
	if err := os.Chmod(target, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o600) })
	_, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err == nil || !strings.Contains(err.Error(), "OPENACE_RETRIEVAL_DEGRADE") {
		t.Fatalf("deny 应报错并指明恢复路径: %v", err)
	}
}

// TestLexicalOnlyFieldsStayEmpty 是 K32：providers 未配置时透明性字段
// 与文本均与 Stage 2 一致。
func TestLexicalOnlyFieldsStayEmpty(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if err != nil {
		t.Fatal(err)
	}
	if result.RetrievalMode != "" || result.DegradedReason != "" || result.SemanticCoverage != "" {
		t.Fatalf("纯词法正常路径不得填充新字段（K32/K34）: %+v", result)
	}
	if strings.Contains(result.Text, "[DEGRADED]") {
		t.Fatalf("不得出现横幅: %q", result.Text)
	}
}
