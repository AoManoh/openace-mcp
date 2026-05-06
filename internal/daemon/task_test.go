package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func TestTaskStoreCompletesRetrieveTask(t *testing.T) {
	useTempTaskStore(t)
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		if req.Kind != TaskKindRetrieve {
			t.Fatalf("unexpected task kind: %s", req.Kind)
		}
		if req.InformationRequest != "find daemon task code" {
			t.Fatalf("unexpected query: %s", req.InformationRequest)
		}
		return workspace.Result{Text: "result", FileCount: 3}, nil
	}, 2)
	cleanupTaskStore(t, store)

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

func TestTaskStoreCompletesMultiRetrieveTask(t *testing.T) {
	useTempTaskStore(t)
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		if req.Kind != TaskKindMultiRetrieve {
			t.Fatalf("unexpected task kind: %s", req.Kind)
		}
		if len(req.DirectoryPaths) != 2 || req.DirectoryPaths[0] != "/tmp/one" || req.DirectoryPaths[1] != "/tmp/two" {
			t.Fatalf("unexpected directory paths: %#v", req.DirectoryPaths)
		}
		return workspace.Result{Text: "multi result", FileCount: 5}, nil
	}, 2)
	cleanupTaskStore(t, store)

	task, err := store.Submit(TaskRequest{
		Kind:               "multi_codebase_retrieval",
		DirectoryPaths:     []string{" /tmp/one ", "/tmp/two"},
		InformationRequest: "find shared code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.DirectoryPaths) != 2 {
		t.Fatalf("task should include directory paths: %+v", task)
	}

	completed := waitForTaskState(t, store, task.ID, TaskStateCompleted)
	if completed.Result == nil || completed.Result.Text != "multi result" {
		t.Fatalf("unexpected completed result: %+v", completed)
	}
	if len(completed.DirectoryPaths) != 2 {
		t.Fatalf("completed task should include directory paths: %+v", completed)
	}
}

func TestTaskStoreRejectsTooManyMultiRetrievePaths(t *testing.T) {
	useTempTaskStore(t)
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		return workspace.Result{}, nil
	}, 2)
	cleanupTaskStore(t, store)
	paths := make([]string, MaxMultiWorkspacePaths+1)
	for i := range paths {
		paths[i] = "/tmp/workspace"
	}
	if _, err := store.Submit(TaskRequest{
		Kind:               TaskKindMultiRetrieve,
		DirectoryPaths:     paths,
		InformationRequest: "find code",
	}); err == nil {
		t.Fatal("expected too many paths error")
	}
}

func TestTaskStoreCancelsRunningTask(t *testing.T) {
	useTempTaskStore(t)
	started := make(chan struct{})
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		close(started)
		<-ctx.Done()
		return workspace.Result{}, ctx.Err()
	}, 1)
	cleanupTaskStore(t, store)

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
	useTempTaskStore(t)
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		return workspace.Result{}, nil
	}, 4)
	cleanupTaskStore(t, store)

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
	useTempTaskStore(t)
	store := NewTaskStore(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		return workspace.Result{Text: "large retrieval text", FileCount: 1}, nil
	}, 2)
	cleanupTaskStore(t, store)

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

func TestTaskStoreRunsTasksConcurrently(t *testing.T) {
	useTempTaskStore(t)
	release := make(chan struct{})
	overlap := make(chan struct{})
	var releaseOnce sync.Once
	var overlapOnce sync.Once
	var mu sync.Mutex
	active := 0
	maxActive := 0

	store := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		if active >= 2 {
			overlapOnce.Do(func() { close(overlap) })
		}
		mu.Unlock()

		select {
		case <-release:
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}

		mu.Lock()
		active--
		mu.Unlock()
		return workspace.Result{FileCount: 1}, nil
	}, 4, 2)
	cleanupTaskStore(t, store)

	first, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/two"})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-overlap:
	case <-time.After(2 * time.Second):
		t.Fatal("tasks did not run concurrently")
	}

	releaseOnce.Do(func() { close(release) })
	_ = waitForTaskState(t, store, first.ID, TaskStateCompleted)
	_ = waitForTaskState(t, store, second.ID, TaskStateCompleted)
	if store.WorkerCount() != 2 {
		t.Fatalf("worker count = %d", store.WorkerCount())
	}
	mu.Lock()
	gotMax := maxActive
	mu.Unlock()
	if gotMax < 2 {
		t.Fatalf("max active tasks = %d", gotMax)
	}
}

