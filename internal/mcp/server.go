package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Syncer interface {
	Retrieve(context.Context, string, string, int) (workspace.Result, error)
	Sync(context.Context, string) (workspace.Result, error)
}

type Server struct {
	syncer Syncer
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

func NewServer(syncer Syncer) *Server {
	return &Server{syncer: syncer}
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
		return ok(req.ID, map[string]any{"tools": []any{retrievalTool(), syncTool()}})
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
