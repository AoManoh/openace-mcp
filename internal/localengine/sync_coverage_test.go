package localengine

import (
	"context"
	"strings"
	"testing"
)

// P8(review 2026-08-06):provider 全程故障时构建照常发布 0% 覆盖
// revision,而 Sync 返回 nil error + 无覆盖信号——"禁止静默降级"红线
// 在 sync 工具面的缺口。修复后:Result 携带 semantic_coverage 与降级
// 原因,Summary 同步可见。
func TestSyncResultCarriesCoverageOnEmbedFailure(t *testing.T) {
	server := newEmbedServer(t, 8)
	server.failWhen = func(texts []string) bool { return true } // provider 全程故障
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 4, "fake-model"))
	root := newFixtureWorkspace(t)
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if result.SemanticCoverage != "0%" {
		t.Fatalf("Sync Result 应携带覆盖率 0%%,得到 %q", result.SemanticCoverage)
	}
	if !strings.Contains(result.DegradedReason, "semantic-coverage-partial") {
		t.Fatalf("Sync Result 应携带降级原因,得到 %q", result.DegradedReason)
	}
	summary := result.Summary()
	if !strings.Contains(summary, "semantic_coverage=0%") || !strings.Contains(summary, "degraded=") {
		t.Fatalf("Summary 应可见覆盖缺口: %q", summary)
	}
}

// 覆盖完整时不出现降级字段(正常路径零噪音)。
func TestSyncResultCleanWhenCoverageComplete(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 4, "fake-model"))
	root := newFixtureWorkspace(t)
	result, err := e.Sync(context.Background(), syncRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if result.SemanticCoverage != "100%" {
		t.Fatalf("覆盖率应为 100%%,得到 %q", result.SemanticCoverage)
	}
	if result.DegradedReason != "" {
		t.Fatalf("完整覆盖不应有降级原因: %q", result.DegradedReason)
	}
}
