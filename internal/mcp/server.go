package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/daemon"
	"github.com/AoManoh/openace-mcp/internal/engine"
)

const maxMultiWorkspacePaths = daemon.MaxMultiWorkspacePaths
const defaultToolTimeout = 110 * time.Second

// Tasker 描述 daemon 任务面能力；只有 daemon client 实现。
type Tasker interface {
	CancelTask(context.Context, string) (daemon.TaskSnapshot, error)
	ListTasks(context.Context, int) ([]daemon.TaskSnapshot, error)
	StartTask(context.Context, daemon.TaskRequest) (daemon.TaskSnapshot, error)
	TaskStatus(context.Context, string) (daemon.TaskSnapshot, error)
}

// DaemonStatuser 描述 daemon 状态查询能力；只有 daemon client 实现。
type DaemonStatuser interface {
	DaemonStatus(context.Context) (daemon.Status, error)
}

type Server struct {
	service   engine.Service
	tasker    Tasker
	inspector engine.WorkspaceInspector
	statuser  DaemonStatuser
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

func NewServer(service engine.Service) *Server {
	server := &Server{service: service}
	if tasker, ok := service.(Tasker); ok {
		server.tasker = tasker
	}
	// P9(review 2026-08-06):inspector 注册不再以 tasker 存在为前提——
	// 该前提源于"与 legacy direct 对齐",legacy 已在 Stage 7 删除。
	// direct 模式恢复 workspace_status/list_workspaces 只读状态面,
	// 语义覆盖缺口在两种形态下都有处可查。
	if inspector, ok := service.(engine.WorkspaceInspector); ok {
		server.inspector = inspector
	}
	if statuser, ok := service.(DaemonStatuser); ok {
		server.statuser = statuser
	}
	return server
}

// maxInputLineBytes 是单条 MCP 输入行上限(与历史 Scanner 缓冲一致)。
const maxInputLineBytes = 8 * 1024 * 1024

func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 64*1024)
	enc := json.NewEncoder(out)

	for {
		line, tooLong, readErr := readInputLine(reader, maxInputLineBytes)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if tooLong {
			// P13(review 二批):此前 Scanner 超限 ErrTooLong 直接终结
			// 整个会话进程;超长参数只应得到协议错误,会话继续。
			resp := rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32700, Message: fmt.Sprintf("input line exceeds %d bytes and was discarded", maxInputLineBytes)}}
			if err := enc.Encode(resp); err != nil {
				return err
			}
		} else if line = strings.TrimSpace(line); line != "" {
			var req rpcRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				resp := rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32700, Message: err.Error()}}
				if err := enc.Encode(resp); err != nil {
					return err
				}
			} else if req.ID == nil {
				s.handleNotification(req)
			} else {
				resp := s.handle(ctx, req)
				if err := enc.Encode(resp); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			return nil
		}
	}
}

// readInputLine 读取一行(上限 limit 字节,含换行符);超限时丢弃该行
// 剩余字节并报告 tooLong,由调用方回协议错误后继续会话。EOF 随最后
// 一行(可无换行)一并返回 io.EOF。
func readInputLine(reader *bufio.Reader, limit int) (string, bool, error) {
	var buf []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(buf)+len(fragment) > limit {
			// 丢弃当前行剩余字节直到换行或 EOF。
			for {
				if err == nil {
					return "", true, nil
				}
				if !errors.Is(err, bufio.ErrBufferFull) {
					return "", true, err
				}
				_, err = reader.ReadSlice('\n')
			}
		}
		buf = append(buf, fragment...)
		if err == nil {
			return string(buf), false, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(buf), false, err
	}
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
			// instructions 是 caller LLM 的检索使用引导(方案③,E4 变体
			// V2 冻结稿):仅增强字段,旧客户端忽略,零 wire 变更。
			"instructions": serverInstructions,
		})
	case "tools/list":
		tools := []any{retrievalTool(), multiRetrievalTool(), syncTool()}
		if s.tasker != nil {
			tools = append(tools, startRetrievalTool(), startMultiRetrievalTool(), startSyncTool(), taskStatusTool(), listTasksTool(), cancelTaskTool())
		}
		if s.statuser != nil {
			tools = append(tools, daemonStatusTool())
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

// callTool 经 handler 表分发(L16,诊断 2026-08-03:257 行巨型 switch
// 每加一个工具都在同一函数膨胀)。工具名统一把 '-' 归一为 '_' 后查表,
// 行为与原 switch 逐字节一致。
func (s *Server) callTool(ctx context.Context, req rpcRequest) rpcResponse {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fail(req.ID, -32602, err.Error())
	}
	name := strings.ReplaceAll(params.Name, "-", "_")
	handler, ok := s.toolHandlers()[name]
	if !ok {
		return toolError(req.ID, "unknown tool: "+params.Name)
	}
	return handler(ctx, req.ID, params.Arguments)
}

