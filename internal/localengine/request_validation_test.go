package localengine

import (
	"context"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestSearchRoutesValidatesRetrievalOptions：评测入口 SearchRoutes 与 Search
// 用同一套纯参数规则，非法 detail 或 artifact_kind 在同步与模型请求之前拒绝。
func TestSearchRoutesValidatesRetrievalOptions(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 16, "validation-model"))
	root := newFixtureWorkspace(t)
	for _, req := range []engine.SearchRequest{
		{Workspace: engine.WorkspaceRef{DirectoryPath: root}, Query: "HandleLogin", ArtifactKind: "CODE"},
		{Workspace: engine.WorkspaceRef{DirectoryPath: root}, Query: "HandleLogin", Detail: "unsupported"},
	} {
		_, err := e.SearchRoutes(context.Background(), req, 10)
		if err == nil || !engine.IsInvalidRequest(err) {
			t.Fatalf("应返回请求类错误: %v", err)
		}
	}
	if server.callCount() != 0 {
		t.Fatalf("非法参数不应触发同步或模型请求，实际 %d 次", server.callCount())
	}
}
