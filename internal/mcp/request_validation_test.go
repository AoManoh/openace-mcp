package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
)

// countingService 记录检索、同步与异步任务提交的调用次数。
type countingService struct {
	fakeTasker
	mu       sync.Mutex
	searches int
	starts   int
}

func (c *countingService) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	c.mu.Lock()
	c.searches++
	c.mu.Unlock()
	return engine.Result{Text: "retrieved"}, nil
}

func (c *countingService) StartTask(ctx context.Context, req daemon.TaskRequest) (daemon.TaskSnapshot, error) {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	return daemon.TaskSnapshot{ID: "task-1", Kind: req.Kind, State: daemon.TaskStateQueued}, nil
}

// TestInvalidRetrievalOptionsRejectedBeforeEngine：四个检索工具对非法 detail
// 与 artifact_kind 都返回工具错误，不调用引擎检索，也不提交异步任务。
func TestInvalidRetrievalOptionsRejectedBeforeEngine(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"codebase_retrieval", `{"directory_path":"/tmp/w","information_request":"q","detail":"unsupported"}`, "invalid detail"},
		{"codebase_retrieval", `{"directory_path":"/tmp/w","information_request":"q","detail":"PATHS"}`, "invalid detail"},
		{"multi_codebase_retrieval", `{"directory_paths":["/tmp/a","/tmp/b"],"information_request":"q","detail":"unsupported"}`, "invalid detail"},
		{"start_codebase_retrieval", `{"directory_path":"/tmp/w","information_request":"q","detail":"unsupported"}`, "invalid detail"},
		{"start_multi_codebase_retrieval", `{"directory_paths":["/tmp/a"],"information_request":"q","detail":"summary"}`, "invalid detail"},
		{"start_codebase_retrieval", `{"directory_path":"/tmp/w","information_request":"q","artifact_kind":"CODE"}`, "invalid artifact_kind"},
	}
	for _, tc := range cases {
		svc := &countingService{}
		out := runMCP(t, NewServer(svc), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tc.tool+`","arguments":`+tc.args+`}}`)
		if !strings.Contains(out, tc.want) || !strings.Contains(out, `"isError":true`) {
			t.Fatalf("%s: 应返回工具错误 %q: %s", tc.tool, tc.want, out)
		}
		if svc.searches != 0 || svc.starts != 0 {
			t.Fatalf("%s: 非法参数不应调用引擎或提交任务: searches=%d starts=%d", tc.tool, svc.searches, svc.starts)
		}
	}
	svc := &countingService{}
	out := runMCP(t, NewServer(svc), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"start_codebase_retrieval","arguments":{"directory_path":"/tmp/w","information_request":"q","detail":" paths "}}}`)
	if strings.Contains(out, `"isError":true`) || svc.starts != 1 {
		t.Fatalf("合法 detail（含首尾空白）应正常提交: starts=%d out=%s", svc.starts, out)
	}
}