type toolHandler func(ctx context.Context, id *json.RawMessage, args json.RawMessage) rpcResponse

// toolHandlers 返回工具名 → handler 映射(可用性守卫在各 handler 内,
// 与 tools/list 的能力暴露一致)。
func (s *Server) toolHandlers() map[string]toolHandler {
	return map[string]toolHandler{
		"codebase_retrieval":             s.handleRetrieval,
		"multi_codebase_retrieval":       s.handleMultiRetrieval,
		"sync_workspace":                 s.handleSyncWorkspace,
		"start_codebase_retrieval":       s.handleStartRetrieval,
		"start_multi_codebase_retrieval": s.handleStartMultiRetrieval,
		"start_sync_workspace":           s.handleStartSync,
		"task_status":                    s.handleTaskStatus,
		"list_tasks":                     s.handleListTasks,
		"cancel_task":                    s.handleCancelTask,
		"daemon_status":                  s.handleDaemonStatus,
		"list_workspaces":                s.handleListWorkspaces,
		"workspace_status":               s.handleWorkspaceStatus,
	}
}

func (s *Server) handleRetrieval(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	var args retrievalArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	if err := normalizeRetrievalArgs(&args); err != nil {
		return toolError(id, err.Error())
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	result, err := s.retrieve(toolCtx, args.DirectoryPath, args.ProviderProfileID, args.InformationRequest, args.MaxOutputLength)
	if err != nil {
		return toolError(id, err.Error())
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "No relevant code sections were found."
	}
	rendered := text + "\n\n" + result.Summary()
	if structured := retrievalStructured(result); structured != nil {
		return ok(id, toolResultWithStructured(rendered, false, structured))
	}
	return ok(id, toolResult(rendered, false))
}

// retrievalStructured 构造单仓检索的 structuredContent(P1,review 二批:
// 同一次检索经 start_codebase_retrieval→task_status 能拿到全部透明性
// 字段,同步路径却只回文本——决策 11 字段在主工具面丢失)。键名与
// task 快照里 engine.Result 的 JSON tag 逐一对应。local-hybrid 恒有
// engine/index_revision;透明性字段按 K32/K34 仅在 provider 配置或降级
// 时出现。字段全空(legacy 形状结果)时返回 nil 不附着,wire 加性。
func retrievalStructured(result engine.Result) map[string]any {
	fields := map[string]any{}
	if result.Engine != "" {
		fields["engine"] = result.Engine
	}
	if result.IndexRevision != "" {
		fields["index_revision"] = result.IndexRevision
	}
	if result.RetrievalMode != "" {
		fields["retrieval_mode"] = result.RetrievalMode
	}
	if result.DegradedReason != "" {
		fields["degraded_reason"] = result.DegradedReason
	}
	if result.SemanticCoverage != "" {
		fields["semantic_coverage"] = result.SemanticCoverage
	}
	if result.RerankSent > 0 {
		fields["rerank_sent"] = result.RerankSent
	}
	if result.QueryPlan != "" {
		fields["query_plan"] = result.QueryPlan
	}
	if result.QueryEmbedFailed {
		fields["query_embed_failed"] = true
	}
	if result.EmbeddingProfile != "" {
		fields["embedding_profile"] = result.EmbeddingProfile
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func (s *Server) handleMultiRetrieval(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	var args multiRetrievalArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.InformationRequest = strings.TrimSpace(args.InformationRequest)
	args.ProviderProfileID = strings.TrimSpace(args.ProviderProfileID)
	if args.InformationRequest == "" {
		return toolError(id, "information_request is required")
	}
	if err := validateMaxOutputLength(args.MaxOutputLength); err != nil {
		return toolError(id, err.Error())
	}
	paths, err := normalizeDirectoryPaths(args.DirectoryPaths)
	if err != nil {
		return toolError(id, err.Error())
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	results := s.retrieveMultiple(toolCtx, paths, args.ProviderProfileID, args.InformationRequest, args.MaxOutputLength)
	status := summarizeMultiRetrievalResults(args.ProviderProfileID, results)
	text := formatMultiRetrievalResults(results, status)
	structured := map[string]any{"multi_status": status}
	if err := toolCtx.Err(); err != nil {
		return ok(id, toolResultWithStructured(text, true, structured))
	}
	if status.FailureCount > 0 {
		return ok(id, toolResultWithStructured(text, true, structured))
	}
	return ok(id, toolResultWithStructured(text, false, structured))
}

func (s *Server) handleSyncWorkspace(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	var args syncArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.DirectoryPath = strings.TrimSpace(args.DirectoryPath)
	args.ProviderProfileID = strings.TrimSpace(args.ProviderProfileID)
	if args.DirectoryPath == "" {
		return toolError(id, "directory_path is required")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	result, err := s.syncWorkspace(toolCtx, args.DirectoryPath, args.ProviderProfileID)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult("Workspace synced.\n"+result.Summary(), false))
}

func (s *Server) handleStartRetrieval(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.tasker == nil {
		return toolError(id, "task tools require daemon mode")
	}
	var args retrievalArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	if err := normalizeRetrievalArgs(&args); err != nil {
		return toolError(id, err.Error())
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	task, err := s.tasker.StartTask(toolCtx, daemon.TaskRequest{
		Kind:               daemon.TaskKindRetrieve,
		DirectoryPath:      args.DirectoryPath,
		ProviderProfileID:  args.ProviderProfileID,
		InformationRequest: args.InformationRequest,
		MaxOutputLength:    args.MaxOutputLength,
	})
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(task), false))
}

func (s *Server) handleStartMultiRetrieval(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.tasker == nil {
		return toolError(id, "task tools require daemon mode")
	}
	var args multiRetrievalArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.InformationRequest = strings.TrimSpace(args.InformationRequest)
	args.ProviderProfileID = strings.TrimSpace(args.ProviderProfileID)
	if args.InformationRequest == "" {
		return toolError(id, "information_request is required")
	}
	if err := validateMaxOutputLength(args.MaxOutputLength); err != nil {
		return toolError(id, err.Error())
	}
	paths, err := normalizeDirectoryPaths(args.DirectoryPaths)
	if err != nil {
		return toolError(id, err.Error())
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	task, err := s.tasker.StartTask(toolCtx, daemon.TaskRequest{
		Kind:               daemon.TaskKindMultiRetrieve,
		DirectoryPaths:     paths,
		ProviderProfileID:  args.ProviderProfileID,
		InformationRequest: args.InformationRequest,
		MaxOutputLength:    args.MaxOutputLength,
	})
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(task), false))
}

func (s *Server) handleStartSync(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.tasker == nil {
		return toolError(id, "task tools require daemon mode")
	}
	var args syncArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.DirectoryPath = strings.TrimSpace(args.DirectoryPath)
	args.ProviderProfileID = strings.TrimSpace(args.ProviderProfileID)
	if args.DirectoryPath == "" {
		return toolError(id, "directory_path is required")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	task, err := s.tasker.StartTask(toolCtx, daemon.TaskRequest{
		Kind:              daemon.TaskKindSync,
		DirectoryPath:     args.DirectoryPath,
		ProviderProfileID: args.ProviderProfileID,
	})
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(task), false))
}

func (s *Server) handleTaskStatus(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.tasker == nil {
		return toolError(id, "task tools require daemon mode")
	}
	var args taskIDArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return toolError(id, "task_id is required")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	task, err := s.tasker.TaskStatus(toolCtx, args.TaskID)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(task), false))
}

