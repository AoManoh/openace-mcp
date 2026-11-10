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

// TestDiagnosticsVectorLoad：诊断行的阶段耗时里有 vector_load（为所用 revision 加载
// 向量段的耗时），各分项相加等于 total；没有加载时写 0，字段位置固定。
func TestDiagnosticsVectorLoad(t *testing.T) {
	result := engine.Result{
		RetrievalMode: "hybrid",
		Timings: &engine.RetrievalTimings{TotalMs: 5807 + 6097, SyncMs: 1710, LexicalMs: 892, QueryEmbedMs: 1513,
			VectorLoadMs: 5807, VectorMs: 68, RerankMs: 1903, RenderMs: 11},
	}
	line := retrievalDiagnosticsText(result)
	want := "timings_ms[total=11904 sync=1710 lexical=892 embed=1513 vector_load=5807 vector=68 fuse=0 rerank=1903 render=11]"
	if !strings.Contains(line, want) {
		t.Fatalf("diagnostics should carry vector_load between embed and vector:\n got %s\nwant %s", line, want)
	}
	result.Timings = &engine.RetrievalTimings{TotalMs: 10}
	if line := retrievalDiagnosticsText(result); !strings.Contains(line, "embed=0 vector_load=0 vector=0") {
		t.Fatalf("vector_load should be written as 0 when nothing was loaded: %s", line)
	}
}
