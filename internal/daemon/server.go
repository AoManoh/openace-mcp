package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/engine"
)

const DefaultAddr = "127.0.0.1:8765"

type Server struct {
	service      engine.Service
	tasks        *TaskStore
	reconciler   *workspaceReconciler
	authToken    string
	authErr      error
	reconcileErr error
	startedAt    time.Time
	statusMu     sync.Mutex
	listenAddr   string
	// logf 是诊断日志出口(灰度反馈 2026-08-07:检索阻塞时 daemon 日志
	// 只有启动行,无从判断请求是否到达、卡在哪、daemon 是哪个构建)。
	// 默认带时间戳写 stderr(managed 模式经 daemonLogFile 落盘);
	// 测试可注入。
	logf func(format string, args ...any)
}

type syncRequest struct {
	DirectoryPath     string `json:"directory_path"`
	ProviderProfileID string `json:"provider_profile_id,omitempty"`
}

type workspaceStatusRequest struct {
	DirectoryPath     string `json:"directory_path"`
	ProviderProfileID string `json:"provider_profile_id,omitempty"`
}

type repoMapRequest struct {
	DirectoryPath   string `json:"directory_path"`
	MaxOutputLength int    `json:"max_output_length,omitempty"`
	Focus           string `json:"focus,omitempty"`
}

