package localengine

import (
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/index"
)

// TestServedResultUsesHeldRevision：查询结果的 revision 与文件数取自查询实际
// 持有的 revision；同步结果描述另一个 revision 时不沿用它的构建字段。
func TestServedResultUsesHeldRevision(t *testing.T) {
	synced := engine.Result{Engine: EngineID, IndexRevision: "rev-a", FileCount: 9, Added: 5, Deleted: 2, BuildMode: "delta", CrossProfileReused: 3,
		RetrievalMode: "hybrid", SemanticCoverage: "50%", DegradedReason: "semantic-coverage-partial"}
	other := servedResult(synced, &index.Manifest{Revision: "rev-b", Counts: index.Counts{Files: 7}})
	if other.IndexRevision != "rev-b" || other.FileCount != 7 || other.Added != 0 || other.Deleted != 0 || other.BuildMode != "" || other.CrossProfileReused != 0 ||
		other.RetrievalMode != "" || other.SemanticCoverage != "" || other.DegradedReason != "" {
		t.Fatalf("不同 revision 不得混用同步字段: %+v", other)
	}
	same := servedResult(synced, &index.Manifest{Revision: "rev-a", Counts: index.Counts{Files: 9}})
	if same.Added != 5 || same.Deleted != 2 || same.BuildMode != "delta" || same.CrossProfileReused != 3 || same.RetrievalMode != "" || same.DegradedReason != "" {
		t.Fatalf("同一 revision 沿用构建字段，但模式与原因由查询自己给出: %+v", same)
	}
}
