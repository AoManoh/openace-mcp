package mcp

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
	"github.com/AoManoh/openace-mcp/internal/localengine"
	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// semanticFixtureGo 的 establishUserSession 是"语义命中目标"：查询与其
// 无任何 token 重叠，只有向量路能召回。
const semanticFixtureGo = `package app

// establishUserSession 建立用户会话并写入审计日志。
func establishUserSession(user string) error {
	audit(user)
	return nil
}

// audit 记录审计事件。
func audit(user string) {}
`

// lexicalMissQuery 与 fixture 无任何 token 重叠。
const lexicalMissQuery = "qqxx zzyy warpfield"

// newSemanticFakeProvider 返回确定性 embedding server：查询与目标 chunk
// 共向（e1），其余按文本 hash 生成；calls 记录请求数。
func newSemanticFakeProvider(t *testing.T, dim int, failQuery *atomic.Bool, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		isQuery := len(req.Input) == 1 && strings.Contains(req.Input[0], "qqxx")
		if isQuery && failQuery != nil && failQuery.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			vec := make([]float32, dim)
			if strings.Contains(text, "establishUserSession") || strings.Contains(text, "qqxx") {
				vec[0] = 1
			} else {
				h := fnv.New32a()
				_, _ = h.Write([]byte(text))
				seed := h.Sum32()
				for j := range vec {
					seed = seed*1664525 + 1013904223
					vec[j] = float32(seed%1000)/1000 + 0.001
				}
			}
			items = append(items, item{Embedding: vec, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// semanticOptions 的 Rerank 显式 off:本测试族演练纯语义路径(配置形态
// 意义上的 opt-out);促升裁决(2026-08-13)后缺配置(非 off)会携带
// rerank-unconfigured 提示,详见 localengine/rerank_default_notice_test.go。
func semanticOptions(url string, dim int) localengine.Options {
	return localengine.Options{Embedding: embedding.Config{
		Enabled: true, ProviderType: embedding.ProviderOpenAI, BaseURL: url,
		Model: "fake-model", Dimension: dim, BatchSize: 16, MaxConcurrency: 2,
		Timeout: 2 * time.Second, MaxRetries: 0,
	}, Rerank: rerank.Config{
		Enabled: false, ProviderType: rerank.ProviderOff,
		DisabledReason: "rerank provider is off",
	}}
}

func writeSemanticFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "session.go"), []byte(semanticFixtureGo), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# demo\nplain documentation words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSemanticHybridEndToEnd 是 Stage 3 北极星 (a) 的 MCP 面固化：
// 概念查询词法必落空、语义路命中目标；结果无降级标记且携带
// retrieval 模式可见的正确 path:line。
func TestSemanticHybridEndToEnd(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "e2e")
	const dim = 8
	var calls atomic.Int32
	provider := newSemanticFakeProvider(t, dim, nil, &calls)
	root := writeSemanticFixture(t)

	service, err := localengine.New(semanticOptions(provider.URL, dim))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	server := NewServer(service)

	sync := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sync_workspace","arguments":{"directory_path":`+jsonString(root)+`}}}`)
	if strings.Contains(sync, `"isError":true`) {
		t.Fatalf("sync 不应报错: %s", sync)
	}
	retrieval := runMCP(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":`+jsonString(lexicalMissQuery)+`,"directory_path":`+jsonString(root)+`}}}`)
	if strings.Contains(retrieval, `"isError":true`) {
		t.Fatalf("retrieval 不应报错: %s", retrieval)
	}
	for _, want := range []string{"session.go:", "establishUserSession"} {
		if !strings.Contains(retrieval, want) {
			t.Fatalf("语义路应命中词法盲区目标，缺少 %q: %s", want, retrieval)
		}
	}
	if strings.Contains(retrieval, "[DEGRADED]") {
		t.Fatalf("完整链路不得降级: %s", retrieval)
	}
	if calls.Load() == 0 {
		t.Fatal("语义路应实际调用 provider")
	}
}

// TestSemanticDegradeAllowViaMCP 是 D8(a) allow 的 MCP 面验收：
// provider 查询期故障时词法结果照常返回，首行 [DEGRADED] 横幅
// 让宿主 AI 可见降级事实（决策 11 输出契约）。
func TestSemanticDegradeAllowViaMCP(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "e2e")
	const dim = 8
	var calls atomic.Int32
	var failQuery atomic.Bool
	provider := newSemanticFakeProvider(t, dim, &failQuery, &calls)
	root := writeSemanticFixture(t)

	service, err := localengine.New(semanticOptions(provider.URL, dim))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	server := NewServer(service)
	if sync := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sync_workspace","arguments":{"directory_path":`+jsonString(root)+`}}}`); strings.Contains(sync, `"isError":true`) {
		t.Fatalf("sync 不应报错: %s", sync)
	}
	failQuery.Store(true)
	retrieval := runMCP(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"establishUserSession qqxx","directory_path":`+jsonString(root)+`}}}`)
	if strings.Contains(retrieval, `"isError":true`) {
		t.Fatalf("allow 模式不得报错: %s", retrieval)
	}
	if !strings.Contains(retrieval, `[DEGRADED] query-embedding-failed(`) || !strings.Contains(retrieval, "mode=lexical") {
		t.Fatalf("应携带 DEGRADED 横幅与模式: %s", retrieval)
	}
	if !strings.Contains(retrieval, "establishUserSession") {
		t.Fatalf("词法结果必须仍然可用: %s", retrieval)
	}
}

// TestSemanticDegradeDenyViaMCP 是 D8(a) deny 的 MCP 面验收（K33）：
// 返回可行动工具错误而非空结果。
func TestSemanticDegradeDenyViaMCP(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "e2e")
	const dim = 8
	var calls atomic.Int32
	var failQuery atomic.Bool
	provider := newSemanticFakeProvider(t, dim, &failQuery, &calls)
	root := writeSemanticFixture(t)

	opts := semanticOptions(provider.URL, dim)
	opts.RetrievalDegrade = localengine.DegradeDeny
	service, err := localengine.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	server := NewServer(service)
	if sync := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sync_workspace","arguments":{"directory_path":`+jsonString(root)+`}}}`); strings.Contains(sync, `"isError":true`) {
		t.Fatalf("sync 不应报错: %s", sync)
	}
	failQuery.Store(true)
	retrieval := runMCP(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"establishUserSession qqxx","directory_path":`+jsonString(root)+`}}}`)
	if !strings.Contains(retrieval, `"isError":true`) {
		t.Fatalf("deny 模式应返回工具错误: %s", retrieval)
	}
	if !strings.Contains(retrieval, "OPENACE_RETRIEVAL_DEGRADE") {
		t.Fatalf("错误应指明恢复路径（K33）: %s", retrieval)
	}
}

// TestSemanticOffNeverCallsProvider 是 K32 的出网面断言：env 配置了
// provider 端点但缺 key（semantic off）时，全链路零 provider 调用，
// 结果与 Stage 2 形状一致。
func TestSemanticOffNeverCallsProvider(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "e2e")
	var calls atomic.Int32
	provider := newSemanticFakeProvider(t, 8, nil, &calls)
	// voyage 类型 + 端点指向 fake + 无 key → OptionsFromEnv 判 semantic off。
	t.Setenv("OPENACE_EMBEDDING_PROVIDER", "voyage")
	t.Setenv("OPENACE_EMBEDDING_BASE_URL", provider.URL)
	t.Setenv("OPENACE_EMBEDDING_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("OPENACE_RERANK_PROVIDER", "off")
	t.Setenv("OPENACE_RETRIEVAL_DEGRADE", "")
	t.Setenv("OPENACE_RERANK_DEGRADE", "")

	opts, err := localengine.OptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	service, err := localengine.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	server := NewServer(service)
	root := writeSemanticFixture(t)
	retrieval := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"establishUserSession","directory_path":`+jsonString(root)+`}}}`)
	if strings.Contains(retrieval, `"isError":true`) || strings.Contains(retrieval, "[DEGRADED]") {
		t.Fatalf("semantic off 是完整词法能力（D1）: %s", retrieval)
	}
	if !strings.Contains(retrieval, "establishUserSession") {
		t.Fatalf("词法检索应命中: %s", retrieval)
	}
	if calls.Load() != 0 {
		t.Fatalf("semantic off 不得出网（K32）: %d", calls.Load())
	}
}
