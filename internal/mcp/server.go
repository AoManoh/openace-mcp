package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const maxMultiWorkspacePaths = daemon.MaxMultiWorkspacePaths
const defaultToolTimeout = 110 * time.Second

type Syncer interface {
	Retrieve(context.Context, string, string, int) (workspace.Result, error)
	Sync(context.Context, string) (workspace.Result, error)
}

type ProviderSyncer interface {
	RetrieveWithProvider(context.Context, string, string, string, int) (workspace.Result, error)
	SyncWithProvider(context.Context, string, string) (workspace.Result, error)
}

type Tasker interface {
	CancelTask(context.Context, string) (daemon.TaskSnapshot, error)
	ListTasks(context.Context, int) ([]daemon.TaskSnapshot, error)
	StartTask(context.Context, daemon.TaskRequest) (daemon.TaskSnapshot, error)
	TaskStatus(context.Context, string) (daemon.TaskSnapshot, error)
}

type WorkspaceInspector interface {
	ListWorkspaceStatuses(context.Context) ([]workspace.WorkspaceStatus, error)
	WorkspaceStatus(context.Context, string) (workspace.WorkspaceStatus, error)
}

type ProviderWorkspaceInspector interface {
	WorkspaceStatusWithProvider(context.Context, string, string) (workspace.WorkspaceStatus, error)
}

type Server struct {
	syncer    Syncer
	tasker    Tasker
	inspector WorkspaceInspector
}

type rpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type retrievalArgs struct {
	InformationRequest string `json:"information_request"`
	DirectoryPath      string `json:"directory_path"`
	ProviderProfileID  string `json:"provider_profile_id,omitempty"`
	MaxOutputLength    int    `json:"max_output_length,omitempty"`
}

type multiRetrievalArgs struct {
	InformationRequest string   `json:"information_request"`
	DirectoryPaths     []string `json:"directory_paths"`
	ProviderProfileID  string   `json:"provider_profile_id,omitempty"`
	MaxOutputLength    int      `json:"max_output_length,omitempty"`
}

type syncArgs struct {
	DirectoryPath     string `json:"directory_path"`
	ProviderProfileID string `json:"provider_profile_id,omitempty"`
}

type taskIDArgs struct {
	TaskID string `json:"task_id"`
}

type listTasksArgs struct {
	Limit int `json:"limit,omitempty"`
}

func NewServer(syncer Syncer) *Server {
	server := &Server{syncer: syncer}
	if tasker, ok := syncer.(Tasker); ok {
		server.tasker = tasker
	}
	if inspector, ok := syncer.(WorkspaceInspector); ok && server.tasker != nil {
		server.inspector = inspector
	}
	return server
}

func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			resp := rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32700, Message: err.Error()}}
			if err := enc.Encode(resp); err != nil {
				return err
			}
			continue
		}
		if req.ID == nil {
			s.handleNotification(req)
			continue
		}
		resp := s.handle(ctx, req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *Server) handle(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return ok(req.ID, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "openace-codebase",
				"version": "0.1.0",
			},
		})
	case "tools/list":
		tools := []any{retrievalTool(), multiRetrievalTool(), syncTool()}
		if s.tasker != nil {
			tools = append(tools, startRetrievalTool(), startMultiRetrievalTool(), startSyncTool(), taskStatusTool(), listTasksTool(), cancelTaskTool())
		}
		if s.inspector != nil {
			tools = append(tools, listWorkspacesTool(), workspaceStatusTool())
		}
		return ok(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		return s.callTool(ctx, req)
	default:
		return fail(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleNotification(req rpcRequest) {
	if req.Method != "notifications/initialized" {
		fmt.Fprintf(os.Stderr, "openace-mcp: ignored notification %s\n", req.Method)
	}
}

func toolTimeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, toolTimeout())
}

func toolTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv("OPENACE_TOOL_TIMEOUT"))
	if value == "" {
		return defaultToolTimeout
	}
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultToolTimeout
}

