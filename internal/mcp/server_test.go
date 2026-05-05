package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type fakeSyncer struct{}

func (fakeSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	return workspace.Result{Text: "retrieved"}, nil
}

func (fakeSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	return workspace.Result{FileCount: 1}, nil
}

type fakeTasker struct {
	fakeSyncer
}

func (fakeTasker) StartTask(ctx context.Context, req daemon.TaskRequest) (daemon.TaskSnapshot, error) {
	return daemon.TaskSnapshot{ID: "task-1", Kind: req.Kind, State: daemon.TaskStateQueued, DirectoryPath: req.DirectoryPath}, nil
}

func (fakeTasker) TaskStatus(ctx context.Context, id string) (daemon.TaskSnapshot, error) {
	return daemon.TaskSnapshot{ID: id, State: daemon.TaskStateCompleted}, nil
}

func (fakeTasker) CancelTask(ctx context.Context, id string) (daemon.TaskSnapshot, error) {
	return daemon.TaskSnapshot{ID: id, State: daemon.TaskStateCancelled}, nil
}

func TestToolsListOnlyIncludesTaskToolsForTasker(t *testing.T) {
	direct := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if strings.Contains(direct, "start_codebase_retrieval") {
		t.Fatalf("direct syncer should not list task tools: %s", direct)
	}

	withTasks := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(withTasks, "start_codebase_retrieval") {
		t.Fatalf("daemon tasker should list task tools: %s", withTasks)
	}
}

func TestStartRetrievalTaskTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"find task code"}}}`)
	if !strings.Contains(out, "task-1") {
		t.Fatalf("task response should include task id: %s", out)
	}
	if !strings.Contains(out, "queued") {
		t.Fatalf("task response should include task state: %s", out)
	}
}

func runMCP(t *testing.T, server *Server, line string) string {
	t.Helper()
	var out bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(line+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}
