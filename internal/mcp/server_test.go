package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
	"github.com/AoManoh/openace-mcp/internal/runtimeinfo"
)

type fakeSyncer struct{}

func (fakeSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	if profile := strings.TrimSpace(req.Workspace.ProviderProfileID); profile != "" {
		return engine.Result{Text: "retrieved with " + profile, ProviderProfileID: profile}, nil
	}
	return engine.Result{Text: "retrieved"}, nil
}

func (fakeSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	if profile := strings.TrimSpace(req.Workspace.ProviderProfileID); profile != "" {
		return engine.Result{CheckpointID: "checkpoint-" + profile, ProviderProfileID: profile, FileCount: 1}, nil
	}
	return engine.Result{FileCount: 1}, nil
}

type fakeTasker struct {
	fakeSyncer
}

type blockingDiagnosticTasker struct {
	fakeSyncer
}

type fakeMultiSyncer struct{}

func (fakeMultiSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	dir := req.Workspace.DirectoryPath
	if strings.Contains(dir, "bad") {
		return engine.Result{}, errors.New("workspace failed")
	}
	return engine.Result{Text: "retrieved from " + dir, CheckpointID: "checkpoint-" + dir, FileCount: 2}, nil
}

func (fakeMultiSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return engine.Result{CheckpointID: "checkpoint-" + req.Workspace.DirectoryPath, FileCount: 2}, nil
}

// transparencySyncer 返回带全套透明性/质量字段的检索结果（P1 用例）。
type transparencySyncer struct{}

func (transparencySyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	return engine.Result{
		Text:             "retrieved",
		Engine:           "local-hybrid",
		IndexRevision:    "rev-abc123",
		RetrievalMode:    "hybrid+rerank",
		SemanticCoverage: "87%",
		RerankSent:       50,
		EmbeddingProfile: "voyage/code-3/1024",
	}, nil
}

func (transparencySyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return engine.Result{FileCount: 1}, nil
}

// degradedMultiSyncer 让 /tmp/deg 仓返回降级结果、其余仓健康（P2 用例）。
type degradedMultiSyncer struct{}

func (degradedMultiSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	dir := req.Workspace.DirectoryPath
	if strings.Contains(dir, "deg") {
		return engine.Result{
			Text:             "[DEGRADED] query-embedding-failed(provider-5xx); mode=lexical\n\nretrieved from " + dir,
			RetrievalMode:    "lexical",
			DegradedReason:   "query-embedding-failed(provider-5xx)",
			SemanticCoverage: "93%",
		}, nil
	}
	return engine.Result{Text: "retrieved from " + dir, RetrievalMode: "hybrid+rerank", SemanticCoverage: "100%"}, nil
}

func (degradedMultiSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return engine.Result{FileCount: 1}, nil
}

type blockingToolSyncer struct{}

func (blockingToolSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	<-ctx.Done()
	return engine.Result{}, ctx.Err()
}

func (blockingToolSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	<-ctx.Done()
	return engine.Result{}, ctx.Err()
}

type timeoutMultiSyncer struct{}

func (timeoutMultiSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	dir := req.Workspace.DirectoryPath
	if strings.Contains(dir, "slow") {
		<-ctx.Done()
		return engine.Result{}, ctx.Err()
	}
	return engine.Result{Text: "retrieved from " + dir, CheckpointID: "checkpoint-" + dir, FileCount: 2}, nil
}