func (s *Server) callTool(ctx context.Context, req rpcRequest) rpcResponse {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fail(req.ID, -32602, err.Error())
	}
	switch params.Name {
	case "codebase_retrieval", "codebase-retrieval":
		var args retrievalArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.InformationRequest == "" {
			return toolError(req.ID, "information_request is required")
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		toolCtx, cancel := toolTimeoutContext(ctx)
		defer cancel()
		result, err := s.retrieve(toolCtx, args.DirectoryPath, args.ProviderProfileID, args.InformationRequest, args.MaxOutputLength)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		text := strings.TrimSpace(result.Text)
		if text == "" {
			text = "No relevant code sections were found."
		}
		return ok(req.ID, toolResult(text+"\n\n"+result.Summary(), false))
	case "multi_codebase_retrieval", "multi-codebase-retrieval":
		var args multiRetrievalArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.InformationRequest == "" {
			return toolError(req.ID, "information_request is required")
		}
		paths, err := normalizeDirectoryPaths(args.DirectoryPaths)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		toolCtx, cancel := toolTimeoutContext(ctx)
		defer cancel()
		results := s.retrieveMultiple(toolCtx, paths, args.ProviderProfileID, args.InformationRequest, args.MaxOutputLength)
		text := formatMultiRetrievalResults(results)
		if err := toolCtx.Err(); err != nil {
			return toolError(req.ID, text)
		}
		if allMultiRetrievalsFailed(results) {
			return toolError(req.ID, text)
		}
		return ok(req.ID, toolResult(text, false))
	case "sync_workspace", "sync-workspace":
		var args syncArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		toolCtx, cancel := toolTimeoutContext(ctx)
		defer cancel()
		result, err := s.syncWorkspace(toolCtx, args.DirectoryPath, args.ProviderProfileID)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult("Workspace synced.\n"+result.Summary(), false))
	case "start_codebase_retrieval", "start-codebase-retrieval":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require daemon mode")
		}
		var args retrievalArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.InformationRequest == "" {
			return toolError(req.ID, "information_request is required")
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		task, err := s.tasker.StartTask(ctx, daemon.TaskRequest{
			Kind:               daemon.TaskKindRetrieve,
			DirectoryPath:      args.DirectoryPath,
			ProviderProfileID:  strings.TrimSpace(args.ProviderProfileID),
			InformationRequest: args.InformationRequest,
			MaxOutputLength:    args.MaxOutputLength,
		})
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "start_multi_codebase_retrieval", "start-multi-codebase-retrieval":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require daemon mode")
		}
		var args multiRetrievalArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.InformationRequest == "" {
			return toolError(req.ID, "information_request is required")
		}
		paths, err := normalizeDirectoryPaths(args.DirectoryPaths)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		task, err := s.tasker.StartTask(ctx, daemon.TaskRequest{
			Kind:               daemon.TaskKindMultiRetrieve,
			DirectoryPaths:     paths,
			ProviderProfileID:  strings.TrimSpace(args.ProviderProfileID),
			InformationRequest: args.InformationRequest,
			MaxOutputLength:    args.MaxOutputLength,
		})
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "start_sync_workspace", "start-sync-workspace":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require daemon mode")
		}
		var args syncArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		task, err := s.tasker.StartTask(ctx, daemon.TaskRequest{
			Kind:              daemon.TaskKindSync,
			DirectoryPath:     args.DirectoryPath,
			ProviderProfileID: strings.TrimSpace(args.ProviderProfileID),
		})
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "task_status", "task-status":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require daemon mode")
		}
		var args taskIDArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.TaskID == "" {
			return toolError(req.ID, "task_id is required")
		}
		task, err := s.tasker.TaskStatus(ctx, args.TaskID)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "list_tasks", "list-tasks":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require daemon mode")
		}
		var args listTasksArgs
		if len(params.Arguments) > 0 {
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				return fail(req.ID, -32602, err.Error())
			}
		}
		tasks, err := s.tasker.ListTasks(ctx, args.Limit)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(map[string]any{"tasks": tasks}), false))
	case "cancel_task", "cancel-task":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require daemon mode")
		}
		var args taskIDArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.TaskID == "" {
			return toolError(req.ID, "task_id is required")
		}
		task, err := s.tasker.CancelTask(ctx, args.TaskID)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "list_workspaces", "list-workspaces":
		if s.inspector == nil {
			return toolError(req.ID, "workspace status tools require daemon mode")
		}
		statuses, err := s.inspector.ListWorkspaceStatuses(ctx)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(map[string]any{"workspaces": statuses}), false))
	case "workspace_status", "workspace-status":
		if s.inspector == nil {
			return toolError(req.ID, "workspace status tools require daemon mode")
		}
		var args syncArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		status, err := s.workspaceStatus(ctx, args.DirectoryPath, args.ProviderProfileID)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(status), false))
	default:
		return toolError(req.ID, "unknown tool: "+params.Name)
	}
}

