package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// deadlineRecorder 记录 Search 收到的上下文截止时间。
type deadlineRecorder struct {
	fakeSyncer
	mu       sync.Mutex
	deadline time.Time
	has      bool
}

func (d *deadlineRecorder) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	d.mu.Lock()
	d.deadline, d.has = ctx.Deadline()
	d.mu.Unlock()
	return d.fakeSyncer.Search(ctx, req)
}

func (d *deadlineRecorder) seen() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deadline, d.has
}

// TestClientPassesCallerDeadlineToDaemon：wrapper 的调用带截止时间时，daemon 端
// 的请求上下文带上同样的截止时间（引擎据此在截止前返回带构建进度的错误）；
// 不带截止时间的调用在 daemon 端也没有截止时间。
func TestClientPassesCallerDeadlineToDaemon(t *testing.T) {
	// daemon 的任务状态文件写在缓存目录下，指向临时目录，不碰真实用户缓存。
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	recorder := &deadlineRecorder{}
	server := newDaemonHTTPTestServer(t, recorder)
	client := NewClient(server.URL)
	request := engine.SearchRequest{Workspace: engine.WorkspaceRef{DirectoryPath: "/tmp/project"}, Query: "find code"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	if _, err := client.Search(ctx, request); err != nil {
		t.Fatal(err)
	}
	got, has := recorder.seen()
	if !has || got.After(want.Add(time.Second)) || want.Sub(got) > 2*time.Second {
		t.Fatalf("daemon 端应收到调用方截止时间: has=%t got=%s want=%s", has, got, want)
	}

	if _, err := client.Search(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, has := recorder.seen(); has {
		t.Fatal("调用方没有截止时间时 daemon 端不应设截止时间")
	}
}
