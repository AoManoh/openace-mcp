package localengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// TestEmbeddingProgressVisible 是 G2 可见性（D8）：构建期轮询
// workspace_status 能看到 embedding 进度单调前进，构建结束后进度归零。
func TestEmbeddingProgressVisible(t *testing.T) {
	const dim = 8
	var mu sync.Mutex
	served := 0
	gate := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		<-gate // 每批等测试放行：进度采样窗口完全确定
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			items = append(items, item{Embedding: fakeVector(dim, text), Index: i})
		}
		mu.Lock()
		served++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer ts.Close()

	e := newTestEngineWith(t, embedOptions(ts.URL, dim, 1, "fake-model"))
	root := newFixtureWorkspace(t)
	syncDone := make(chan error, 1)
	go func() {
		_, err := e.Sync(context.Background(), syncRequest(root))
		syncDone <- err
	}()

	waitProgress := func(wantDone int) engine.WorkspaceStatus {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			status, err := e.WorkspaceStatus(context.Background(), engineRef(root))
			if err == nil && status.Semantic != nil && status.Semantic.EmbeddedChunks >= wantDone {
				return status
			}
			select {
			case <-deadline:
				t.Fatalf("等待进度 %d 超时: %+v", wantDone, status.Semantic)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	// 放行 3 批，采样 3 个递增进度点（G2 验收：≥3 个递增采样）。
	var samples []engine.WorkspaceStatus
	for i := 1; i <= 3; i++ {
		gate <- struct{}{}
		samples = append(samples, waitProgress(i))
	}
	for i := 1; i < len(samples); i++ {
		prev, curr := samples[i-1].Semantic, samples[i].Semantic
		if curr.EmbeddedChunks <= prev.EmbeddedChunks-1 || curr.PendingChunks > prev.PendingChunks {
			t.Fatalf("进度应单调前进: %+v → %+v", prev, curr)
		}
	}
	if samples[0].Semantic.PendingChunks == 0 {
		t.Fatalf("构建中应有待嵌入计数: %+v", samples[0].Semantic)
	}

	close(gate) // 放行剩余批次
	if err := <-syncDone; err != nil {
		t.Fatal(err)
	}
	final, err := e.WorkspaceStatus(context.Background(), engineRef(root))
	if err != nil || final.Semantic == nil {
		t.Fatal(err)
	}
	if final.Semantic.PendingChunks != 0 || final.Semantic.EmbeddedChunks != 0 {
		t.Fatalf("构建结束后进度应归零: %+v", final.Semantic)
	}
	if final.Semantic.JournalEntries != 0 {
		t.Fatalf("发布后 journal 应为空: %+v", final.Semantic)
	}
	if final.Semantic.Coverage != "100%" {
		t.Fatalf("最终覆盖应完整: %+v", final.Semantic)
	}
}

// TestSearchSyncFailureNoRevisionRawError 是自审 RS2 回归：sync 失败且
// 无任何可用 revision 时，Search 返回原始错误而非降级横幅（无可降级
// 对象，deny/allow 不适用）。
func TestSearchSyncFailureNoRevisionRawError(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("OPENACE_CACHE_DIR", cacheDir)
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	root := newFixtureWorkspace(t)

	holder, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	// 持有者只拿锁不发布（用一个不存在文件的工作区制造无 revision？
	// 直接对同 cache 起第二实例：pre-search sync 因写锁失败，且盘上
	// 无任何 revision——错误必须原样上浮。
	_, workspaceKey, err := holder.resolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := holder.storeFor(workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.acquireWriteLock(workspaceKey, store); err != nil {
		t.Fatal(err)
	}

	second, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	_, searchErr := second.Search(context.Background(), searchRequest(root, "HandleLogin"))
	if searchErr == nil || !strings.Contains(searchErr.Error(), "写锁") {
		t.Fatalf("无可用 revision 时 sync 失败应原样上浮: %v", searchErr)
	}
}