type multiRetrievalResult struct {
	DirectoryPath string
	Text          string
	Summary       string
	Error         string
}

func (s *Server) retrieve(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int) (workspace.Result, error) {
	providerProfileID = strings.TrimSpace(providerProfileID)
	if providerProfileID == "" {
		return s.syncer.Retrieve(ctx, dir, query, maxOutputLen)
	}
	providerSyncer, ok := s.syncer.(ProviderSyncer)
	if !ok {
		return workspace.Result{}, fmt.Errorf("provider_profile_id is not supported by this openACE mode")
	}
	return providerSyncer.RetrieveWithProvider(ctx, dir, providerProfileID, query, maxOutputLen)
}

func (s *Server) syncWorkspace(ctx context.Context, dir string, providerProfileID string) (workspace.Result, error) {
	providerProfileID = strings.TrimSpace(providerProfileID)
	if providerProfileID == "" {
		return s.syncer.Sync(ctx, dir)
	}
	providerSyncer, ok := s.syncer.(ProviderSyncer)
	if !ok {
		return workspace.Result{}, fmt.Errorf("provider_profile_id is not supported by this openACE mode")
	}
	return providerSyncer.SyncWithProvider(ctx, dir, providerProfileID)
}

func (s *Server) workspaceStatus(ctx context.Context, dir string, providerProfileID string) (workspace.WorkspaceStatus, error) {
	providerProfileID = strings.TrimSpace(providerProfileID)
	if providerProfileID == "" {
		return s.inspector.WorkspaceStatus(ctx, dir)
	}
	providerInspector, ok := s.inspector.(ProviderWorkspaceInspector)
	if !ok {
		return workspace.WorkspaceStatus{}, fmt.Errorf("provider_profile_id is not supported by this openACE mode")
	}
	return providerInspector.WorkspaceStatusWithProvider(ctx, dir, providerProfileID)
}

func normalizeDirectoryPaths(paths []string) ([]string, error) {
	normalized := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		normalized = append(normalized, path)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("directory_paths is required")
	}
	if len(normalized) > maxMultiWorkspacePaths {
		return nil, fmt.Errorf("directory_paths supports at most %d workspaces", maxMultiWorkspacePaths)
	}
	return normalized, nil
}

func (s *Server) retrieveMultiple(ctx context.Context, paths []string, providerProfileID string, query string, maxOutputLen int) []multiRetrievalResult {
	results := make([]multiRetrievalResult, len(paths))
	var wg sync.WaitGroup
	for i, path := range paths {
		i, path := i, path
		results[i].DirectoryPath = path
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.retrieve(ctx, path, providerProfileID, query, maxOutputLen)
			if err != nil {
				results[i].Error = err.Error()
				return
			}
			text := strings.TrimSpace(result.Text)
			if text == "" {
				text = "No relevant code sections were found."
			}
			results[i].Text = text
			results[i].Summary = result.Summary()
		}()
	}
	wg.Wait()
	return results
}