func (s *Server) handleListTasks(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.tasker == nil {
		return toolError(id, "task tools require daemon mode")
	}
	var args listTasksArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return fail(id, -32602, err.Error())
		}
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	tasks, err := s.tasker.ListTasks(toolCtx, args.Limit)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(map[string]any{"tasks": tasks}), false))
}

func (s *Server) handleCancelTask(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.tasker == nil {
		return toolError(id, "task tools require daemon mode")
	}
	var args taskIDArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.TaskID = strings.TrimSpace(args.TaskID)
	if args.TaskID == "" {
		return toolError(id, "task_id is required")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	task, err := s.tasker.CancelTask(toolCtx, args.TaskID)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(task), false))
}

func (s *Server) handleDaemonStatus(ctx context.Context, id *json.RawMessage, _ json.RawMessage) rpcResponse {
	if s.statuser == nil {
		return toolError(id, "daemon_status requires daemon mode")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	status, err := s.statuser.DaemonStatus(toolCtx)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(map[string]any{
		"mcp_build": buildinfo.Current(),
		"daemon":    status,
	}), false))
}

func (s *Server) handleListWorkspaces(ctx context.Context, id *json.RawMessage, _ json.RawMessage) rpcResponse {
	if s.inspector == nil {
		return toolError(id, "workspace status tools require daemon mode")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	statuses, err := s.inspector.ListWorkspaceStatuses(toolCtx)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(map[string]any{"workspaces": statuses}), false))
}

