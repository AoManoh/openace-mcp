package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

type Server struct {
	syncer Syncer
	mu     sync.Mutex
}

type syncRequest struct {
	DirectoryPath string `json:"directory_path"`
}

type retrieveRequest struct {
	DirectoryPath      string `json:"directory_path"`
	InformationRequest string `json:"information_request"`
	MaxOutputLength    int    `json:"max_output_length,omitempty"`
}

func NewServer(syncer Syncer) *Server {
	return &Server{syncer: syncer}
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if strings.TrimSpace(addr) == "" {
		addr = DefaultAddr
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

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.health)
	mux.HandleFunc("/v1/sync", s.sync)
	mux.HandleFunc("/v1/retrieve", s.retrieve)
	return mux
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
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.syncer.Sync(r.Context(), req.DirectoryPath)
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
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.syncer.Retrieve(r.Context(), req.DirectoryPath, req.InformationRequest, req.MaxOutputLength)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
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
