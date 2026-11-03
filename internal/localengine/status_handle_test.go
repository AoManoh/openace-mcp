package localengine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// openHandles 返回引擎为该工作区缓存的检索句柄数。
func openHandles(e *Engine, workspaceKey string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for key := range e.handles {
		if strings.HasPrefix(key, workspaceKey+"\x00") {
			n++
		}
	}
	return n
}

// TestWorkspaceStatusDoesNotOpenHandle：workspace_status 与“是否正在构建”的查询
// 不新建检索句柄，顶层目录文件数照旧从 manifest 得出。改动前统计顶层目录文件数
// 会打开完整检索句柄并解码全部 chunk 元数据，后台监视每 30 秒查一次，被监视的
// 工作区在每次发布后都因此常驻一份句柄（第 8 步堆剖析，一半副本 92 MiB）。
func TestWorkspaceStatusDoesNotOpenHandle(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	_, workspaceKey, err := e.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	e.retireHandles(workspaceKey, "", "")
	if n := openHandles(e, workspaceKey); n != 0 {
		t.Fatalf("setup: %d handles still cached", n)
	}

	status, err := e.WorkspaceStatus(context.Background(), ref)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	manifest, _ := loadActiveManifest(t, e, root)
	want := map[string]int{}
	for path := range manifest.Files {
		top := "."
		if i := strings.IndexByte(path, '/'); i > 0 {
			top = path[:i]
		}
		want[top]++
	}
	if len(status.TopLevelFileCounts) == 0 || len(status.TopLevelFileCounts) != len(want) {
		t.Fatalf("top-level counts %v, want %v", status.TopLevelFileCounts, want)
	}
	for top, count := range want {
		if status.TopLevelFileCounts[top] != count {
			t.Fatalf("top-level counts %v, want %v", status.TopLevelFileCounts, want)
		}
	}
	if n := openHandles(e, workspaceKey); n != 0 {
		t.Fatalf("WorkspaceStatus opened %d retrieval handles", n)
	}
	inFlight, err := e.WorkspaceInFlight(context.Background(), ref)
	if err != nil || inFlight {
		t.Fatalf("idle workspace: inFlight=%v err=%v", inFlight, err)
	}
	if n := openHandles(e, workspaceKey); n != 0 {
		t.Fatalf("WorkspaceInFlight opened %d retrieval handles", n)
	}
}

// TestWorkspaceInFlight：构建进行中返回 true，结束后返回 false。
func TestWorkspaceInFlight(t *testing.T) {
	server := newEmbedServer(t, 8)
	release := make(chan struct{})
	server.setFailWhen(func([]string) bool {
		<-release
		return false
	})
	e := newTestEngineWith(t, embedOptions(server.ts.URL, 8, 64, "inflight"))
	root := newFixtureWorkspace(t)
	ref := engine.WorkspaceRef{DirectoryPath: root}
	done := make(chan error, 1)
	go func() {
		_, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: ref})
		done <- err
	}()
	sawInFlight := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if inFlight, err := e.WorkspaceInFlight(context.Background(), ref); err == nil && inFlight {
			sawInFlight = true
			break
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !sawInFlight {
		t.Fatal("WorkspaceInFlight should report true while the build is running")
	}
	if inFlight, err := e.WorkspaceInFlight(context.Background(), ref); err != nil || inFlight {
		t.Fatalf("after the build: inFlight=%v err=%v", inFlight, err)
	}
}
