package mcp

import (
	"bytes"
	"context"
	"errors"
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

type blockingDiagnosticTasker struct {
	fakeSyncer
}

type fakeMultiSyncer struct{}

func (fakeMultiSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	if strings.Contains(dir, "bad") {
		return workspace.Result{}, errors.New("workspace failed")
	}
	return workspace.Result{Text: "retrieved from " + dir, CheckpointID: "checkpoint-" + dir, FileCount: 2}, nil
}

func (fakeMultiSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	return workspace.Result{CheckpointID: "checkpoint-" + dir, FileCount: 2}, nil
}

type blockingToolSyncer struct{}

func (blockingToolSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	<-ctx.Done()
	return workspace.Result{}, ctx.Err()
}

func (blockingToolSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	<-ctx.Done()
	return workspace.Result{}, ctx.Err()
}

type timeoutMultiSyncer struct{}

func (timeoutMultiSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	if strings.Contains(dir, "slow") {
		<-ctx.Done()
		return workspace.Result{}, ctx.Err()
	}
	return workspace.Result{Text: "retrieved from " + dir, CheckpointID: "checkpoint-" + dir, FileCount: 2}, nil
}

func (timeoutMultiSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	return workspace.Result{CheckpointID: "checkpoint-" + dir, FileCount: 2}, nil
}

func (fakeTasker) StartTask(ctx context.Context, req daemon.TaskRequest) (daemon.TaskSnapshot, error) {
	return daemon.TaskSnapshot{ID: "task-1", Kind: req.Kind, State: daemon.TaskStateQueued, DirectoryPath: req.DirectoryPath, DirectoryPaths: append([]string(nil), req.DirectoryPaths...), ProviderProfileID: req.ProviderProfileID}, nil
}

func (fakeTasker) ListTasks(ctx context.Context, limit int) ([]daemon.TaskSnapshot, error) {
	return []daemon.TaskSnapshot{{ID: "task-1", State: daemon.TaskStateCompleted}}, nil
}

func (fakeTasker) TaskStatus(ctx context.Context, id string) (daemon.TaskSnapshot, error) {
	return daemon.TaskSnapshot{ID: id, State: daemon.TaskStateCompleted}, nil
}

func (fakeTasker) CancelTask(ctx context.Context, id string) (daemon.TaskSnapshot, error) {
	return daemon.TaskSnapshot{ID: id, State: daemon.TaskStateCancelled}, nil
}

func (blockingDiagnosticTasker) StartTask(ctx context.Context, req daemon.TaskRequest) (daemon.TaskSnapshot, error) {
	<-ctx.Done()
	return daemon.TaskSnapshot{}, ctx.Err()
}

func (blockingDiagnosticTasker) ListTasks(ctx context.Context, limit int) ([]daemon.TaskSnapshot, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingDiagnosticTasker) TaskStatus(ctx context.Context, id string) (daemon.TaskSnapshot, error) {
	<-ctx.Done()
	return daemon.TaskSnapshot{}, ctx.Err()
}

func (blockingDiagnosticTasker) CancelTask(ctx context.Context, id string) (daemon.TaskSnapshot, error) {
	<-ctx.Done()
	return daemon.TaskSnapshot{}, ctx.Err()
}

func (fakeTasker) ListWorkspaceStatuses(ctx context.Context) ([]workspace.WorkspaceStatus, error) {
	return []workspace.WorkspaceStatus{{
		DirectoryPath:          "/tmp/workspace",
		CheckpointID:           "checkpoint",
		FileCount:              3,
		UpstreamStatus:         "backoff",
		UpstreamLastStatusCode: 429,
		UpstreamRetryAfter:     "30s",
	}}, nil
}

func (fakeTasker) WorkspaceStatus(ctx context.Context, dir string) (workspace.WorkspaceStatus, error) {
	return workspace.WorkspaceStatus{
		DirectoryPath:          dir,
		CheckpointID:           "checkpoint",
		FileCount:              3,
		UpstreamStatus:         "backoff",
		UpstreamLastStatusCode: 429,
		UpstreamRetryAfter:     "30s",
	}, nil
}

type fakeProviderSyncer struct {
	fakeTasker
}

func (fakeProviderSyncer) RetrieveWithProvider(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int) (workspace.Result, error) {
	return workspace.Result{Text: "retrieved with " + providerProfileID, ProviderProfileID: providerProfileID}, nil
}

func (fakeProviderSyncer) SyncWithProvider(ctx context.Context, dir string, providerProfileID string) (workspace.Result, error) {
	return workspace.Result{CheckpointID: "checkpoint-" + providerProfileID, ProviderProfileID: providerProfileID, FileCount: 1}, nil
}

func (fakeProviderSyncer) WorkspaceStatusWithProvider(ctx context.Context, dir string, providerProfileID string) (workspace.WorkspaceStatus, error) {
	return workspace.WorkspaceStatus{
		DirectoryPath:          dir,
		ProviderProfileID:      providerProfileID,
		ProviderState:          "ready",
		CheckpointID:           "checkpoint-" + providerProfileID,
		FileCount:              1,
		UpstreamLastStatusCode: 0,
	}, nil
}

func TestToolsListOnlyIncludesTaskToolsForTasker(t *testing.T) {
	direct := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(direct, "multi_codebase_retrieval") {
		t.Fatalf("direct syncer should list multi retrieval tool: %s", direct)
	}
	if strings.Contains(direct, "start_codebase_retrieval") {
		t.Fatalf("direct syncer should not list task tools: %s", direct)
	}
	if strings.Contains(direct, "list_workspaces") {
		t.Fatalf("direct syncer should not list workspace status tools: %s", direct)
	}

	withTasks := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(withTasks, "start_codebase_retrieval") {
		t.Fatalf("daemon tasker should list task tools: %s", withTasks)
	}
	if !strings.Contains(withTasks, "start_multi_codebase_retrieval") {
		t.Fatalf("daemon tasker should list multi retrieval task tool: %s", withTasks)
	}
	if !strings.Contains(withTasks, "list_tasks") {
		t.Fatalf("daemon tasker should list task diagnostics tool: %s", withTasks)
	}
	if !strings.Contains(withTasks, "list_workspaces") {
		t.Fatalf("daemon tasker should list workspace diagnostics tool: %s", withTasks)
	}
}

func TestProviderProfileArgumentPassesThroughRetrievalTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeProviderSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","provider_profile_id":"standby","information_request":"find code"}}}`)
	if !strings.Contains(out, "retrieved with standby") {
		t.Fatalf("retrieval should use provider-aware syncer: %s", out)
	}
	if !strings.Contains(out, "provider_profile_id") || !strings.Contains(out, "standby") {
		t.Fatalf("retrieval summary should include provider result metadata: %s", out)
	}
}

