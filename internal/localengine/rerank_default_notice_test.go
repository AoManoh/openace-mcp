package localengine

import (
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/rerank"
)

// 本文件回归 2026-08-13 用户裁决"rerank 生产默认(质量至上)":
// 语义链路已配置而 rerank 未配置时,检索结果必须显式提示(降级 reason
// 进横幅),不得静默按 RRF 序放行;OPENACE_QUALITY_STRICT=on 时升级为
// 显式报错(既有 strict 闸自动覆盖 reason)。显式 OPENACE_RERANK_PROVIDER
// =off 是配置形态而非缺口(config.go:Enabled=false 语义),不提示;
// 纯词法零凭据路径不受影响(红线:词法路永远可用,不标 DEGRADED)。

// TestSemanticWithoutRerankEmitsUnconfiguredNotice:语义 on + rerank
// 缺 key → reason 必须含 rerank-unconfigured 且进入 DegradedReason。
func TestSemanticWithoutRerankEmitsUnconfiguredNotice(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "notice-model")
	opts.Rerank = rerank.Config{
		Enabled:        false,
		ProviderType:   rerank.ProviderVoyage,
		DisabledReason: "rerank provider \"voyage\" has no API key",
	}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)

	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(result.DegradedReason, "rerank-unconfigured") {
		t.Fatalf("语义 on + rerank 未配置必须显式提示(质量至上裁决),实际 degraded_reason=%q", result.DegradedReason)
	}
}

// TestSemanticWithRerankOffStaysClean:显式 provider=off 是配置形态,
// 不产生 rerank-unconfigured 提示。
func TestSemanticWithRerankOffStaysClean(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "off-model")
	opts.Rerank = rerank.Config{
		Enabled:        false,
		ProviderType:   rerank.ProviderOff,
		DisabledReason: "rerank provider is off",
	}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)

	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if strings.Contains(result.DegradedReason, "rerank-unconfigured") {
		t.Fatalf("显式 off 是配置形态不是缺口,不应提示: %q", result.DegradedReason)
	}
}

// TestQualityStrictRejectsUnconfiguredRerank:strict 档下缺口升级为
// 显式报错("甚至直接抛出错误"的裁决语义,经既有 strict 闸)。
func TestQualityStrictRejectsUnconfiguredRerank(t *testing.T) {
	const dim = 8
	server := newEmbedServer(t, dim)
	defer server.ts.Close()
	opts := embedOptions(server.ts.URL, dim, 8, "strict-model")
	opts.QualityStrict = true
	opts.Rerank = rerank.Config{
		Enabled:        false,
		ProviderType:   rerank.ProviderVoyage,
		DisabledReason: "rerank provider \"voyage\" has no API key",
	}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)

	_, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err == nil || !strings.Contains(err.Error(), "rerank-unconfigured") {
		t.Fatalf("strict 档下 rerank 缺口必须显式报错,实际 err=%v", err)
	}
}

// TestLexicalOnlyUnaffectedByRerankNotice:零凭据纯词法路径不受本
// 裁决影响(红线:词法路永远可用,不标 DEGRADED)。
func TestLexicalOnlyUnaffectedByRerankNotice(t *testing.T) {
	opts := Options{Rerank: rerank.Config{
		Enabled:        false,
		ProviderType:   rerank.ProviderVoyage,
		DisabledReason: "rerank provider \"voyage\" has no API key",
	}}
	e := newTestEngineWith(t, opts)
	root := newFixtureWorkspace(t)

	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil {
		t.Fatalf("lexical search failed: %v", err)
	}
	if result.DegradedReason != "" {
		t.Fatalf("纯词法零凭据路径不得携带降级原因: %q", result.DegradedReason)
	}
}
