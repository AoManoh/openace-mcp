package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 第 5 步 T5.5：daemon 聚合跨工作区检索时保留每个工作区正文里的 [INDEX] 行，
// multi_status 条目带 index_freshness，被截断的小节仍可经结构化字段读到。
func TestAggregateMultiRetrieveKeepsIndexLine(t *testing.T) {
	published := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	freshness := &engine.IndexFreshness{SnapshotRevision: "rev-old", PublishedAt: &published, PendingKnown: false}
	indexLine := "[INDEX] snapshot=rev-old published=2026-11-05T01:02:03Z pending=unknown"
	items := []multiRetrieveResult{
		{directoryPath: "/tmp/fresh", result: engine.Result{Text: "hit A", RetrievalMode: "hybrid", SemanticCoverage: "100%"}},
		{directoryPath: "/tmp/refreshing", result: engine.Result{
			Text:             "[DEGRADED] index-refreshing; mode=hybrid; semantic_coverage=100%\n" + indexLine + "\n\nhit B",
			RetrievalMode:    "hybrid",
			DegradedReason:   "index-refreshing",
			SemanticCoverage: "100%",
			IndexFreshness:   freshness,
		}},
	}
	aggregate, successCount := aggregateMultiRetrieveResults("", items)
	if successCount != 2 || aggregate.MultiStatus == nil {
		t.Fatalf("expected 2 successes with multi status, got %d %+v", successCount, aggregate.MultiStatus)
	}
	if !strings.Contains(aggregate.Text, "## /tmp/refreshing\n[DEGRADED] index-refreshing; mode=hybrid; semantic_coverage=100%\n"+indexLine+"\n\nhit B") {
		t.Fatalf("聚合正文应保留该工作区的 [INDEX] 行: %q", aggregate.Text)
	}
	for _, ws := range aggregate.MultiStatus.Workspaces {
		switch ws.DirectoryPath {
		case "/tmp/refreshing":
			if ws.IndexFreshness != freshness {
				t.Fatalf("multi_status 的降级工作区应带 index_freshness: %+v", ws)
			}
		default:
			if ws.IndexFreshness != nil {
				t.Fatalf("健康的工作区不应带 index_freshness: %+v", ws)
			}
		}
	}
}
