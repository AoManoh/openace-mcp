package localengine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestMixedLoadEightClients 是 G6 进程内封板：8 client 并发混合
// sync/search/status + 编辑波次，断言构建无放大（每波次全体 client 收敛
// 到同一新 revision，singleflight）、查询零错误零空窗、Close 后句柄表
// 清空（无泄漏）。
func TestMixedLoadEightClients(t *testing.T) {
	e := newTestEngineWith(t, Options{})
	root := newFixtureWorkspace(t)
	if _, err := e.Sync(context.Background(), syncRequest(root)); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errs := make(chan error, 16)
	report := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	var background sync.WaitGroup
	// 4 个持续查询 client + 2 个状态轮询 client。
	for i := 0; i < 4; i++ {
		background.Add(1)
		go func() {
			defer background.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				result, err := e.Search(context.Background(), searchRequest(root, "HandleLogin"))
				if err != nil {
					report(fmt.Errorf("search: %w", err))
					return
				}
				if !strings.Contains(result.Text, "HandleLogin") {
					report(fmt.Errorf("查询空窗: %q", result.Text))
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		background.Add(1)
		go func() {
			defer background.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := e.WorkspaceStatus(context.Background(), engineRef(root)); err != nil {
					report(fmt.Errorf("status: %w", err))
					return
				}
			}
		}()
	}

	// 10 个编辑波次：每波 8 个 sync client 并发，必须收敛到同一 revision
	// （单构建 owner，构建数 == 内容变更批次数）。
	previousRevision := ""
	for wave := 1; wave <= 10; wave++ {
		writeFixture(t, root, "util.py", fmt.Sprintf("def parse_config(path):\n    return %d  # wave\n", wave))
		var wg sync.WaitGroup
		revisions := make([]string, 8)
		for client := 0; client < 8; client++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				result, err := e.Sync(context.Background(), syncRequest(root))
				if err != nil {
					report(fmt.Errorf("wave %d sync: %w", wave, err))
					return
				}
				revisions[slot] = result.IndexRevision
			}(client)
		}
		wg.Wait()
		for slot := 1; slot < 8; slot++ {
			if revisions[slot] != revisions[0] {
				t.Fatalf("wave %d 构建放大（违反 G6 singleflight）: %v", wave, revisions)
			}
		}
		if revisions[0] == previousRevision {
			t.Fatalf("wave %d 应发布新 revision", wave)
		}
		previousRevision = revisions[0]
	}
	close(stop)
	background.Wait()
	select {
	case err := <-errs:
		t.Fatalf("混合负载错误（G6）: %v", err)
	default:
	}

	// 末态一致性 + Close 后零句柄泄漏。
	result, err := e.Search(context.Background(), searchRequest(root, "parse_config"))
	if err != nil || !strings.Contains(result.Text, "return 10") {
		t.Fatalf("末态检索: %v %q", err, result.Text)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	leaked := len(e.handles)
	e.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("Close 后句柄泄漏: %d", leaked)
	}
	if _, err := e.Sync(context.Background(), engine.SyncRequest{Workspace: engineRef(root)}); err == nil {
		t.Fatalf("Close 后应拒绝新请求")
	}
}
