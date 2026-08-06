package localengine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// P7(review 二批):输入类错误必须带请求类标记(daemon 据此 4xx 分流);
// 逐一钉住三个错误产生点——目录非法/profile 被拒/查询为空。
func TestInputErrorsCarryInvalidRequestMark(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	ctx := context.Background()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := e.Search(ctx, engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: missing}, Query: "find code"}); err == nil || !engine.IsInvalidRequest(err) {
		t.Fatalf("不存在的目录应判为请求类错误: %v", err)
	}

	if _, err := e.Search(ctx, engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: t.TempDir(), ProviderProfileID: "p1"}, Query: "find code"}); err == nil || !engine.IsInvalidRequest(err) {
		t.Fatalf("被拒的 provider_profile_id 应判为请求类错误: %v", err)
	}

	if _, err := e.Search(ctx, engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: t.TempDir()}, Query: "   "}); err == nil || !engine.IsInvalidRequest(err) {
		t.Fatalf("空查询应判为请求类错误: %v", err)
	}

	if _, err := e.Sync(ctx, engine.SyncRequest{Workspace: engine.WorkspaceRef{DirectoryPath: missing}}); err == nil || !engine.IsInvalidRequest(err) {
		t.Fatalf("Sync 对不存在目录应判为请求类错误: %v", err)
	}
}