func TestProviderProfileArgumentPassesThroughTaskTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeProviderSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_sync_workspace","arguments":{"directory_path":"/tmp/workspace","provider_profile_id":"standby"}}}`)
	if !strings.Contains(out, "provider_profile_id") || !strings.Contains(out, "standby") {
		t.Fatalf("task response should retain provider profile: %s", out)
	}
}

func TestProviderProfileArgumentPassesThroughWorkspaceStatusTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeProviderSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_status","arguments":{"directory_path":"/tmp/workspace","provider_profile_id":"standby"}}}`)
	if !strings.Contains(out, "provider_profile_id") || !strings.Contains(out, "standby") || !strings.Contains(out, "checkpoint-standby") {
		t.Fatalf("workspace status should use provider-aware inspector: %s", out)
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

func TestTaskStatusToolAppliesTimeout(t *testing.T) {
	t.Setenv("OPENACE_TOOL_TIMEOUT", "10ms")
	out := runMCP(t, NewServer(blockingDiagnosticTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_status","arguments":{"task_id":"task-1"}}}`)
	if !strings.Contains(out, "context deadline exceeded") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("task status timeout should return tool error: %s", out)
	}
}

func TestCodebaseRetrievalRejectsWhitespaceArguments(t *testing.T) {
	out := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"   ","information_request":"find code"}}}`)
	if !strings.Contains(out, "directory_path is required") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("blank directory path should be rejected: %s", out)
	}
	out = runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"   "}}}`)
	if !strings.Contains(out, "information_request is required") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("blank information request should be rejected: %s", out)
	}
}

func TestRetrievalToolsValidateMaxOutputLength(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"find code","max_output_length":-1}}}`)
	if !strings.Contains(out, "max_output_length must be non-negative") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("negative max output should be rejected: %s", out)
	}
	out = runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"find code","max_output_length":1000001}}}`)
	if !strings.Contains(out, "max_output_length must be") || !strings.Contains(out, "1000000") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("huge max output should be rejected: %s", out)
	}
}

func TestStartMultiRetrievalTaskTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/one","/tmp/two"],"information_request":"find task code"}}}`)
	if !strings.Contains(out, "task-1") {
		t.Fatalf("task response should include task id: %s", out)
	}
	if !strings.Contains(out, "multi_codebase_retrieval") {
		t.Fatalf("task response should include multi retrieval kind: %s", out)
	}
	if !strings.Contains(out, "/tmp/one") || !strings.Contains(out, "/tmp/two") {
		t.Fatalf("task response should include directory paths: %s", out)
	}
}

func TestListTasksTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{"limit":5}}}`)
	if !strings.Contains(out, "task-1") {
		t.Fatalf("list response should include task id: %s", out)
	}
	if !strings.Contains(out, "completed") {
		t.Fatalf("list response should include task state: %s", out)
	}
}

func TestListWorkspacesTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_workspaces","arguments":{}}}`)
	if !strings.Contains(out, "/tmp/workspace") {
		t.Fatalf("workspace list should include directory path: %s", out)
	}
	if !strings.Contains(out, "checkpoint") {
		t.Fatalf("workspace list should include checkpoint: %s", out)
	}
	if !strings.Contains(out, "upstream_status") || !strings.Contains(out, "backoff") {
		t.Fatalf("workspace list should include upstream health: %s", out)
	}
}

func TestWorkspaceStatusTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_status","arguments":{"directory_path":"/tmp/workspace"}}}`)
	if !strings.Contains(out, "/tmp/workspace") {
		t.Fatalf("workspace status should include directory path: %s", out)
	}
	if !strings.Contains(out, "file_count") || !strings.Contains(out, "3") {
		t.Fatalf("workspace status should include file count: %s", out)
	}
	if !strings.Contains(out, "upstream_last_status_code") || !strings.Contains(out, "429") {
		t.Fatalf("workspace status should include upstream health: %s", out)
	}
}

func TestMultiCodebaseRetrievalTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/one","/tmp/two"],"information_request":"find shared auth code"}}}`)
	if !strings.Contains(out, "/tmp/one") || !strings.Contains(out, "/tmp/two") {
		t.Fatalf("multi retrieval should include both workspace paths: %s", out)
	}
	if !strings.Contains(out, "retrieved from /tmp/one") || !strings.Contains(out, "retrieved from /tmp/two") {
		t.Fatalf("multi retrieval should include both results: %s", out)
	}
}

func TestMultiCodebaseRetrievalKeepsPartialResults(t *testing.T) {
	out := runMCP(t, NewServer(fakeMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/one","/tmp/bad"],"information_request":"find shared auth code"}}}`)
	if !strings.Contains(out, "retrieved from /tmp/one") {
		t.Fatalf("multi retrieval should keep successful workspace result: %s", out)
	}
	if !strings.Contains(out, "/tmp/bad") || !strings.Contains(out, "workspace failed") {
		t.Fatalf("multi retrieval should include failed workspace error: %s", out)
	}
	if strings.Contains(out, `"isError":true`) {
		t.Fatalf("partial failures should not fail whole tool: %s", out)
	}
}

func TestMultiCodebaseRetrievalReportsAllFailuresAsToolError(t *testing.T) {
	out := runMCP(t, NewServer(fakeMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/bad-one","/tmp/bad-two"],"information_request":"find shared auth code"}}}`)
	if !strings.Contains(out, "/tmp/bad-one") || !strings.Contains(out, "/tmp/bad-two") {
		t.Fatalf("multi retrieval should include every failed workspace: %s", out)
	}
	if !strings.Contains(out, "workspace failed") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("all failures should return tool error: %s", out)
	}
}

func TestCodebaseRetrievalToolAppliesTimeout(t *testing.T) {
	t.Setenv("OPENACE_TOOL_TIMEOUT", "10ms")
	out := runMCP(t, NewServer(blockingToolSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"find code"}}}`)
	if !strings.Contains(out, "context deadline exceeded") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("timeout should return tool error: %s", out)
	}
}

func TestMultiCodebaseRetrievalTimeoutReportsToolErrorEvenWithPartialResults(t *testing.T) {
	t.Setenv("OPENACE_TOOL_TIMEOUT", "10ms")
	out := runMCP(t, NewServer(timeoutMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/fast","/tmp/slow"],"information_request":"find shared code"}}}`)
	if !strings.Contains(out, "retrieved from /tmp/fast") {
		t.Fatalf("timeout response should keep partial result: %s", out)
	}
	if !strings.Contains(out, "context deadline exceeded") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("timeout should return tool error: %s", out)
	}
}

func TestMultiCodebaseRetrievalValidatesWorkspaceLimit(t *testing.T) {
	out := runMCP(t, NewServer(fakeMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["1","2","3","4","5","6","7","8","9"],"information_request":"find code"}}}`)
	if !strings.Contains(out, "at most") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("workspace limit should return tool error: %s", out)
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
