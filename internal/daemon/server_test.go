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