func TestTaskStoreShutdownCancelsRunningTasksAndRejectsSubmit(t *testing.T) {
	useTempTaskStore(t)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get(task.ID)
	if !ok {
		t.Fatal("task should still be retained after shutdown")
	}
	if got.State != TaskStateCancelled || got.Error != "shutdown" {
		t.Fatalf("running task should be cancelled by shutdown: %+v", got)
	}
	if _, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/other"}); !errors.Is(err, ErrTaskStoreClosed) {
		t.Fatalf("submit after shutdown error = %v, want %v", err, ErrTaskStoreClosed)
	}
}

func TestTaskStorePersistsShutdownCancelledTasksBeyondHistoryLimit(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_TASK_HISTORY_LIMIT", "1")
	started := make(chan struct{}, 2)
	store := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return workspace.Result{}, ctx.Err()
	}, 2, 2)

	first, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/two"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("tasks did not start")
		}
	}

	shutdownTaskStore(t, store)
	recovered := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		t.Fatal("shutdown-cancelled task should not run")
		return workspace.Result{}, nil
	}, 2, 1)
	cleanupTaskStore(t, recovered)
	for _, id := range []string{first.ID, second.ID} {
		got, ok := recovered.Get(id)
		if !ok {
			t.Fatalf("task %s was dropped from persisted manifest", id)
		}
		if got.State != TaskStateCancelled || got.Error != "shutdown" {
			t.Fatalf("unexpected recovered task %s: %+v", id, got)
		}
	}
}

func TestTaskWorkerCountEnvironment(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_TASK_WORKERS", "7")
	if got := taskWorkerCount(); got != 7 {
		t.Fatalf("task worker count = %d", got)
	}
	t.Setenv("OPENACE_TASK_WORKERS", "0")
	if got := taskWorkerCount(); got != defaultTaskWorkerCount {
		t.Fatalf("zero worker count should use default, got %d", got)
	}
	t.Setenv("OPENACE_TASK_WORKERS", "1000")
	if got := taskWorkerCount(); got != maxTaskWorkerCount {
		t.Fatalf("worker count should be capped, got %d", got)
	}
	t.Setenv("OPENACE_TASK_WORKERS", "not-a-number")
	if got := taskWorkerCount(); got != defaultTaskWorkerCount {
		t.Fatalf("invalid worker count should use default, got %d", got)
	}
}

func TestTaskQueueSizeEnvironment(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_TASK_QUEUE_SIZE", "")
	if got := taskQueueSize(); got != defaultTaskQueueSize {
		t.Fatalf("default queue size = %d", got)
	}
	t.Setenv("OPENACE_TASK_QUEUE_SIZE", "512")
	if got := taskQueueSize(); got != 512 {
		t.Fatalf("custom queue size = %d", got)
	}
	t.Setenv("OPENACE_TASK_QUEUE_SIZE", "0")
	if got := taskQueueSize(); got != defaultTaskQueueSize {
		t.Fatalf("zero queue size should use default, got %d", got)
	}
	t.Setenv("OPENACE_TASK_QUEUE_SIZE", "100000")
	if got := taskQueueSize(); got != maxTaskQueueSize {
		t.Fatalf("queue size should be capped, got %d", got)
	}
	t.Setenv("OPENACE_TASK_QUEUE_SIZE", "not-a-number")
	if got := taskQueueSize(); got != defaultTaskQueueSize {
		t.Fatalf("invalid queue size should use default, got %d", got)
	}
}

