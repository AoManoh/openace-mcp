package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const DefaultAddr = "127.0.0.1:8765"

type Syncer interface {
	Retrieve(context.Context, string, string, int) (workspace.Result, error)
	Sync(context.Context, string) (workspace.Result, error)
}

type WorkspaceInspector interface {
	ListWorkspaceStatuses(context.Context) ([]workspace.WorkspaceStatus, error)
	WorkspaceStatus(context.Context, string) (workspace.WorkspaceStatus, error)
}

type Server struct {
	syncer    Syncer
	tasks     *TaskStore
	authToken string
}

type syncRequest struct {
	DirectoryPath string `json:"directory_path"`
}

type workspaceStatusRequest struct {
	DirectoryPath string `json:"directory_path"`
}

type retrieveRequest struct {
	DirectoryPath      string `json:"directory_path"`
	InformationRequest string `json:"information_request"`
	MaxOutputLength    int    `json:"max_output_length,omitempty"`
}

func NewServer(syncer Syncer) *Server {
	server := &Server{
		syncer:    syncer,
		authToken: strings.TrimSpace(os.Getenv("OPENACE_DAEMON_TOKEN")),
	}
	server.tasks = NewTaskStore(server.runTask, defaultTaskQueueSize)
	return server
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if strings.TrimSpace(addr) == "" {
		addr = DefaultAddr
	}
	if err := validateListenAddr(addr); err != nil {
		return err
	}
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
	mux.HandleFunc("/v1/sync", s.sync)
	mux.HandleFunc("/v1/retrieve", s.retrieve)
	mux.HandleFunc("/v1/workspaces", s.workspaces)
	mux.HandleFunc("/v1/workspace/status", s.workspaceStatus)
	mux.HandleFunc("/v1/tasks", s.tasksCollection)
	mux.HandleFunc("/v1/tasks/", s.taskItem)
	if s.authToken == "" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+s.authToken {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "openace-daemon",
	})
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
	result, err := s.runSync(r.Context(), req.DirectoryPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
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
	result, err := s.runRetrieve(r.Context(), req.DirectoryPath, req.InformationRequest, req.MaxOutputLength)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
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
	status, err := inspector.WorkspaceStatus(r.Context(), req.DirectoryPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) workspaceInspector() (WorkspaceInspector, bool) {
	inspector, ok := s.syncer.(WorkspaceInspector)
	return inspector, ok
}

func (s *Server) tasksCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit := parsePositiveInt(r.URL.Query().Get("limit"))
		writeJSON(w, http.StatusOK, map[string]any{"tasks": s.tasks.List(limit)})
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
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
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) runTask(ctx context.Context, req TaskRequest) (workspace.Result, error) {
	switch req.Kind {
	case TaskKindSync:
		return s.runSync(ctx, req.DirectoryPath)
	case TaskKindRetrieve:
		return s.runRetrieve(ctx, req.DirectoryPath, req.InformationRequest, req.MaxOutputLength)
	case TaskKindMultiRetrieve:
		return s.runMultiRetrieve(ctx, req.DirectoryPaths, req.InformationRequest, req.MaxOutputLength)
	default:
		return workspace.Result{}, fmt.Errorf("unknown task kind: %s", req.Kind)
	}
}

func (s *Server) runSync(ctx context.Context, dir string) (workspace.Result, error) {
	return s.syncer.Sync(ctx, dir)
}

func (s *Server) runRetrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	return s.syncer.Retrieve(ctx, dir, query, maxOutputLen)
}

type multiRetrieveResult struct {
	directoryPath string
	result        workspace.Result
	err           error
}

func (s *Server) runMultiRetrieve(ctx context.Context, dirs []string, query string, maxOutputLen int) (workspace.Result, error) {
	results := make([]multiRetrieveResult, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		i, dir := i, dir
		results[i].directoryPath = dir
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.syncer.Retrieve(ctx, dir, query, maxOutputLen)
			results[i].result = result
			results[i].err = err
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return workspace.Result{}, err
	}
	return aggregateMultiRetrieveResults(results), nil
}

func aggregateMultiRetrieveResults(results []multiRetrieveResult) workspace.Result {
	var out strings.Builder
	out.WriteString("Cross-workspace retrieval results")
	aggregate := workspace.Result{Text: ""}
	for _, item := range results {
		out.WriteString("\n\n## ")
		out.WriteString(item.directoryPath)
		out.WriteString("\n")
		if item.err != nil {
			out.WriteString("ERROR: ")
			out.WriteString(item.err.Error())
			continue
		}
		text := strings.TrimSpace(item.result.Text)
		if text == "" {
			text = "No relevant code sections were found."
		}
		out.WriteString(text)
		out.WriteString("\n\n")
		out.WriteString(item.result.Summary())
		aggregate.FileCount += item.result.FileCount
		aggregate.Uploaded += item.result.Uploaded
		aggregate.Added += item.result.Added
		aggregate.Deleted += item.result.Deleted
	}
	aggregate.Text = out.String()
	return aggregate
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
