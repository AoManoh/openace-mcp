package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type fakeSyncer struct{}

func (fakeSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	return workspace.Result{CheckpointID: "checkpoint", FileCount: 1}, nil
}

func (fakeSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	return workspace.Result{Text: "retrieved", CheckpointID: "checkpoint", FileCount: 1}, nil
}

func TestServerTaskLifecycle(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := httptest.NewServer(NewServer(fakeSyncer{}).routes())
	defer server.Close()

	task := postTask(t, server.URL, TaskRequest{
		Kind:               TaskKindRetrieve,
		DirectoryPath:      "/tmp/workspace",
		InformationRequest: "find server task lifecycle",
	})
	if task.State != TaskStateQueued && task.State != TaskStateRunning {
		t.Fatalf("unexpected submitted task state: %+v", task)
	}

	completed := pollHTTPTask(t, server.URL, task.ID, TaskStateCompleted)
	if completed.Result == nil {
		t.Fatal("completed task should include result")
	}
	if completed.Result.Text != "retrieved" {
		t.Fatalf("unexpected task result: %+v", completed.Result)
	}
}

func TestServerListsTasks(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := httptest.NewServer(NewServer(fakeSyncer{}).routes())
	defer server.Close()

	first := postTask(t, server.URL, TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/one"})
	second := postTask(t, server.URL, TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/two"})

	resp, err := http.Get(server.URL + "/v1/tasks?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %s", resp.Status)
	}
	var list struct {
		Tasks []TaskSnapshot `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("expected 1 listed task, got %d", len(list.Tasks))
	}
	if list.Tasks[0].ID != second.ID {
		t.Fatalf("newest task should be listed first: got %s want %s", list.Tasks[0].ID, second.ID)
	}
	if list.Tasks[0].ID == first.ID {
		t.Fatalf("limit should exclude older task %s", first.ID)
	}
}

func TestServerOptionalBearerAuth(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "local-test-token")
	server := httptest.NewServer(NewServer(fakeSyncer{}).routes())
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized request status = %s", resp.Status)
	}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer local-test-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized request status = %s", resp.Status)
	}
}

func TestValidateListenAddrRequiresLoopbackByDefault(t *testing.T) {
	t.Setenv("OPENACE_ALLOW_REMOTE_DAEMON", "")
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765"} {
		if err := validateListenAddr(addr); err != nil {
			t.Fatalf("loopback addr %q rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8765", ":8765", "http://127.0.0.1:8765"} {
		if err := validateListenAddr(addr); err == nil {
			t.Fatalf("non-loopback or URL addr %q should be rejected", addr)
		}
	}
}

func TestValidateListenAddrCanAllowRemoteExplicitly(t *testing.T) {
	t.Setenv("OPENACE_ALLOW_REMOTE_DAEMON", "1")
	if err := validateListenAddr("0.0.0.0:8765"); err != nil {
		t.Fatalf("remote addr should be allowed when explicit: %v", err)
	}
}

func postTask(t *testing.T, baseURL string, req TaskRequest) TaskSnapshot {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(baseURL+"/v1/tasks", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unexpected status: %s", resp.Status)
	}
	var task TaskSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	return task
}

func pollHTTPTask(t *testing.T, baseURL string, id string, want TaskState) TaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/v1/tasks/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var task TaskSnapshot
		if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if task.State == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach %s", id, want)
	return TaskSnapshot{}
}
