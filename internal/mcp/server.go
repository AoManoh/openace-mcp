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
	mapper    engine.RepoMapper
	// unavailable 非 nil = 启动失败但会话保持的降级形态(灰度反馈一号
	// P1-3):initialize/tools/list 正常应答,任何 tools/call 返回失败
	// 原因与修复指引——build/profile mismatch 的可行动文案从 stderr
	// (多数 MCP 客户端不展示)搬进调用方看得见的工具错误。
	unavailable error
	// reconnect 是不可用形态的惰性重探测(灰度反馈三 C.4:判定只做
	// 一次会把会话永久锁死——坏 daemon 已清除、好 daemon 已就位,
	// 会话仍报旧错,只能重启)。按 reconnectTTL 节流:探测可能拉起
	// managed Connect(含 daemon spawn),不能每调用都做。
	reconnect   func() (engine.Service, error)
	reconnectAt time.Time
}

// reconnectTTL 是不可用形态重探测的最小间隔。
const reconnectTTL = 10 * time.Second

// NewUnavailableServer 构造启动失败降级形态的 MCP 服务(见
// Server.unavailable)。此前 Connect 失败直接 exit(1),客户端只见裸
// "Failed to connect",两轮灰度反馈均卡在此排障。reconnect 可为 nil
// (不重探测,始终返回 cause)。
func NewUnavailableServer(cause error, reconnect func() (engine.Service, error)) *Server {
	return &Server{unavailable: cause, reconnect: reconnect}
}

// attachService 装配服务与能力面(NewServer 与不可用形态自愈共用)。
func (s *Server) attachService(service engine.Service) {
	s.service = service
	if tasker, ok := service.(Tasker); ok {
		s.tasker = tasker
	}
	if inspector, ok := service.(engine.WorkspaceInspector); ok {
		s.inspector = inspector
	}
	if statuser, ok := service.(DaemonStatuser); ok {
		s.statuser = statuser
	}
	if mapper, ok := service.(engine.RepoMapper); ok {
		s.mapper = mapper
	}
}

