//go:build stress

package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

func TestStressTaskStoreHighConcurrency(t *testing.T) {
	t.Setenv("OPENACE_TASK_STORE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_DIR", "")

	const workers = 16
	const taskCount = 512
	var active int32
	var maxActive int32

	store := NewTaskStoreWithWorkers(func(ctx context.Context, req TaskRequest) (workspace.Result, error) {
		nowActive := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if nowActive <= old || atomic.CompareAndSwapInt32(&maxActive, old, nowActive) {
				break
			}
		}
		defer atomic.AddInt32(&active, -1)

		select {
		case <-time.After(5 * time.Millisecond):
			return workspace.Result{FileCount: 1}, nil
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}
	}, taskCount, workers)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown task store: %v", err)
		}
	}()

	ids := make([]string, 0, taskCount)
	for i := 0; i < taskCount; i++ {
		task, err := store.Submit(TaskRequest{
			Kind:          TaskKindSync,
			DirectoryPath: fmt.Sprintf("/tmp/workspace-%03d", i),
		})
		if err != nil {
			t.Fatalf("submit task %d: %v", i, err)
		}
		ids = append(ids, task.ID)
	}

	deadline := time.Now().Add(10 * time.Second)
	for _, id := range ids {
		for {
			task, ok := store.Get(id)
			if !ok {
				t.Fatalf("task %s disappeared", id)
			}
			if task.State == TaskStateCompleted {
				break
			}
			if task.State == TaskStateFailed || task.State == TaskStateCancelled {
				t.Fatalf("task %s ended unexpectedly: %+v", id, task)
			}
			if time.Now().After(deadline) {
				t.Fatalf("task %s did not complete before deadline: %+v", id, task)
			}
			time.Sleep(time.Millisecond)
		}
	}

	if got := atomic.LoadInt32(&maxActive); got != workers {
		t.Fatalf("max active workers = %d, want %d", got, workers)
	}
}
