package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func TestClientHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL).Health(context.Background()); err != nil {
		t.Fatalf("health should accept openace daemon: %v", err)
	}
}

func TestClientHealthRejectsUnexpectedService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"other"}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL).Health(context.Background()); err == nil {
		t.Fatal("health should reject non-openace service")
	}
}

func TestClientDaemonStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daemon/status" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","pid":123,"capabilities":{"runtime_identity":true},"cache_namespace":"test"}`))
	}))
	defer server.Close()

	status, err := NewClient(server.URL).DaemonStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.PID != 123 || !status.Capabilities["runtime_identity"] || status.CacheNamespace != "test" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestClientRequiresProviderCapabilityBeforeProviderRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("provider request should stop at health check, got %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).SyncWithProvider(context.Background(), "/tmp/project", "standby")
	if err == nil || !strings.Contains(err.Error(), "provider profile support") {
		t.Fatalf("expected provider capability error, got %v", err)
	}
}

func TestClientSendsProviderProfileWhenDaemonAdvertisesCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","capabilities":{"provider_profiles":true}}`))
		case "/v1/retrieve":
			var req retrieveRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.ProviderProfileID != "standby" {
				t.Fatalf("provider_profile_id = %q, want standby", req.ProviderProfileID)
			}
			_ = json.NewEncoder(w).Encode(workspace.Result{Text: "ok", ProviderProfileID: req.ProviderProfileID})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	result, err := NewClient(server.URL).RetrieveWithProvider(context.Background(), "/tmp/project", "standby", "find code", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderProfileID != "standby" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestClientCachesProviderCapability(t *testing.T) {
	healthCalls := 0
	retrieveCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			healthCalls++
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","capabilities":{"provider_profiles":true}}`))
		case "/v1/retrieve":
			retrieveCalls++
			var req retrieveRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.ProviderProfileID != "standby" {
				t.Fatalf("provider_profile_id = %q, want standby", req.ProviderProfileID)
			}
			_ = json.NewEncoder(w).Encode(workspace.Result{Text: "ok", ProviderProfileID: req.ProviderProfileID})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	for i := 0; i < 2; i++ {
		if _, err := client.RetrieveWithProvider(context.Background(), "/tmp/project", "standby", "find code", 0); err != nil {
			t.Fatal(err)
		}
	}
	if healthCalls != 1 {
		t.Fatalf("health calls = %d, want 1", healthCalls)
	}
	if retrieveCalls != 2 {
		t.Fatalf("retrieve calls = %d, want 2", retrieveCalls)
	}
}

func TestClientCachesProviderCapabilityForConcurrentRequests(t *testing.T) {
	var healthCalls atomic.Int64
	var retrieveCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			healthCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","capabilities":{"provider_profiles":true}}`))
		case "/v1/retrieve":
			retrieveCalls.Add(1)
			var req retrieveRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			if req.ProviderProfileID != "standby" {
				t.Errorf("provider_profile_id = %q, want standby", req.ProviderProfileID)
			}
			_ = json.NewEncoder(w).Encode(workspace.Result{Text: "ok", ProviderProfileID: req.ProviderProfileID})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	start := make(chan struct{})
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := client.RetrieveWithProvider(context.Background(), "/tmp/project", "standby", "find code", 0)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := healthCalls.Load(); got != 1 {
		t.Fatalf("health calls = %d, want 1", got)
	}
	if got := retrieveCalls.Load(); got != int64(cap(errs)) {
		t.Fatalf("retrieve calls = %d, want %d", got, cap(errs))
	}
}

func TestClientDecodesWorkspaceUpstreamHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspaces": []workspace.WorkspaceStatus{{
					DirectoryPath:          "/tmp/project",
					UpstreamStatus:         "backoff",
					UpstreamLastStatusCode: 429,
					UpstreamRetryAfter:     "30s",
				}},
			})
		case "/v1/workspace/status":
			_ = json.NewEncoder(w).Encode(workspace.WorkspaceStatus{
				DirectoryPath:          "/tmp/project",
				UpstreamStatus:         "backoff",
				UpstreamLastStatusCode: 429,
				UpstreamRetryAfter:     "30s",
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	statuses, err := client.ListWorkspaceStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].UpstreamStatus != "backoff" || statuses[0].UpstreamLastStatusCode != 429 {
		t.Fatalf("workspace list should decode upstream health: %+v", statuses)
	}

	status, err := client.WorkspaceStatus(context.Background(), "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	if status.UpstreamStatus != "backoff" || status.UpstreamRetryAfter != "30s" {
		t.Fatalf("workspace status should decode upstream health: %+v", status)
	}
}
