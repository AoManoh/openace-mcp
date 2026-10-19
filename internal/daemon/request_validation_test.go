package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// countingSearcher 记录引擎检索调用次数。
type countingSearcher struct {
	fakeSyncer
	searches int64
}

func (c *countingSearcher) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	atomic.AddInt64(&c.searches, 1)
	return engine.Result{Text: "retrieved"}, nil
}

// TestDaemonRejectsInvalidRetrievalOptionsOnReceipt：同步检索与单仓、多仓异步
// 任务收到非法 detail 或 artifact_kind 时返回 HTTP 400，不生成任务、不调用
// 引擎；同步任务不校验检索参数。
func TestDaemonRejectsInvalidRetrievalOptionsOnReceipt(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	searcher := &countingSearcher{}
	server := newDaemonHTTPTestServer(t, searcher)
	post := func(path string, body any) int {
		t.Helper()
		payload, _ := json.Marshal(body)
		resp, err := http.Post(server.URL+path, "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/v1/retrieve", map[string]any{"directory_path": "/tmp/w", "information_request": "q", "detail": "unsupported"}); code != http.StatusBadRequest {
		t.Fatalf("同步检索非法 detail 应为 400，实际 %d", code)
	}
	for _, req := range []TaskRequest{
		{Kind: TaskKindRetrieve, DirectoryPath: "/tmp/w", InformationRequest: "q", Detail: "unsupported"},
		{Kind: TaskKindRetrieve, DirectoryPath: "/tmp/w", InformationRequest: "q", ArtifactKind: "CODE"},
		{Kind: TaskKindMultiRetrieve, DirectoryPaths: []string{"/tmp/a", "/tmp/b"}, InformationRequest: "q", Detail: "summary"},
	} {
		if code := post("/v1/tasks", req); code != http.StatusBadRequest {
			t.Fatalf("异步任务非法参数应为 400，实际 %d: %+v", code, req)
		}
	}
	resp, err := http.Get(server.URL + "/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Tasks []TaskSnapshot `json:"tasks"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed.Tasks) != 0 || atomic.LoadInt64(&searcher.searches) != 0 {
		t.Fatalf("非法参数不应生成任务或调用引擎: tasks=%d searches=%d", len(listed.Tasks), atomic.LoadInt64(&searcher.searches))
	}
	if code := post("/v1/tasks", TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/w", Detail: "unsupported"}); code != http.StatusAccepted {
		t.Fatalf("同步任务不凭检索参数被拒绝，应为 202，实际 %d", code)
	}
}