type retrieveRequest struct {
	DirectoryPath      string `json:"directory_path"`
	ProviderProfileID  string `json:"provider_profile_id,omitempty"`
	InformationRequest string `json:"information_request"`
	MaxOutputLength    int    `json:"max_output_length,omitempty"`
	// Detail 是输出详略(框架 18.2/S2):""/"full"=内容块;"paths"=
	// 只回 path:range 头行。非法值由引擎按请求类错误拒绝(400)。
	Detail     string `json:"detail,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
}

func NewServer(service engine.Service) *Server {
	// M5:默认档自动生成随机 token(0600 状态文件);=off 显式关闭;
	// 文件不可用 fail-closed(此处保守起见退回显式报错的哨兵空 token
	// 会打开零认证,故失败即 panic 边界改为:构造期记录错误,
	// ListenAndServe 前置校验拒绝启动)。
	token, tokenErr := resolveAuthToken()
	server := &Server{
		service:   service,
		authToken: token,
		authErr:   tokenErr,
		startedAt: time.Now().UTC(),
		logf:      log.New(os.Stderr, "openace-daemon: ", log.LstdFlags|log.LUTC|log.Lmsgprefix).Printf,
	}
	server.tasks = NewTaskStore(server.runTask, 0)
	server.reconciler, server.reconcileErr = newWorkspaceReconciler(service)
	return server
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if s.authErr != nil {
		// fail-closed(M5):默认 token 档不可用时拒绝启动,不静默
		// 退回零认证。
		return s.authErr
	}
	if s.reconcileErr != nil {
		// fail-fast(M10):监测并发配置非法拒绝启动。
		return s.reconcileErr
	}
	if strings.TrimSpace(addr) == "" {
		addr = DefaultAddr
	}
	if err := validateListenAddr(addr); err != nil {
		return err
	}
	s.setListenAddr(addr)
	// 启动行携带构建身份与 pid:日志自身可判定 daemon 是哪个构建
	// (灰度反馈 2026-08-07 的版本诊断卡点,此前只能另调 daemon_status)。
	build := buildinfo.Current()
	s.logf("serving %s build=%s(%s) pid=%d", addr, build.VCSRevision, build.Version, os.Getpid())
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return ctx.Err()
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.reconciler != nil {
		if err := s.reconciler.Shutdown(ctx); err != nil {
			return err
		}
	}
	if s.tasks != nil {
		if err := s.tasks.Shutdown(ctx); err != nil {
			return err
		}
	}
	// 持有本地资源的引擎（如 local-hybrid 的索引句柄）随 daemon 一并释放。
	if lifecycle, ok := s.service.(engine.Lifecycle); ok {
		return lifecycle.Close(ctx)
	}
	return nil
}

func validateListenAddr(addr string) error {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return fmt.Errorf("daemon listen addr must be host:port, got URL %q", addr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid daemon listen addr %q: %w", addr, err)
	}
	if isRemoteDaemonAllowed() {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing non-loopback daemon listen addr %q; set OPENACE_ALLOW_REMOTE_DAEMON=1 only after adding network-level access control", addr)
}

func isRemoteDaemonAllowed() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("OPENACE_ALLOW_REMOTE_DAEMON")))
	return value == "1" || value == "true" || value == "yes"
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.health)
	mux.HandleFunc("/v1/daemon/status", s.daemonStatus)
	mux.HandleFunc("/v1/sync", s.sync)
	mux.HandleFunc("/v1/retrieve", s.retrieve)
	mux.HandleFunc("/v1/repo-map", s.repoMap)
	mux.HandleFunc("/v1/workspaces", s.workspaces)
	mux.HandleFunc("/v1/workspace/status", s.workspaceStatus)
	mux.HandleFunc("/v1/tasks", s.tasksCollection)
	mux.HandleFunc("/v1/tasks/", s.taskItem)
	if s.authToken == "" {
		return s.withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
			mux.ServeHTTP(w, r)
		}))
	}
	expected := []byte("Bearer " + s.authToken)
	return s.withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 常数时间比较(M5 附带):防 token 逐字节计时侧信道。
		got := []byte(r.Header.Get("authorization"))
		if len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		mux.ServeHTTP(w, r)
	}))
}

// withRequestLog 输出请求级诊断日志(灰度反馈 2026-08-07:检索阻塞时
// daemon 日志只有启动行,无从判断请求是否到达、卡在哪)。慢端点
// (/v1/retrieve、/v1/sync,可能等待在建索引)记 start 行——悬挂请求
// 表现为"有 start 无完成";全部请求记完成行(方法/路径/状态/时长),
// 401 一并可见(token 错配诊断)。/healthz、/readyz 的非错误响应不记
// (waitReady 100ms 轮询会刷屏)。查询文本与请求体不入日志(本地隐私面)。
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/retrieve" || r.URL.Path == "/v1/sync" {
			s.logf("%s %s started", r.Method, r.URL.Path)
		}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, r)
		if (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") && recorder.status < 400 {
			return
		}
		s.logf("%s %s -> %d (%s)", r.Method, r.URL.Path, recorder.status, time.Since(start).Round(time.Millisecond))
	})
}

// statusRecorder 捕获响应状态码供请求日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.statusSnapshot(r.Context()))
}

func (s *Server) daemonStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.statusSnapshot(r.Context()))
}

func (s *Server) setListenAddr(addr string) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.listenAddr = addr
}

func (s *Server) currentListenAddr() string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.listenAddr
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req syncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DirectoryPath) == "" {
		writeError(w, http.StatusBadRequest, "directory_path is required")
		return
	}
	result, err := s.runSync(r.Context(), req.DirectoryPath, req.ProviderProfileID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	s.attachResultServedBy(&result)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) retrieve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req retrieveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DirectoryPath) == "" {
		writeError(w, http.StatusBadRequest, "directory_path is required")
		return
	}
	if strings.TrimSpace(req.InformationRequest) == "" {
		writeError(w, http.StatusBadRequest, "information_request is required")
		return
	}
	maxOutputLength, err := normalizeMaxOutputLength(req.MaxOutputLength)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.runRetrieve(r.Context(), req.DirectoryPath, req.ProviderProfileID, req.InformationRequest, maxOutputLength, req.Detail, req.PathPrefix)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	s.attachResultServedBy(&result)
	writeJSON(w, http.StatusOK, result)
}

// repoMap 是 repo_map R1 端点(D4):快照只读,冷仓显式 not-ready。
func (s *Server) repoMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mapper, ok := s.service.(engine.RepoMapper)
	if !ok {
		writeError(w, http.StatusNotImplemented, "repo map is not supported by this engine")
		return
	}
	var req repoMapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DirectoryPath) == "" {
		writeError(w, http.StatusBadRequest, "directory_path is required")
		return
	}
	maxOutputLength, err := normalizeMaxOutputLength(req.MaxOutputLength)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.observeWorkspace(req.DirectoryPath, "")
	result, err := mapper.RepoMap(r.Context(), engine.RepoMapRequest{
		Workspace:    engine.WorkspaceRef{DirectoryPath: req.DirectoryPath},
		MaxOutputLen: maxOutputLength,
		Focus:        req.Focus,
	})
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	s.attachResultServedBy(&result)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	inspector, ok := s.workspaceInspector()
	if !ok {
		writeError(w, http.StatusNotImplemented, "workspace status is not supported")
		return
	}
	statuses, err := inspector.ListWorkspaceStatuses(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	for i := range statuses {
		s.decorateWorkspaceStatus(&statuses[i])
		s.attachWorkspaceStatusServedBy(&statuses[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": statuses})
}

func (s *Server) workspaceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	inspector, ok := s.workspaceInspector()
	if !ok {
		writeError(w, http.StatusNotImplemented, "workspace status is not supported")
		return
	}
	var req workspaceStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DirectoryPath) == "" {
		writeError(w, http.StatusBadRequest, "directory_path is required")
		return
	}
	status, err := s.workspaceStatusForProvider(r.Context(), inspector, req.DirectoryPath, req.ProviderProfileID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	s.decorateWorkspaceStatus(&status)
	s.attachWorkspaceStatusServedBy(&status)
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) workspaceInspector() (engine.WorkspaceInspector, bool) {
	inspector, ok := s.service.(engine.WorkspaceInspector)
	return inspector, ok
}

func (s *Server) workspaceStatusForProvider(ctx context.Context, inspector engine.WorkspaceInspector, dir string, providerProfileID string) (engine.WorkspaceStatus, error) {
	return inspector.WorkspaceStatus(ctx, engine.WorkspaceRef{
		DirectoryPath:     dir,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	})
}

func (s *Server) decorateWorkspaceStatus(status *engine.WorkspaceStatus) {
	if s.reconciler != nil {
		s.reconciler.Decorate(status)
	}
}

func (s *Server) attachResultServedBy(result *engine.Result) {
	if result == nil {
		return
	}
	identity := s.servedBy()
	result.ServedBy = &identity
}

func (s *Server) attachWorkspaceStatusServedBy(status *engine.WorkspaceStatus) {
	if status == nil {
		return
	}
	identity := s.servedBy()
	status.ServedBy = &identity
}

func (s *Server) attachTaskServedBy(task *TaskSnapshot) {
	if task == nil {
		return
	}
	identity := s.servedBy()
	task.ServedBy = &identity
	if task.Result != nil && task.Result.ServedBy == nil {
		task.Result.ServedBy = &identity
	}
}

func (s *Server) tasksCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit := parsePositiveInt(r.URL.Query().Get("limit"))
		tasks := s.tasks.List(limit)
		for i := range tasks {
			s.attachTaskServedBy(&tasks[i])
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
		return
	case http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	task, err := s.tasks.Submit(req)
	if err != nil {
		if errors.Is(err, ErrTaskQueueFull) {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if errors.Is(err, ErrTaskStoreClosed) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.attachTaskServedBy(&task)
	writeJSON(w, http.StatusAccepted, task)
}

func parsePositiveInt(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func (s *Server) taskItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	if path == "" {
		writeError(w, http.StatusNotFound, "task id is required")
		return
	}
	if id, ok := strings.CutSuffix(path, "/cancel"); ok {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if strings.Contains(id, "/") || id == "" {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		task, found := s.tasks.Cancel(id)
		if !found {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		s.attachTaskServedBy(&task)
		writeJSON(w, http.StatusOK, task)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if strings.Contains(path, "/") {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	task, found := s.tasks.Get(path)
	if !found {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	s.attachTaskServedBy(&task)
	s.attachTaskProgress(r.Context(), &task)
	writeJSON(w, http.StatusOK, task)
}

// attachTaskProgress 给 running 态任务现查工作区进度(P3,灰度反馈):
// stage + 文件数 + 语义嵌入进度,失败静默留空(诊断字段不阻断任务面)。
func (s *Server) attachTaskProgress(ctx context.Context, task *TaskSnapshot) {
	if task.State != TaskStateRunning || strings.TrimSpace(task.DirectoryPath) == "" {
		return
	}
	inspector, ok := s.workspaceInspector()
	if !ok {
		return
	}
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	status, err := inspector.WorkspaceStatus(statusCtx, engine.WorkspaceRef{DirectoryPath: task.DirectoryPath})
	if err != nil {
		return
	}
	progress := fmt.Sprintf("stage=%s files=%d", status.Stage, status.FileCount)
	if status.Semantic != nil && (status.Semantic.PendingChunks > 0 || status.Semantic.EmbeddedChunks > 0) {
		progress += fmt.Sprintf(" embedded=%d pending=%d", status.Semantic.EmbeddedChunks, status.Semantic.PendingChunks)
	}
	task.Progress = progress
}

func (s *Server) runTask(ctx context.Context, req TaskRequest) (engine.Result, error) {
	var result engine.Result
	var err error
	switch req.Kind {
	case TaskKindSync:
		result, err = s.runSync(ctx, req.DirectoryPath, req.ProviderProfileID)
	case TaskKindRetrieve:
		result, err = s.runRetrieve(ctx, req.DirectoryPath, req.ProviderProfileID, req.InformationRequest, req.MaxOutputLength, req.Detail, req.PathPrefix)
	case TaskKindMultiRetrieve:
		result, err = s.runMultiRetrieve(ctx, req.DirectoryPaths, req.ProviderProfileID, req.InformationRequest, req.MaxOutputLength)
	default:
		return engine.Result{}, fmt.Errorf("unknown task kind: %s", req.Kind)
	}
	if err != nil {
		return engine.Result{}, err
	}
	s.attachResultServedBy(&result)
	return result, nil
}

func (s *Server) runSync(ctx context.Context, dir string, providerProfileID string) (engine.Result, error) {
	s.observeWorkspace(dir, providerProfileID)
	return s.service.Sync(ctx, engine.SyncRequest{Workspace: engine.WorkspaceRef{
		DirectoryPath:     dir,
		ProviderProfileID: strings.TrimSpace(providerProfileID),
	}})
}

func (s *Server) runRetrieve(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int, detail string, pathPrefix string) (engine.Result, error) {
	s.observeWorkspace(dir, providerProfileID)
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

func (s *Server) observeWorkspace(dir string, providerProfileID string) {
	if s.reconciler != nil {
		s.reconciler.ObserveWithProvider(dir, providerProfileID)
	}
}

type multiRetrieveResult struct {
	directoryPath string
	result        engine.Result
	err           error
}

func (s *Server) runMultiRetrieve(ctx context.Context, dirs []string, providerProfileID string, query string, maxOutputLen int) (engine.Result, error) {
	results := make([]multiRetrieveResult, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		i, dir := i, dir
		results[i].directoryPath = dir
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.runRetrieve(ctx, dir, providerProfileID, query, maxOutputLen, "", "")
			results[i].result = result
			results[i].err = err
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return engine.Result{}, err
	}
	result, successCount := aggregateMultiRetrieveResults(providerProfileID, results)
	if successCount == 0 {
		return engine.Result{}, fmt.Errorf("all workspace retrievals failed\n\n%s", strings.TrimSpace(result.Text))
	}
	return result, nil
}

func aggregateMultiRetrieveResults(providerProfileID string, results []multiRetrieveResult) (engine.Result, int) {
	var out strings.Builder
	out.WriteString("Cross-workspace retrieval results")
	status := engine.MultiRetrievalStatus{
		ProviderProfileID: strings.TrimSpace(providerProfileID),
		TotalWorkspaces:   len(results),
		Workspaces:        make([]engine.MultiWorkspaceStatus, 0, len(results)),
	}
	aggregate := engine.Result{Text: "", ProviderProfileID: status.ProviderProfileID, MultiStatus: &status}
	for _, item := range results {
		out.WriteString("\n\n## ")
		out.WriteString(item.directoryPath)
		out.WriteString("\n")
		workspaceStatus := engine.MultiWorkspaceStatus{
			DirectoryPath: item.directoryPath,
			Status:        "success",
		}
		if item.err != nil {
			status.FailureCount++
			workspaceStatus.Status = "error"
			workspaceStatus.Error = item.err.Error()
			status.Workspaces = append(status.Workspaces, workspaceStatus)
			out.WriteString("ERROR: ")
			out.WriteString(item.err.Error())
			continue
		}
		text := strings.TrimSpace(item.result.Text)
		if text == "" {
			text = "No relevant code sections were found."
		}
		status.SuccessCount++
		// P2(review 二批):每仓降级透明性字段进 multi_status 结构化条目
		// ——任务文本被 1MiB 截断(limitTaskResult 只裁 Text)时,被截掉
		// 小节的降级语义仍可经结构化字段获取(P3 兜底)。
		workspaceStatus.RetrievalMode = item.result.RetrievalMode
		workspaceStatus.DegradedReason = item.result.DegradedReason
		workspaceStatus.SemanticCoverage = item.result.SemanticCoverage
		if item.result.DegradedReason != "" {
			status.DegradedCount++
		}
		status.Workspaces = append(status.Workspaces, workspaceStatus)
		out.WriteString(text)
		out.WriteString("\n\n")
		out.WriteString(item.result.Summary())
		aggregate.FileCount += item.result.FileCount
		aggregate.Uploaded += item.result.Uploaded
		aggregate.Added += item.result.Added
		aggregate.Deleted += item.result.Deleted
	}
	status.PartialFailure = status.SuccessCount > 0 && status.FailureCount > 0
	if status.FailureCount > 0 {
		outString := out.String()
		out.Reset()
		out.WriteString("Cross-workspace retrieval results")
		out.WriteString(fmt.Sprintf("\nWARNING: %d of %d workspaces failed; successful results are partial.", status.FailureCount, status.TotalWorkspaces))
		out.WriteString(strings.TrimPrefix(outString, "Cross-workspace retrieval results"))
	}
	aggregate.MultiStatus = &status
	// P2:有降级仓时聚合文本以 [DEGRADED] 汇总横幅开头(与 mcp 同步
	// 路径、单仓首行横幅同一心智模型)。
	aggregate.Text = status.DegradedBanner() + out.String()
	return aggregate, status.SuccessCount
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintf(w, `{"error":%q}`, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeUpstreamError 把引擎错误映射为 HTTP 状态(Stage 7 简化:legacy
// 上游 429 直通逻辑随 ACE 引擎删除;local-hybrid 的 provider 失败在
// 引擎层已分类为可行动文本,按引擎错误原样透出)。P7(review 二批):
// 请求类错误(目录非法/参数被拒/查询为空)分流 400——重试必然再失败,
// 不得伪装成可重试的网关故障;其余保持 502。
func writeUpstreamError(w http.ResponseWriter, err error) {
	if engine.IsInvalidRequest(err) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}