func TestTaskHistoryLimitEnvironment(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_TASK_HISTORY_LIMIT", "")
	if got := taskHistoryLimit(); got != defaultTaskHistoryLimit {
		t.Fatalf("default history limit = %d", got)
	}
	t.Setenv("OPENACE_TASK_HISTORY_LIMIT", "2048")
	if got := taskHistoryLimit(); got != 2048 {
		t.Fatalf("custom history limit = %d", got)
	}
	t.Setenv("OPENACE_TASK_HISTORY_LIMIT", "0")
	if got := taskHistoryLimit(); got != defaultTaskHistoryLimit {
		t.Fatalf("zero history limit should use default, got %d", got)
	}
	t.Setenv("OPENACE_TASK_HISTORY_LIMIT", "100000")
	if got := taskHistoryLimit(); got != maxTaskHistoryLimit {
		t.Fatalf("history limit should be capped, got %d", got)
	}
	t.Setenv("OPENACE_TASK_HISTORY_LIMIT", "not-a-number")
	if got := taskHistoryLimit(); got != defaultTaskHistoryLimit {
		t.Fatalf("invalid history limit should use default, got %d", got)
	}
}

func TestTaskStorePersistsCompletedTasks(t *testing.T) {
	dir := useTempTaskStore(t)
	store := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		return workspace.Result{Text: "persisted result", FileCount: 9}, nil
	}, 2, 1)
	cleanupTaskStore(t, store)

	task, err := store.Submit(TaskRequest{Kind: TaskKindSync, DirectoryPath: "/tmp/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForTaskState(t, store, task.ID, TaskStateCompleted)
	if completed.Result == nil {
		t.Fatal("completed task should include result")
	}
	shutdownTaskStore(t, store)

	if _, err := os.Stat(filepath.Join(dir, "tasks.json")); err != nil {
		t.Fatal(err)
	}

	recovered := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		t.Fatal("recovered terminal task should not run")
		return workspace.Result{}, nil
	}, 2, 1)
	cleanupTaskStore(t, recovered)
	got, ok := recovered.Get(task.ID)
	if !ok {
		t.Fatalf("recovered task %s not found", task.ID)
	}
	if got.State != TaskStateCompleted || got.Result == nil || got.Result.Text != "persisted result" {
		t.Fatalf("unexpected recovered task: %+v", got)
	}
	list := recovered.List(10)
	if len(list) != 1 || list[0].Result == nil || list[0].Result.Text != "" {
		t.Fatalf("list should recover summary without result text: %+v", list)
	}
}

func TestTaskStoreMarksRunningTaskAbandonedOnRestart(t *testing.T) {
	dir := useTempTaskStore(t)
	now := time.Now().UTC()
	task := TaskSnapshot{
		ID:            "running-task",
		Kind:          TaskKindSync,
		State:         TaskStateRunning,
		DirectoryPath: "/tmp/workspace",
		SubmittedAt:   now.Add(-time.Second),
		StartedAt:     &now,
	}
	if err := saveTaskManifest(dir, taskManifest{Tasks: []TaskSnapshot{task}}); err != nil {
		t.Fatal(err)
	}

	recovered := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		t.Fatal("abandoned task should not run")
		return workspace.Result{}, nil
	}, 2, 1)
	cleanupTaskStore(t, recovered)
	got, ok := recovered.Get(task.ID)
	if !ok {
		t.Fatalf("recovered task %s not found", task.ID)
	}
	if got.State != TaskStateFailed || got.Error != "abandoned after daemon restart" {
		t.Fatalf("running task should be marked abandoned: %+v", got)
	}
}

func useTempTaskStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OPENACE_TASK_STORE_DIR", dir)
	t.Setenv("OPENACE_CACHE_DIR", "")
	t.Setenv("OPENACE_CACHE_NAMESPACE", "test")
	return dir
}

func cleanupTaskStore(t *testing.T, store *TaskStore) {
	t.Helper()
	t.Cleanup(func() {
		if err := shutdownTaskStoreErr(store); err != nil {
			t.Errorf("shutdown task store: %v", err)
		}
	})
}

func shutdownTaskStore(t *testing.T, store *TaskStore) {
	t.Helper()
	if err := shutdownTaskStoreErr(store); err != nil {
		t.Fatalf("shutdown task store: %v", err)
	}
}

func shutdownTaskStoreErr(store *TaskStore) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return store.Shutdown(ctx)
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
