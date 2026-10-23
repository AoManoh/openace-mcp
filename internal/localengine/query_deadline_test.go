package localengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowDocumentServer 是文档嵌入请求固定耗时 delay 的 fake provider；查询嵌入
// 立即返回。docs 统计文档请求数。
func slowDocumentServer(t *testing.T, dim int, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var docs atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > 0 && strings.HasPrefix(req.Input[0], docTextPrefix) {
			docs.Add(1)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		items := make([]item, 0, len(req.Input))
		for i, text := range req.Input {
			items = append(items, item{Embedding: fakeVector(dim, text), Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(ts.Close)
	return ts, &docs
}

// TestQueryWaitEndsBeforeCallerDeadline：OPENACE_QUERY_BUILD_WAIT=0（等到构建
// 完成）而调用方带截止时间时（MCP 工具调用默认 110 秒），查询在截止前返回带
// 构建进度的错误，而不是等到截止、只得到 context deadline exceeded；查询离开
// 后构建继续，不重新发文档请求，之后的检索以 hybrid 作答。
func TestQueryWaitEndsBeforeCallerDeadline(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	const dim = 8
	ts, docs := slowDocumentServer(t, dim, 4*time.Second)
	opts := embedOptions(ts.URL, dim, 64, "deadline")
	opts.Embedding.Timeout = 30 * time.Second
	opts.QueryBuildWait = 0
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	root := newFixtureWorkspace(t)

	callerTimeout := 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), callerTimeout)
	defer cancel()
	started := time.Now()
	_, err = e.Search(ctx, searchRequest(root, "user login"))
	elapsed := time.Since(started)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("应在调用方截止前返回带构建进度的错误: elapsed=%s err=%v", elapsed, err)
	}
	if !strings.Contains(err.Error(), "build progress") && !strings.Contains(err.Error(), "index still building") {
		t.Fatalf("错误应带构建进度: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		result, err := e.Search(context.Background(), searchRequest(root, "user login"))
		if err == nil {
			if !strings.Contains(result.RetrievalMode, "hybrid") {
				t.Fatalf("构建完成后应以 hybrid 作答: %+v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("查询离开后构建应继续并完成: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := docs.Load(); got != 1 {
		t.Fatalf("查询离开不应取消构建，文档请求应只发 1 次，实际 %d 次", got)
	}
}
