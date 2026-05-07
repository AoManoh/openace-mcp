package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
