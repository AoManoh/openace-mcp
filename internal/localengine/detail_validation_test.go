package localengine

import (
	"context"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestInvalidDetailRejectedBeforeRetrieval：非法 detail 是纯参数错误，在
// 同步、建索引和查询嵌入之前拒绝。未索引的工作区和已索引的工作区都不产生
// 任何模型请求，错误是请求类错误（daemon 返回 400）。
func TestInvalidDetailRejectedBeforeRetrieval(t *testing.T) {
	server := newEmbedServer(t, 8)
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 16, "detail-model"))
	root := newFixtureWorkspace(t)
	req := engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: root}, Query: "HandleLogin", Detail: "unsupported"}

	_, err := e.Search(context.Background(), req)
	if err == nil || !engine.IsInvalidRequest(err) || server.callCount() != 0 {
		t.Fatalf("未索引工作区：应在任何模型请求前返回请求类错误: err=%v provider_requests=%d", err, server.callCount())
	}

	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}
	before := server.callCount()
	_, err = e.Search(context.Background(), req)
	if err == nil || !engine.IsInvalidRequest(err) || server.callCount() != before {
		t.Fatalf("已索引工作区：不应发出查询嵌入: err=%v provider_requests %d -> %d", err, before, server.callCount())
	}

	req.Detail = "paths"
	if _, err := e.Search(context.Background(), req); err != nil {
		t.Fatalf("合法 detail 应照常检索: %v", err)
	}
}
