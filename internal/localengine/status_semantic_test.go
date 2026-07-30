package localengine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/embedding"
)

// TestStatusSemanticDisabledReason 是 D1：配置了 provider 但缺 key 时，
// 状态以一行说明原因，词法照常。
func TestStatusSemanticDisabledReason(t *testing.T) {
	opts := Options{Embedding: embedding.Config{
		Enabled: false, ProviderType: embedding.ProviderVoyage,
		DisabledReason: "embedding provider \"voyage\" has no API key; semantic path disabled, lexical retrieval fully available",
	}}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	if status.Semantic == nil || status.Semantic.Enabled {
		t.Fatalf("应挂接未启用的语义块: %+v", status.Semantic)
	}
	if !strings.Contains(status.Semantic.DisabledReason, "no API key") {
		t.Fatalf("原因应说明缺 key: %q", status.Semantic.DisabledReason)
	}
	if status.Stage != "ready" {
		t.Fatalf("词法应照常 ready: %s", status.Stage)
	}
}

// TestStatusSemanticCoverageAndCircuit 是 T08 业务验收的四个运营问题：
// 覆盖多少、provider 状态、多久恢复、当前模式可推断。
func TestStatusSemanticCoverageAndCircuit(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	server.zeroWhen = func(text string) bool { return strings.Contains(text, "parse_config") }
	e := newTestEngineWith(t, embedOptions(server.ts.URL, dim, 16, "fake-model"))
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	semantic := status.Semantic
	if semantic == nil || !semantic.Enabled {
		t.Fatalf("语义块缺失: %+v", status)
	}
	if semantic.Provider != "openai" || semantic.Model != "fake-model" || semantic.Dimension != dim {
		t.Fatalf("provider 身份不符: %+v", semantic)
	}
	if semantic.CoveredChunks != semantic.TotalChunks-1 || semantic.RejectedChunks != 1 {
		t.Fatalf("覆盖/拒绝计数不符（K31/K35）: %+v", semantic)
	}
	if semantic.Coverage == "" || semantic.Coverage == "100%" {
		t.Fatalf("覆盖率应部分: %q", semantic.Coverage)
	}
	if semantic.ProviderState != "healthy" {
		t.Fatalf("零向量拒绝不是 circuit 失败: %q", semantic.ProviderState)
	}
}

// TestStatusBackoffVisibility：provider 故障后状态给出退避与脱敏错误。
func TestStatusBackoffVisibility(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	server.setFailWhen(func([]string) bool { return true })
	opts := embedOptions(server.ts.URL, dim, 16, "fake-model")
	opts.Embedding.APIKey = "canary-status-key"
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	semantic := status.Semantic
	if semantic == nil || semantic.ProviderState != "backoff" {
		t.Fatalf("应处于退避: %+v", semantic)
	}
	if semantic.BackoffUntil == nil || !semantic.BackoffUntil.After(time.Now()) {
		t.Fatalf("应给出恢复时间: %+v", semantic.BackoffUntil)
	}
	if semantic.LastError == "" || strings.Contains(semantic.LastError, "canary-status-key") {
		t.Fatalf("错误应存在且脱敏（K21）: %q", semantic.LastError)
	}
	if semantic.Coverage != "0%" {
		t.Fatalf("零覆盖应如实: %q", semantic.Coverage)
	}
}

// TestStatusRerankVisibility：精排 provider 状态可见。
func TestStatusRerankVisibility(t *testing.T) {
	ts := newRerankServer(t, func(string) float64 { return 0.5 }, nil)
	e := newTestEngineWith(t, Options{Rerank: rerankOptions(ts.URL)})
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	if status.Semantic == nil || status.Semantic.RerankProvider == "" || status.Semantic.RerankState != "healthy" {
		t.Fatalf("精排状态缺失: %+v", status.Semantic)
	}
	if status.Semantic.Enabled {
		t.Fatalf("embedding 未配置时 Enabled 应为 false")
	}
}

// TestStatusStage2WireUnchanged 是 K32/K34：零配置时状态 JSON 无 semantic 键。
func TestStatusStage2WireUnchanged(t *testing.T) {
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\"semantic\"") {
		t.Fatalf("零配置状态不得含 semantic 键（K32）: %s", raw)
	}
}

// TestStatusColdStartCoverage：冷启动（无内存状态）从 manifest 恢复覆盖视图。
func TestStatusColdStartCoverage(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)
	opts := embedOptions(server.ts.URL, dim, 16, "fake-model")

	warm, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := warm.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	_ = warm.Close(context.Background())

	cold, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cold.Close(context.Background()) })
	status, err := cold.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil {
		t.Fatal(err)
	}
	if status.Semantic == nil || status.Semantic.Coverage != "100%" || status.Semantic.CoveredChunks == 0 {
		t.Fatalf("冷启动应从 manifest 恢复覆盖: %+v", status.Semantic)
	}
}