func (timeoutMultiSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return engine.Result{CheckpointID: "checkpoint-" + req.Workspace.DirectoryPath, FileCount: 2}, nil
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

func (fakeTasker) DaemonStatus(ctx context.Context) (daemon.Status, error) {
	return daemon.Status{
		Status: "ok",
		ServedBy: runtimeinfo.ServedBy{
			Service:        "openace-daemon",
			PID:            123,
			CacheNamespace: "test",
			Capabilities:   map[string]bool{"runtime_identity": true},
		},
	}, nil
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

func (fakeTasker) ListWorkspaceStatuses(ctx context.Context) ([]engine.WorkspaceStatus, error) {
	return []engine.WorkspaceStatus{{
		DirectoryPath:          "/tmp/workspace",
		CheckpointID:           "checkpoint",
		FileCount:              3,
		UpstreamStatus:         "backoff",
		UpstreamLastStatusCode: 429,
		UpstreamRetryAfter:     "30s",
	}}, nil
}

func (fakeTasker) WorkspaceStatus(ctx context.Context, ref engine.WorkspaceRef) (engine.WorkspaceStatus, error) {
	if profile := strings.TrimSpace(ref.ProviderProfileID); profile != "" {
		return engine.WorkspaceStatus{
			DirectoryPath:     ref.DirectoryPath,
			ProviderProfileID: profile,
			ProviderState:     "ready",
			CheckpointID:      "checkpoint-" + profile,
			FileCount:         1,
		}, nil
	}
	return engine.WorkspaceStatus{
		DirectoryPath:          ref.DirectoryPath,
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

// 灰度反馈协议(用户明示 2026-08-07):开启后 instructions 追加诊断
// 报告规范——调用 AI 每轮检索后输出多维事实报告,用户汇总回传改进。
// 产品默认不带(冻结文案不变)。
func TestGrayFeedbackProtocolAppendsToInstructions(t *testing.T) {
	plain := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if strings.Contains(plain, "GRAY-TEST FEEDBACK PROTOCOL") {
		t.Fatalf("默认 instructions 不得携带灰度协议: %s", plain)
	}
	t.Setenv(EnvGrayFeedback, "1")
	gray := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	for _, want := range []string{"GRAY-TEST FEEDBACK PROTOCOL", "诊断报告", "reproduction"} {
		if !strings.Contains(gray, want) {
			t.Fatalf("灰度 instructions 应含 %q: %s", want, gray)
		}
	}
}

// 灰度反馈一号 P1-3:Connect 失败此前直接 exit(1),客户端只见裸
// "Failed to connect",build/profile mismatch 的可行动文案永远到不了
// 调用方。不可用模式下会话保持:initialize/tools/list 正常,任何
// tools/call 返回失败原因与修复指引。
func TestUnavailableServerSurfacesConnectFailure(t *testing.T) {
	server := NewUnavailableServer(errors.New("openACE daemon at 127.0.0.1:8765 is not compatible with this MCP wrapper: wrapper revision aaa != daemon revision bbb"))
	init := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if !strings.Contains(init, "serverInfo") {
		t.Fatalf("不可用模式 initialize 应正常应答: %s", init)
	}
	list := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(list, "codebase_retrieval") {
		t.Fatalf("不可用模式应仍列出工具面: %s", list)
	}
	call := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/w","information_request":"q"}}}`)
	if !strings.Contains(call, `"isError":true`) || !strings.Contains(call, "wrapper revision aaa != daemon revision bbb") || !strings.Contains(call, "restart") {
		t.Fatalf("tools/call 应透传失败原因与修复指引: %s", call)
	}
}

// D2(吸收清单 codegraph #1):默认单工具面——多工具面导致弱 caller
// mis-pick 且每会话白烧 schema tokens;完整面经 OPENACE_MCP_TOOLS 恢复。
func TestToolsListDefaultsToMinimalFace(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(out, `"codebase_retrieval"`) {
		t.Fatalf("最小面应含主检索工具: %s", out)
	}
	for _, hidden := range []string{"multi_codebase_retrieval", "sync_workspace", "start_codebase_retrieval", "task_status", "daemon_status", "workspace_status", "list_tasks"} {
		if strings.Contains(out, `"`+hidden+`"`) {
			t.Fatalf("默认面不应列出 %s: %s", hidden, out)
		}
	}
}

// D2:未列出的工具保留 handler——按名直调仍被处理(工具面只影响发现,
// 不影响能力;既有编排/文档引用零破坏)。
func TestHiddenToolsRemainCallable(t *testing.T) {
	out := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sync_workspace","arguments":{"directory_path":"/tmp/workspace"}}}`)
	if !strings.Contains(out, "Workspace synced.") {
		t.Fatalf("未列出的工具应可按名调用: %s", out)
	}
}

// D2:自定义清单与能力面取交集(direct 模式没有 task 面,清单里的
// task_status 不出现,不报错——同一配置可跨形态共用)。
func TestToolsListEnvCustomListIntersectsCapability(t *testing.T) {
	t.Setenv(EnvMCPTools, "codebase_retrieval, task_status")
	withTasks := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(withTasks, `"codebase_retrieval"`) || !strings.Contains(withTasks, `"task_status"`) {
		t.Fatalf("daemon 形态应列出清单内两个工具: %s", withTasks)
	}
	if strings.Contains(withTasks, `"sync_workspace"`) {
		t.Fatalf("清单外工具不应列出: %s", withTasks)
	}
	direct := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(direct, `"codebase_retrieval"`) || strings.Contains(direct, `"task_status"`) {
		t.Fatalf("direct 形态应按能力面交集列出: %s", direct)
	}
}

// D2:清单里的未知工具名 fail-fast(会话起点即可行动报错,防拼写错误
// 静默缩面)。
func TestRunFailsFastOnUnknownToolInAllowlist(t *testing.T) {
	t.Setenv(EnvMCPTools, "codebase_retrieval,typo_tool")
	var out bytes.Buffer
	err := NewServer(fakeSyncer{}).Run(context.Background(), strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "typo_tool") || !strings.Contains(err.Error(), EnvMCPTools) {
		t.Fatalf("未知工具名应在启动即报可行动错误: %v", err)
	}
}

func TestToolsListOnlyIncludesTaskToolsForTasker(t *testing.T) {
	t.Setenv(EnvMCPTools, "all") // D2 后完整能力面须显式开启
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
	if !strings.Contains(withTasks, "daemon_status") {
		t.Fatalf("daemon tasker should list daemon status tool: %s", withTasks)
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

func TestDaemonStatusTool(t *testing.T) {
	out := runMCP(t, NewServer(fakeTasker{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"daemon_status","arguments":{}}}`)
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("decode MCP response: %v\n%s", err, out)
	}
	if len(response.Result.Content) == 0 {
		t.Fatalf("daemon_status should return content: %s", out)
	}
	var payload struct {
		MCPBuild map[string]any `json:"mcp_build"`
		Daemon   struct {
			Service        string          `json:"service"`
			PID            int             `json:"pid"`
			CacheNamespace string          `json:"cache_namespace"`
			Capabilities   map[string]bool `json:"capabilities"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode daemon_status payload: %v\n%s", err, response.Result.Content[0].Text)
	}
	if len(payload.MCPBuild) == 0 || payload.Daemon.Service != "openace-daemon" || payload.Daemon.PID != 123 || !payload.Daemon.Capabilities["runtime_identity"] {
		t.Fatalf("daemon_status should include wrapper and daemon identity: %+v", payload)
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
	if !strings.Contains(out, `"isError":true`) {
		t.Fatalf("partial failures should be visible as tool errors: %s", out)
	}
	if !strings.Contains(out, `"partial_failure":true`) || !strings.Contains(out, `"failure_count":1`) {
		t.Fatalf("partial failures should include structured status: %s", out)
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
	if !strings.Contains(out, `"partial_failure":false`) || !strings.Contains(out, `"failure_count":2`) {
		t.Fatalf("all failures should include structured status: %s", out)
	}
}

// P1(review 二批):单仓 codebase_retrieval 须以 structuredContent 携带
// 与 task 路径同名的透明性/质量字段——调用方可程序化判断检索模式与
// 降级状态,不再只能解析文本横幅。
func TestCodebaseRetrievalCarriesStructuredTransparency(t *testing.T) {
	out := runMCP(t, NewServer(transparencySyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"find code"}}}`)
	if !strings.Contains(out, "structuredContent") {
		t.Fatalf("retrieval should attach structured content: %s", out)
	}
	for _, want := range []string{
		`"retrieval_mode":"hybrid+rerank"`,
		`"semantic_coverage":"87%"`,
		`"rerank_sent":50`,
		`"embedding_profile":"voyage/code-3/1024"`,
		`"index_revision":"rev-abc123"`,
		`"engine":"local-hybrid"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("structured content should include %s: %s", want, out)
		}
	}

	// 字段全空(纯词法零配置/legacy 形状)时不附着 structuredContent,
	// 不得出现 "structuredContent":null(typed-nil 陷阱)。
	plain := runMCP(t, NewServer(fakeSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/workspace","information_request":"find code"}}}`)
	if strings.Contains(plain, "structuredContent") {
		t.Fatalf("plain result should not attach structured content: %s", plain)
	}
}

// P2(review 二批):multi_status 每仓条目须携带降级字段,聚合层给
// degraded_count 与首行 [DEGRADED] 汇总横幅;降级不是错误(isError=false)。
func TestMultiCodebaseRetrievalExposesPerWorkspaceDegradation(t *testing.T) {
	out := runMCP(t, NewServer(degradedMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/one","/tmp/deg"],"information_request":"find shared code"}}}`)
	if strings.Contains(out, `"isError":true`) {
		t.Fatalf("degraded workspaces are not tool errors: %s", out)
	}
	for _, want := range []string{
		`"degraded_count":1`,
		`"degraded_reason":"query-embedding-failed(provider-5xx)"`,
		`"retrieval_mode":"lexical"`,
		`"retrieval_mode":"hybrid+rerank"`,
		`"semantic_coverage":"93%"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("multi_status should carry per-workspace degradation %s: %s", want, out)
		}
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("decode MCP response: %v\n%s", err, out)
	}
	if len(response.Result.Content) == 0 || !strings.HasPrefix(response.Result.Content[0].Text, "[DEGRADED] 1 of 2 workspaces returned degraded results") {
		t.Fatalf("aggregate text should lead with degraded banner: %s", out)
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
	if !strings.Contains(out, `"failure_count":1`) {
		t.Fatalf("timeout should include structured partial status: %s", out)
	}
}

func TestMultiCodebaseRetrievalValidatesWorkspaceLimit(t *testing.T) {
	out := runMCP(t, NewServer(fakeMultiSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["1","2","3","4","5","6","7","8","9"],"information_request":"find code"}}}`)
	if !strings.Contains(out, "at most") || !strings.Contains(out, `"isError":true`) {
		t.Fatalf("workspace limit should return tool error: %s", out)
	}
}

// P13(review 二批):单条输入超 8MiB 此前令 Scanner ErrTooLong 终结整个
// MCP 会话进程;超长行只应得到 -32700 协议错误,会话继续服务后续请求。
func TestRunSurvivesOversizedInputLine(t *testing.T) {
	huge := strings.Repeat("x", 9<<20)
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"` + huge + `","directory_path":"/tmp/w"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	var out bytes.Buffer
	if err := NewServer(fakeSyncer{}).Run(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("oversized line must not kill the session: %v", err)
	}
	response := out.String()
	if !strings.Contains(response, "-32700") {
		t.Fatalf("oversized line should yield protocol error: %s", response)
	}
	if !strings.Contains(response, `"id":2`) || !strings.Contains(response, "codebase_retrieval") {
		t.Fatalf("session should keep serving after oversized line: %s", response)
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
