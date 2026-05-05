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

func TestTaskStoreListsNewestTasksFirst(t *testing.T) {
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		return workspace.Result{}, nil
	}, 4)

	first, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/one"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	second, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/two"})
	if err != nil {
		t.Fatal(err)
	}

	tasks := store.List(1)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 listed task, got %d", len(tasks))
	}
	if tasks[0].ID != second.ID {
		t.Fatalf("newest task should be listed first: got %s want %s", tasks[0].ID, second.ID)
	}
	if tasks[0].ID == first.ID {
		t.Fatalf("limit should exclude older task %s", first.ID)
	}
}

func TestTaskStoreListOmitsResultText(t *testing.T) {
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		return workspace.Result{Text: "large retrieval text", FileCount: 1}, nil
	}, 2)

	task, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitForTaskState(t, store, task.ID, TaskStateCompleted)

	tasks := store.List(10)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Result == nil {
		t.Fatal("summary should include result metadata")
	}
	if tasks[0].Result.Text != "" {
		t.Fatalf("list should omit result text, got %q", tasks[0].Result.Text)
	}
	detail, ok := store.Get(task.ID)
	if !ok {
		t.Fatal("task should exist")
	}
	if detail.Result == nil || detail.Result.Text == "" {
		t.Fatalf("detail should retain result text: %+v", detail)
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