func formatMultiRetrievalResults(results []multiRetrievalResult) string {
	var out strings.Builder
	out.WriteString("Cross-workspace retrieval results")
	for _, result := range results {
		out.WriteString("\n\n## ")
		out.WriteString(result.DirectoryPath)
		out.WriteString("\n")
		if result.Error != "" {
			out.WriteString("ERROR: ")
			out.WriteString(result.Error)
			continue
		}
		out.WriteString(result.Text)
		if result.Summary != "" {
			out.WriteString("\n\n")
			out.WriteString(result.Summary)
		}
	}
	return out.String()
}

func allMultiRetrievalsFailed(results []multiRetrievalResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if result.Error == "" {
			return false
		}
	}
	return true
}

func retrievalTool() map[string]any {
	return map[string]any{
		"name":        "codebase_retrieval",
		"description": "Query the current codebase through the Augment ACE retrieval flow.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string"},
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_path"},
		},
	}
}

func multiRetrievalTool() map[string]any {
	return map[string]any{
		"name":        "multi_codebase_retrieval",
		"description": "Query multiple explicit workspaces independently through ACE and return per-workspace results.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string"},
				"directory_paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_paths"},
		},
	}
}

func syncTool() map[string]any {
	return map[string]any{
		"name":        "sync_workspace",
		"description": "Scan, upload missing blobs, and checkpoint a workspace before retrieval.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
			},
			"required": []string{"directory_path"},
		},
	}
}

func startRetrievalTool() map[string]any {
	return map[string]any{
		"name":        "start_codebase_retrieval",
		"description": "Submit an asynchronous ACE codebase retrieval task to the local openACE daemon.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string"},
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_path"},
		},
	}
}

func startMultiRetrievalTool() map[string]any {
	return map[string]any{
		"name":        "start_multi_codebase_retrieval",
		"description": "Submit an asynchronous retrieval task for multiple explicit workspaces to the local openACE daemon.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string"},
				"directory_paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_paths"},
		},
	}
}

func startSyncTool() map[string]any {
	return map[string]any{
		"name":        "start_sync_workspace",
		"description": "Submit an asynchronous workspace sync task to the local openACE daemon.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
			},
			"required": []string{"directory_path"},
		},
	}
}

func taskStatusTool() map[string]any {
	return map[string]any{
		"name":        "task_status",
		"description": "Get status and result for an openACE daemon task.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string"},
			},
			"required": []string{"task_id"},
		},
	}
}

func listTasksTool() map[string]any {
	return map[string]any{
		"name":        "list_tasks",
		"description": "List recent openACE daemon tasks for diagnostics and pressure-test observation.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer"},
			},
		},
	}
}

func cancelTaskTool() map[string]any {
	return map[string]any{
		"name":        "cancel_task",
		"description": "Cancel a queued or running openACE daemon task.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string"},
			},
			"required": []string{"task_id"},
		},
	}
}

func listWorkspacesTool() map[string]any {
	return map[string]any{
		"name":        "list_workspaces",
		"description": "List workspace states currently known by the local openACE daemon.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func workspaceStatusTool() map[string]any {
	return map[string]any{
		"name":        "workspace_status",
		"description": "Get checkpoint, file count, sync stage, watcher state, and last error for a workspace known by the local openACE daemon.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional ACE provider profile ID. Omit to use the daemon default provider state."},
			},
			"required": []string{"directory_path"},
		},
	}
}

func ok(id *json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func fail(id *json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func toolError(id *json.RawMessage, message string) rpcResponse {
	return ok(id, toolResult(message, true))
}

func toolResult(text string, isError bool) map[string]any {
	result := map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	}
	if isError {
		result["isError"] = true
	}
	return result
}

func jsonText(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(data)
}
