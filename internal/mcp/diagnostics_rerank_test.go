package mcp

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestDiagnosticsRerankAttempts：诊断行在阶段耗时之后写出精排尝试次数、重试
// 等待与最长单次耗时；没有精排尝试时不写这三项。
func TestDiagnosticsRerankAttempts(t *testing.T) {
	result := engine.Result{
		RetrievalMode: "hybrid+rerank",
		RerankSent:    50,
		Timings:       &engine.RetrievalTimings{TotalMs: 3000, RerankMs: 2500, RerankAttempts: 2, RerankRetryWaitMs: 600, RerankAttemptMs: 1700},
	}
	line := retrievalDiagnosticsText(result)
	if !strings.Contains(line, "rerank_attempts=2 rerank_retry_wait_ms=600 rerank_attempt_ms=1700") {
		t.Fatalf("diagnostics should carry rerank attempts: %s", line)
	}
	result.Timings = &engine.RetrievalTimings{TotalMs: 10}
	if line := retrievalDiagnosticsText(result); strings.Contains(line, "rerank_attempts") {
		t.Fatalf("no rerank attempt, no rerank_attempts field: %s", line)
	}
}
