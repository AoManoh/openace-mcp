package localengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// M1 配套状态面回归:扫描期权限跳过如实上报 permission_skipped=N(K6)。
func TestPermissionSkippedSurfacesInStatus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受权限位约束")
	}
	e := newTestEngine(t)
	root := newFixtureWorkspace(t)
	locked := filepath.Join(root, "locked.py")
	if err := os.WriteFile(locked, []byte("secret = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })

	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatalf("M1 后单文件权限错误不应中止构建: %v", err)
	}
	status, err := e.WorkspaceStatus(context.Background(), engine.WorkspaceRef{DirectoryPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.UpstreamStatus, "permission_skipped=1") {
		t.Fatalf("权限跳过应如实上报(K6): %q", status.UpstreamStatus)
	}
}