// tryReconnect 在不可用形态下按 TTL 重探测;成功即本会话自愈,失败则
// 刷新失败原因(调用方看到的是最新一次探测的错误,不是启动期陈账)。
func (s *Server) tryReconnect() bool {
	if s.reconnect == nil || time.Since(s.reconnectAt) < reconnectTTL {
		return s.unavailable == nil
	}
	s.reconnectAt = time.Now()
	service, err := s.reconnect()
	if err != nil {
		s.unavailable = err
		return false
	}
	s.attachService(service)
	s.unavailable = nil
	return true
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
	// Detail 是输出详略(框架 18.2/S2,用户候选"路径+行号优先"的
	// 实验载体):full(默认,内容块)/paths(只回 path:range 头行,
	// 内容由调用方按需 Read)。
	Detail     string `json:"detail,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
}

type multiRetrievalArgs struct {
	InformationRequest string   `json:"information_request"`
	DirectoryPaths     []string `json:"directory_paths"`
	ProviderProfileID  string   `json:"provider_profile_id,omitempty"`
	MaxOutputLength    int      `json:"max_output_length,omitempty"`
}

type repoMapArgs struct {
	DirectoryPath   string `json:"directory_path"`
	MaxOutputLength int    `json:"max_output_length,omitempty"`
	Focus           string `json:"focus,omitempty"`
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
	server := &Server{}
	// P9(review 2026-08-06):inspector 注册不再以 tasker 存在为前提——
	// 该前提源于"与 legacy direct 对齐",legacy 已在 Stage 7 删除。
	// direct 模式恢复 workspace_status/list_workspaces 只读状态面,
	// 语义覆盖缺口在两种形态下都有处可查。
	server.attachService(service)
	return server
}

// maxInputLineBytes 是单条 MCP 输入行上限(与历史 Scanner 缓冲一致)。
const maxInputLineBytes = 8 * 1024 * 1024

// EnvMCPTools 支配 tools/list 暴露面(D2,吸收清单 codegraph #1:
// 多工具面导致弱 caller mis-pick,且每会话为用不到的 schema 白烧
// tokens——工具面就是提示工程)。未设/空 = 最小面(仅
// codebase_retrieval);"all" = 完整能力面;逗号清单 = 指定面(与
// 能力面取交集,同一配置可跨 direct/daemon 形态共用)。未列出的工具
// 保留 handler,按名调用仍被处理——只影响发现,不影响能力。
const EnvMCPTools = "OPENACE_MCP_TOOLS"

// resolveToolFace 解析工具面配置:返回 (指定面, 是否全量, 错误)。
// 未知工具名 fail-fast(防拼写错误静默缩面)。
func resolveToolFace() (map[string]bool, bool, error) {
	value := strings.TrimSpace(os.Getenv(EnvMCPTools))
	if value == "" {
		return map[string]bool{"codebase_retrieval": true}, false, nil
	}
	if strings.EqualFold(value, "all") {
		return nil, true, nil
	}
	known := knownToolNames()
	allow := make(map[string]bool)
	for _, part := range strings.Split(value, ",") {
		name := strings.ReplaceAll(strings.TrimSpace(part), "-", "_")
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, false, fmt.Errorf("%s: unknown tool %q; valid tools: %s (or \"all\")", EnvMCPTools, name, strings.Join(knownToolList(), ", "))
		}
		allow[name] = true
	}
	if len(allow) == 0 {
		return map[string]bool{"codebase_retrieval": true}, false, nil
	}
	return allow, false, nil
}

// knownToolList 是全部工具名(与 toolHandlers 键一致,稳定序)。
func knownToolList() []string {
	return []string{
		"codebase_retrieval", "multi_codebase_retrieval", "sync_workspace",
		"start_codebase_retrieval", "start_multi_codebase_retrieval", "start_sync_workspace",
		"task_status", "list_tasks", "cancel_task",
		"daemon_status", "list_workspaces", "workspace_status",
		"repo_map",
	}
}

func knownToolNames() map[string]bool {
	names := make(map[string]bool)
	for _, name := range knownToolList() {
		names[name] = true
	}
	return names
}

func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	// D2 fail-fast:工具面配置非法即拒绝启动(会话起点可行动报错)。
	if _, _, err := resolveToolFace(); err != nil {
		return err
	}
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
			// 灰度反馈协议按 env 追加(EnvGrayFeedback)。
			"instructions": activeInstructions(),
		})
	case "tools/list":
		allow, all, err := resolveToolFace()
		if err != nil {
			return fail(req.ID, -32603, err.Error())
		}
		tools := []any{}
		for _, tool := range s.capabilityTools() {
			if all || allow[tool["name"].(string)] {
				tools = append(tools, tool)
			}
		}
		return ok(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		return s.callTool(ctx, req)
	default:
		return fail(req.ID, -32601, "method not found: "+req.Method)
	}
}

// capabilityTools 返回当前形态的完整能力面(能力守卫:task 面仅 daemon、
// 状态面按 statuser/inspector 注册);tools/list 在其上按 D2 工具面
// 配置过滤,handler 表不受影响。
func (s *Server) capabilityTools() []map[string]any {
	tools := []map[string]any{retrievalTool(), multiRetrievalTool(), syncTool()}
	if s.tasker != nil {
		tools = append(tools, startRetrievalTool(), startMultiRetrievalTool(), startSyncTool(), taskStatusTool(), listTasksTool(), cancelTaskTool())
	}
	if s.statuser != nil {
		tools = append(tools, daemonStatusTool())
	}
	if s.inspector != nil {
		tools = append(tools, listWorkspacesTool(), workspaceStatusTool())
	}
	if s.mapper != nil {
		tools = append(tools, repoMapTool())
	}
	return tools
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
	if s.unavailable != nil && !s.tryReconnect() {
		return toolError(req.ID, fmt.Sprintf("openACE is unavailable in this session: %v; fix the daemon or configuration and retry (the session re-probes automatically), or restart the MCP session", s.unavailable))
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
		"repo_map":                       s.handleRepoMap,
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
	result, err := s.retrieve(toolCtx, args.DirectoryPath, args.ProviderProfileID, args.InformationRequest, args.MaxOutputLength, args.Detail, args.PathPrefix)
	if err != nil {
		return toolError(id, err.Error())
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "No relevant code sections were found."
	}
	rendered := text + "\n\n" + result.Summary()
	if diagnostics := retrievalDiagnosticsText(result); diagnostics != "" {
		// 结构化字段的文本兜底(真实 Devin -p Agent A/B 2026-08-07:
		// MCP bridge 未向 Agent 展开 structuredContent,导致 timings/hits/
		// display 虽在 wire 中却不可见,灰度协议无法记录)。紧凑单行
		// 保证所有客户端都能诊断;structuredContent 仍是机器读主合同。
		rendered += "\n" + diagnostics
	}
	if structured := retrievalStructured(result); structured != nil {
		return ok(id, toolResultWithStructured(rendered, false, structured))
	}
	return ok(id, toolResult(rendered, false))
}

// handleRepoMap 是 repo_map R1(D4):orientation 面,快照只读。
func (s *Server) handleRepoMap(ctx context.Context, id *json.RawMessage, rawArgs json.RawMessage) rpcResponse {
	if s.mapper == nil {
		return toolError(id, "repo_map is not supported by this engine")
	}
	var args repoMapArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fail(id, -32602, err.Error())
	}
	args.DirectoryPath = strings.TrimSpace(args.DirectoryPath)
	if args.DirectoryPath == "" {
		return toolError(id, "directory_path is required")
	}
	if err := validateMaxOutputLength(args.MaxOutputLength); err != nil {
		return toolError(id, err.Error())
	}
	toolCtx, cancel := toolTimeoutContext(ctx)
	defer cancel()
	result, err := s.mapper.RepoMap(toolCtx, engine.RepoMapRequest{
		Workspace:    engine.WorkspaceRef{DirectoryPath: args.DirectoryPath},
		MaxOutputLen: args.MaxOutputLength,
		Focus:        args.Focus,
	})
	if err != nil {
		return toolError(id, err.Error())
	}
	text := result.Text
	if diagnostics := retrievalDiagnosticsText(result); diagnostics != "" {
		text += "\n" + diagnostics
	}
	if structured := retrievalStructured(result); structured != nil {
		return ok(id, toolResultWithStructured(text, false, structured))
	}
	return ok(id, toolResult(text, false))
}

// retrievalDiagnosticsText 把关键结构化诊断投影为一行文本兜底。
// 不含查询/内容/绝对路径,仅计时与计数(隐私面不扩张)。
func retrievalDiagnosticsText(result engine.Result) string {
	var parts []string
	if result.RetrievalMode != "" {
		parts = append(parts, "mode="+result.RetrievalMode)
	}
	if result.RerankSent > 0 {
		parts = append(parts, fmt.Sprintf("rerank_sent=%d", result.RerankSent))
	}
	if result.Timings != nil {
		t := result.Timings
		parts = append(parts, fmt.Sprintf("timings_ms[total=%d sync=%d lexical=%d embed=%d vector=%d fuse=%d rerank=%d render=%d]",
			t.TotalMs, t.SyncMs, t.LexicalMs, t.QueryEmbedMs, t.VectorMs, t.FuseMs, t.RerankMs, t.RenderMs))
	}
	if result.Display != nil {
		d := result.Display
		parts = append(parts, fmt.Sprintf("display[candidates=%d shown=%d files=%d truncated=%t]",
			d.CandidateBlocks, d.ShownBlocks, d.ShownFiles, d.Truncated))
	}
	if len(parts) == 0 {
		return ""
	}
	return "diagnostics: " + strings.Join(parts, " ")
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
	if result.Timings != nil {
		// 阶段耗时(框架 18.3):调用方可归因延迟(sync/embed/rerank/
		// render),灰度"13.1s 不知慢在哪"从此有数。
		fields["timings"] = result.Timings
	}
	if len(result.Hits) > 0 {
		// 结构化 hits 清单与展示统计(框架 18.2):候选到交付的完整性
		// 机器可读,未展示 hit 可定向 Read 续取。
		fields["hits"] = result.Hits
	}
	if result.Display != nil {
		fields["display"] = result.Display
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
	text := "Workspace synced.\n" + result.Summary()
	return ok(id, toolResultWithStructured(text, false, syncStructured(result)))
}

func syncStructured(result engine.Result) map[string]any {
	fields := map[string]any{
		"engine":         result.Engine,
		"index_revision": result.IndexRevision,
		"file_count":     result.FileCount,
		"added_chunks":   result.Added,
		"deleted":        result.Deleted,
	}
	if result.BuildMode != "" {
		fields["build_mode"] = result.BuildMode
	}
	if result.CrossProfileReused > 0 {
		fields["cross_profile_reused"] = result.CrossProfileReused
	}
	if result.SemanticCoverage != "" {
		fields["semantic_coverage"] = result.SemanticCoverage
	}
	if result.DegradedReason != "" {
		fields["degraded_reason"] = result.DegradedReason
	}
	return fields
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
		Detail:             args.Detail,
		PathPrefix:         args.PathPrefix,
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

func (s *Server) retrieve(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int, detail string, pathPrefix string) (engine.Result, error) {
	return s.service.Search(ctx, engine.SearchRequest{
		Workspace: engine.WorkspaceRef{
			DirectoryPath:     dir,
			ProviderProfileID: strings.TrimSpace(providerProfileID),
		},
		Query:        query,
		MaxOutputLen: maxOutputLen,
		Detail:       detail,
		PathPrefix:   pathPrefix,
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
			result, err := s.retrieve(ctx, path, providerProfileID, query, maxOutputLen, "", "")
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

// EnvGrayFeedback 开启灰度反馈协议(用户明示 2026-08-07:灰度测试者
// 自愿参与、事先知情,报告详尽优先——token 成本与隐私顾虑明确豁免)。
// 开启后 instructions 追加诊断报告规范,调用 AI 在任务收尾给用户交付
// 答案时一次性汇总本次任务全部调用的多维事实报告,由用户回传维护者。
// 产品默认关闭,冻结文案不变。
const EnvGrayFeedback = "OPENACE_GRAY_FEEDBACK"

// grayFeedbackInstructions 是灰度反馈协议文案:维度=事实(结构化字段
// 原样抄录)/效果(预期 vs 实得,含排序问题)/体验(摩擦点逐条)/
// 时间(观测耗时)/bug(最小复现,不可复现则记触发场景)。时机=对话
// 收尾一次性汇总(用户裁决 2026-08-07:逐调用输出会打断任务推进,
// 正确位置是给用户交付答案时顺带输出)。
const grayFeedbackInstructions = "GRAY-TEST FEEDBACK PROTOCOL (this deployment opted in; verbose reports are expected and token cost is accepted): when you deliver your final answer to the user, append ONE consolidated diagnostic report covering every openACE tool call made during the task. Do NOT interrupt the task to report per call; collect silently and report once at the end. For EACH call include:\n" +
	"## openACE 调用诊断报告\n" +
	"- call: tool name; workspace path; the information_request verbatim; approximate wall time you observed\n" +
	"- outcome: hit / partial / miss — name the files or symbols you expected versus what was returned; call out ranking problems (target below noise), missing expected results, and truncation\n" +
	"- facts: copy retrieval_mode, semantic_coverage, degraded_reason, rerank_sent, query_plan, index_revision and any [DEGRADED] banner or truncation marker from the result verbatim; on failure copy the exact error text\n" +
	"- experience: setup, latency, output-format friction — one factual sentence each; rate this call 1-5 for usefulness to your actual task\n" +
	"- bug: if anything misbehaved, give a minimal reproduction (workspace state, query, exact call sequence); if not reproducible, record the exact trigger context instead\n" +
	"Be exhaustive and factual; report failures without softening. Never skip the end-of-task report, even when every call succeeded."

// activeInstructions 组装 initialize 下发的 instructions:默认=冻结稿,
// 灰度开关追加反馈协议。
func activeInstructions() string {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(EnvGrayFeedback))) {
	case "1", "true", "on", "yes":
		return serverInstructions + "\n\n" + grayFeedbackInstructions
	}
	return serverInstructions
}

const retrievalDescription = "Search the local codebase index (hybrid lexical + semantic retrieval with reranking). Use this tool FIRST whenever you need to find code, configuration, tests, or documentation in the workspace before answering or editing. One focused intent per call; issue multiple calls for multi-part tasks. File selection honors .gitignore and .openaceignore: a directory that never appears in any result is usually excluded there (add a !pattern in .openaceignore to re-include gitignored paths)."

func repoMapTool() map[string]any {
	return map[string]any{
		"name":        "repo_map",
		"description": repoMapDescription,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"directory_path":    map[string]any{"type": "string", "description": "Absolute path of the workspace to map."},
				"max_output_length": map[string]any{"type": "integer", "description": maxOutputLengthDescription},
				"focus":             map[string]any{"type": "string", "description": "Optional path prefix; map only that subtree (drill down from a full-repo map)."},
			},
			"required": []string{"directory_path"},
		},
	}
}

// repoMapDescription:orientation 面(D4 R1)——检索之前先认路。
const repoMapDescription = "Budget-bounded map of the indexed repository: directories with per-file key symbols and line spans, importance-ranked (symbol density, tests/vendored deprioritized, every top-level directory represented). Read-only snapshot of the active index revision — never triggers indexing or provider calls; returns index_not_ready on a cold workspace. Use it to orient before searching, then follow up with codebase_retrieval or Read."

// pathPrefixDescription 是 repo_map 定向后的子树检索约束(真实 Devin
// A/B:两臂都已知 internal/localengine,但根研究文档/tests 仍挤占生产
// 定义;显式前缀可把 rerank/输出预算留给目标子树)。
const pathPrefixDescription = "Optional indexed relative path prefix (for example `internal/localengine`). Filters fused candidates before reranking; use after repo_map identifies the relevant subtree. Omit for repository-wide search."

// detailDescription 是输出详略契约(框架 18.2/S2;业界"默认小、按需大"
// 光谱的参数化位:Anthropic response_format enum 同型)。
const detailDescription = "Output detail: \"full\" (default) returns source content blocks; \"paths\" returns only `## path:start-end symbol` header lines so you can Read the files yourself — fresher (reads hit the live disk) and far cheaper in tokens, at the cost of one more round trip."

// maxOutputLengthDescription 是 max_output_length 参数契约(灰度反馈:
// 该参数此前无描述,调用方盲传小值致结果被截断、检索质量凭空劣化,
// review P5 附带项;用户裁决 2026-08-07:质量优先,非明确要省 token
// 不应设限)。
const maxOutputLengthDescription = "Optional output budget in BYTES (default 20000, max 1000000). OMIT this unless the user explicitly wants to cap token spend: small values truncate results and silently degrade retrieval quality. When output is truncated a marker reports how many result blocks were shown."

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
				"max_output_length":   map[string]any{"type": "integer", "description": maxOutputLengthDescription},
				"detail":              map[string]any{"type": "string", "enum": []string{"full", "paths"}, "description": detailDescription},
				"path_prefix":         map[string]any{"type": "string", "description": pathPrefixDescription},
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
				"max_output_length":   map[string]any{"type": "integer", "description": maxOutputLengthDescription},
				"detail":              map[string]any{"type": "string", "enum": []string{"full", "paths"}, "description": detailDescription},
				"path_prefix":         map[string]any{"type": "string", "description": pathPrefixDescription},
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
				"max_output_length":   map[string]any{"type": "integer", "description": maxOutputLengthDescription},
				"detail":              map[string]any{"type": "string", "enum": []string{"full", "paths"}, "description": detailDescription},
				"path_prefix":         map[string]any{"type": "string", "description": pathPrefixDescription},
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
				"max_output_length":   map[string]any{"type": "integer", "description": maxOutputLengthDescription},
				"detail":              map[string]any{"type": "string", "enum": []string{"full", "paths"}, "description": detailDescription},
				"path_prefix":         map[string]any{"type": "string", "description": pathPrefixDescription},
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
