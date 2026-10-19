package mcp

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestRetrievalToolSchemaUsesSharedEnums：工具 schema 的 enum 取自 engine 的
// 合法取值清单，不另写一份。
func TestRetrievalToolSchemaUsesSharedEnums(t *testing.T) {
	props := retrievalTool()["inputSchema"].(map[string]any)["properties"].(map[string]any)
	detail := props["detail"].(map[string]any)["enum"].([]string)
	kind := props["artifact_kind"].(map[string]any)["enum"].([]string)
	if strings.Join(detail, ",") != strings.Join(engine.DetailValues(), ",") || strings.Join(kind, ",") != strings.Join(engine.ArtifactKindValues(), ",") {
		t.Fatalf("schema enum 应与 engine 清单一致: %v %v", detail, kind)
	}
}
