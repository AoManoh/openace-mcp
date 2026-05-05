package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Syncer interface {
	Retrieve(context.Context, string, string, int) (workspace.Result, error)
	Sync(context.Context, string) (workspace.Result, error)
}

type Tasker interface {
	CancelTask(context.Context, string) (daemon.TaskSnapshot, error)
	ListTasks(context.Context, int) ([]daemon.TaskSnapshot, error)
	StartTask(context.Context, daemon.TaskRequest) (daemon.TaskSnapshot, error)
	TaskStatus(context.Context, string) (daemon.TaskSnapshot, error)
}

type Server struct {
	syncer Syncer
	tasker Tasker
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
	MaxOutputLength    int    `json:"max_output_length,omitempty"`
}

type syncArgs struct {
	DirectoryPath string `json:"directory_path"`
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
		tools := []any{retrievalTool(), syncTool()}
		if s.tasker != nil {
			tools = append(tools, startRetrievalTool(), startSyncTool(), taskStatusTool(), listTasksTool(), cancelTaskTool())
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
		result, err := s.syncer.Retrieve(ctx, args.DirectoryPath, args.InformationRequest, args.MaxOutputLength)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		text := strings.TrimSpace(result.Text)
		if text == "" {
			text = "No relevant code sections were found."
		}
		return ok(req.ID, toolResult(text+"\n\n"+result.Summary(), false))
	case "sync_workspace", "sync-workspace":
		var args syncArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		result, err := s.syncer.Sync(ctx, args.DirectoryPath)
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult("Workspace synced.\n"+result.Summary(), false))
	case "start_codebase_retrieval", "start-codebase-retrieval":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require OPENACE_DAEMON_ADDR")
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
			InformationRequest: args.InformationRequest,
			MaxOutputLength:    args.MaxOutputLength,
		})
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "start_sync_workspace", "start-sync-workspace":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require OPENACE_DAEMON_ADDR")
		}
		var args syncArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return fail(req.ID, -32602, err.Error())
		}
		if args.DirectoryPath == "" {
			return toolError(req.ID, "directory_path is required")
		}
		task, err := s.tasker.StartTask(ctx, daemon.TaskRequest{
			Kind:          daemon.TaskKindSync,
			DirectoryPath: args.DirectoryPath,
		})
		if err != nil {
			return toolError(req.ID, err.Error())
		}
		return ok(req.ID, toolResult(jsonText(task), false))
	case "task_status", "task-status":
		if s.tasker == nil {
			return toolError(req.ID, "task tools require OPENACE_DAEMON_ADDR")
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
			return toolError(req.ID, "task tools require OPENACE_DAEMON_ADDR")
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
			return toolError(req.ID, "task tools require OPENACE_DAEMON_ADDR")
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
	default:
		return toolError(req.ID, "unknown tool: "+params.Name)
	}
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
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_path"},
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
				"directory_path": map[string]any{"type": "string"},
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
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_path"},
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
				"directory_path": map[string]any{"type": "string"},
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
