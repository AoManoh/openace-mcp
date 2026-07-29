package engine

import (
	"context"
	"go/build"
	"strings"
	"testing"
)

// fakeEngine 锁定 contract 方法签名：任何接口形状变化都会在此编译失败。
type fakeEngine struct{}

func (fakeEngine) Sync(_ context.Context, req SyncRequest) (Result, error) {
	return Result{CheckpointID: req.Workspace.DirectoryPath}, nil
}

func (fakeEngine) Search(_ context.Context, req SearchRequest) (Result, error) {
	return Result{Text: req.Query}, nil
}

func (fakeEngine) SyncBackground(_ context.Context, _ SyncRequest) (Result, error) {
	return Result{}, nil
}

func (fakeEngine) WorkspaceStatus(_ context.Context, ref WorkspaceRef) (WorkspaceStatus, error) {
	return WorkspaceStatus{DirectoryPath: ref.DirectoryPath}, nil
}

func (fakeEngine) ListWorkspaceStatuses(_ context.Context) ([]WorkspaceStatus, error) {
	return nil, nil
}

func (fakeEngine) WorkspaceChanged(_ context.Context, _ WorkspaceRef) (bool, error) {
	return false, nil
}

func (fakeEngine) Close(_ context.Context) error { return nil }

var (
	_ Service            = fakeEngine{}
	_ WorkspaceInspector = fakeEngine{}
	_ ChangeDetector     = fakeEngine{}
	_ BackgroundSyncer   = fakeEngine{}
	_ Lifecycle          = fakeEngine{}
)

func TestServiceContractRoundTrip(t *testing.T) {
	var svc Service = fakeEngine{}
	result, err := svc.Search(context.Background(), SearchRequest{Query: "q"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if result.Text != "q" {
		t.Fatalf("Search result text = %q, want %q", result.Text, "q")
	}
}

// TestEngineHasNoImplementationImports 守住依赖方向：
// engine 只能被实现方导入，不得反向依赖实现或宿主层。
func TestEngineHasNoImplementationImports(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("import engine package: %v", err)
	}
	forbidden := []string{"/internal/workspace", "/internal/ace", "/internal/mcp", "/internal/daemon", "/internal/provider", "/internal/auth", "/internal/managed"}
	for _, imported := range pkg.Imports {
		for _, banned := range forbidden {
			if strings.HasSuffix(imported, banned) {
				t.Fatalf("engine 不得导入实现或宿主包，发现 %s", imported)
			}
		}
	}
}
