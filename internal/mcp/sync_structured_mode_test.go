package mcp

import (
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestSyncStructuredIncludesRetrievalMode：同步结构化结果携带 retrieval_mode
// 与 deleted。同步的模式说明返回 revision 已建立的检索能力（lexical 或
// hybrid），纯词法也显式给出。
func TestSyncStructuredIncludesRetrievalMode(t *testing.T) {
	for _, mode := range []string{"lexical", "hybrid"} {
		fields := syncStructured(engine.Result{Engine: engine.EngineLocalHybrid, IndexRevision: "rev-1", FileCount: 3, Deleted: 25, RetrievalMode: mode})
		if fields["retrieval_mode"] != mode || fields["deleted"] != 25 {
			t.Fatalf("同步结构化结果应携带 retrieval_mode=%s 与 deleted: %+v", mode, fields)
		}
	}
}
