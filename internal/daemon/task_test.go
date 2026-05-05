package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func TestTaskStoreCompletesRetrieveTask(t *testing.T) {
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		if req.Kind != TaskKindRetrieve {
			t.Fatalf("unexpected task kind: %s", req.Kind)
		}
		if req.InformationRequest != "find daemon task code" {
			t.Fatalf("unexpected query: %s", req.InformationRequest)
		}
		return workspace.Result{Text: "result", FileCount: 3}, nil
	}, 2)

	task, err := store.Submit(TaskRequest{
		Kind:               "retrieve",
		DirectoryPath:      "/tmp/workspace",
		InformationRequest: "find daemon task code",
	})
	if err != nil {
		t.Fatal(err)
	}

	completed := waitForTaskState(t, store, task.ID, TaskStateCompleted)
	if completed.Result == nil {
		t.Fatal("completed task should include result")
	}
	if completed.Result.FileCount != 3 {
		t.Fatalf("unexpected result: %+v", completed.Result)
	}
}

func TestTaskStoreCancelsRunningTask(t *testing.T) {
	started := make(chan struct{})
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		close(started)
		<-ctx.Done()
		return workspace.Result{}, ctx.Err()
	}, 1)

	task, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not start")
	}

	cancelled, ok := store.Cancel(task.ID)
	if !ok {
		t.Fatal("task should exist")
	}
	if cancelled.State != TaskStateCancelled {
		t.Fatalf("task should be cancelled: %+v", cancelled)
	}
}

func waitForTaskState(t *testing.T, store *TaskStore, id string, want TaskState) TaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := store.Get(id)
		if !ok {
			t.Fatalf("task %s not found", id)
		}
		if task.State == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := store.Get(id)
	t.Fatalf("task %s did not reach %s: %+v", id, want, task)
	return TaskSnapshot{}
}