func (s *Server) handleWorkspaceStatus(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.inspector == nil {
		return toolError(id, "workspace status tools require daemon mode")
	}
	var args syncArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.DirectoryPath = strings.TrimSpace(args.DirectoryPath)
	args.ProviderProfileID = strings.TrimSpace(args.ProviderProfileID)
	if args.DirectoryPath == "" {
		return toolError(id, "directory_path is required")
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	status, err := s.workspaceStatus(toolCtx, args.DirectoryPath, args.ProviderProfileID)
	if err != nil {
		return toolError(id, err.Error())
	}
	return ok(id, toolResult(jsonText(status), false))
}

type multiRetrievalResult struct {
	DirectoryPath string
	Text          string
	Summary       string
	Error         string
	// 每仓降级透明性素材(P2):进 multi_status 结构化条目,文本被
	// 截断时仍可获取。
	RetrievalMode    string
	DegradedReason   string
	SemanticCoverage string
}

func (s *Server) retrieve(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int) (engine.Result, error) {
	return s.service.Search(ctx, engine.SearchRequest{
		Workspace: engine.WorkspaceRef{
			DirectoryPath:     dir,
			ProviderProfileID: strings.TrimSpace(providerProfileID),
		},
		Query:        query,
		MaxOutputLen: maxOutputLen,
	})
}

func (s *Server) syncWorkspace(ctx context.Context, dir string, providerProfileID string) (engine.Result, error) {
	return s.service.Sync(ctx, engine.SyncRequest{Workspace: engine.WorkspaceRef{
		DirectoryPath:     dir,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	}})
}

func (s *Server) workspaceStatus(ctx context.Context, dir string, providerProfileID string) (engine.WorkspaceStatus, error) {
	return s.inspector.WorkspaceStatus(ctx, engine.WorkspaceRef{
		DirectoryPath:     dir,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	})
}

func normalizeRetrievalArgs(args *retrievalArgs) error {
	args.InformationRequest = strings.TrimSpace(args.InformationRequest)
	args.DirectoryPath = strings.TrimSpace(args.DirectoryPath)
	args.ProviderProfileID = strings.TrimSpace(args.ProviderProfileID)
	if args.InformationRequest == "" {
		return fmt.Errorf("information_request is required")
	}
	if args.DirectoryPath == "" {
		return fmt.Errorf("directory_path is required")
	}
	return validateMaxOutputLength(args.MaxOutputLength)
}

func validateMaxOutputLength(value int) error {
	if value < 0 {
		return fmt.Errorf("max_output_length must be non-negative")
	}
	if value > daemon.MaxOutputLengthLimit {
		return fmt.Errorf("max_output_length must be <= %d", daemon.MaxOutputLengthLimit)
	}
	return nil
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
			results[i].RetrievalMode = result.RetrievalMode
			results[i].DegradedReason = result.DegradedReason
			results[i].SemanticCoverage = result.SemanticCoverage
		}()
	}
	wg.Wait()
	return results
}

func summarizeMultiRetrievalResults(providerProfileID string, results []multiRetrievalResult) engine.MultiRetrievalStatus {
	status := engine.MultiRetrievalStatus{
		ProviderProfileID: strings.TrimSpace(providerProfileID),
		TotalWorkspaces:   len(results),
		Workspaces:        make([]engine.MultiWorkspaceStatus, 0, len(results)),
	}
	for _, result := range results {
		item := engine.MultiWorkspaceStatus{
			DirectoryPath: result.DirectoryPath,
			Status:        "success",
		}
		if result.Error != "" {
			item.Status = "error"
			item.Error = result.Error
			status.FailureCount++
		} else {
			status.SuccessCount++
			// P2:每仓降级字段进结构化条目;降级仓单独计数。
			item.RetrievalMode = result.RetrievalMode
			item.DegradedReason = result.DegradedReason
			item.SemanticCoverage = result.SemanticCoverage
			if result.DegradedReason != "" {
				status.DegradedCount++
			}
		}
		status.Workspaces = append(status.Workspaces, item)
	}
	status.PartialFailure = status.SuccessCount > 0 && status.FailureCount > 0
	return status
}

func formatMultiRetrievalResults(results []multiRetrievalResult, status engine.MultiRetrievalStatus) string {
	var out strings.Builder
	// P2:有降级仓时聚合文本以 [DEGRADED] 汇总横幅开头(单仓首行横幅
	// 的心智模型;每仓横幅仍在各自小节内)。
	out.WriteString(status.DegradedBanner())
	out.WriteString("Cross-workspace retrieval results")
	if status.FailureCount > 0 {
		out.WriteString(fmt.Sprintf("\nWARNING: %d of %d workspaces failed; successful results are partial.", status.FailureCount, status.TotalWorkspaces))
	}
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

// serverInstructions/工具与参数描述是方案③(2026-08-02 批准)的冻结文案,
// 来源 E4 变体 V2(docs/benchmarks/work/e4/variant-v2.json);provider 中立、
// 引擎中立,≤1024 字符约束由单测钉住。
const serverInstructions = "openACE serves code retrieval for this workspace. For vague user asks, decompose into focused retrieval intents and call codebase_retrieval once per intent before answering. Keep the user's distinctive terminology in requests; add known identifiers and file-type hints. If results look degraded (a [DEGRADED] banner appears), tell the user retrieval quality is reduced and why."

const retrievalDescription = "Search the local codebase index (hybrid lexical + semantic retrieval with reranking). Use this tool FIRST whenever you need to find code, configuration, tests, or documentation in the workspace before answering or editing. One focused intent per call; issue multiple calls for multi-part tasks."

const informationRequestDescription = "A complete, specific description of what to find. Include: (1) the purpose or behavior you seek, in a full sentence; (2) exact identifiers if known (function/class/config key names, keep original casing); (3) artifact type hints when relevant (config file, test, docs). Preserve distinctive terms from the user's request verbatim; do not translate identifiers. Good: \"where is the retry backoff policy for embedding provider requests implemented\". Bad: \"retry\"."

func retrievalTool() map[string]any {
	return map[string]any{
		"name":        "codebase_retrieval",
		"description": retrievalDescription,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string", "description": informationRequestDescription},
				"directory_path":      map[string]any{"type": "string", "description": "Absolute path of the workspace root to search."},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_path"},
		},
	}
}

func multiRetrievalTool() map[string]any {
	return map[string]any{
		"name":        "multi_codebase_retrieval",
		"description": "Search multiple explicit workspaces independently with the same request and return per-workspace results. Use codebase_retrieval instead when only one workspace is involved.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string", "description": informationRequestDescription},
				"directory_paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
				"max_output_length":   map[string]any{"type": "integer"},
			},
			"required": []string{"information_request", "directory_paths"},
		},
	}
}

func syncTool() map[string]any {
	return map[string]any{
		"name":        "sync_workspace",
		"description": "Scan and index a workspace so retrieval reflects the latest file state. Retrieval tools sync automatically; call this only to warm up a workspace ahead of time.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
			},
			"required": []string{"directory_path"},
		},
	}
}

func startRetrievalTool() map[string]any {
	return map[string]any{
		"name":        "start_codebase_retrieval",
		"description": "Submit an asynchronous codebase retrieval task to the local openACE daemon.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"information_request": map[string]any{"type": "string", "description": informationRequestDescription},
				"directory_path":      map[string]any{"type": "string"},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
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
				"information_request": map[string]any{"type": "string", "description": informationRequestDescription},
				"directory_paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
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
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
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
		"description": "List recent openACE daemon tasks for diagnostics and pressure-test observation. Result text is omitted in this list view (entries set result_text_omitted); use task_status to fetch the full text of a task.",
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

func daemonStatusTool() map[string]any {
	return map[string]any{
		"name":        "daemon_status",
		"description": "Show the MCP wrapper and openACE daemon runtime identity, build revision, cache namespace, and capabilities.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
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
				"provider_profile_id": map[string]any{"type": "string", "description": "Optional provider profile ID (legacy engine only). Omit to use the daemon default provider state."},
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

func toolResultWithStructured(text string, isError bool, structured any) map[string]any {
	result := toolResult(text, isError)
	if structured != nil {
		result["structuredContent"] = structured
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
