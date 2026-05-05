package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const defaultTaskQueueSize = 16
const defaultTaskHistoryLimit = 128

type TaskKind string

const (
	TaskKindSync     TaskKind = "sync_workspace"
	TaskKindRetrieve TaskKind = "codebase_retrieval"
)

type TaskState string

const (
	TaskStateQueued    TaskState = "queued"
	TaskStateRunning   TaskState = "running"
	TaskStateCompleted TaskState = "completed"
	TaskStateFailed    TaskState = "failed"
	TaskStateCancelled TaskState = "cancelled"
)

type TaskRequest struct {
	Kind               TaskKind `json:"kind"`
	DirectoryPath      string   `json:"directory_path"`
	InformationRequest string   `json:"information_request,omitempty"`
	MaxOutputLength    int      `json:"max_output_length,omitempty"`
}

type TaskSnapshot struct {
	ID                 string            `json:"id"`
	Kind               TaskKind          `json:"kind"`
	State              TaskState         `json:"state"`
	DirectoryPath      string            `json:"directory_path"`
	InformationRequest string            `json:"information_request,omitempty"`
	MaxOutputLength    int               `json:"max_output_length,omitempty"`
	SubmittedAt        time.Time         `json:"submitted_at"`
	StartedAt          *time.Time        `json:"started_at,omitempty"`
	CompletedAt        *time.Time        `json:"completed_at,omitempty"`
	Error              string            `json:"error,omitempty"`
	Result             *workspace.Result `json:"result,omitempty"`
}

type TaskRunner func(context.Context, TaskRequest) (workspace.Result, error)

type TaskStore struct {
	mu     sync.Mutex
	runner TaskRunner
	queue  chan string
	tasks  map[string]*taskRecord
}

type taskRecord struct {
	snapshot TaskSnapshot
	request  TaskRequest
	cancel   context.CancelFunc
}

var ErrTaskQueueFull = errors.New("task queue is full")

func NewTaskStore(runner TaskRunner, queueSize int) *TaskStore {
	if queueSize <= 0 {
		queueSize = defaultTaskQueueSize
	}
	store := &TaskStore{
		runner: runner,
		queue:  make(chan string, queueSize),
		tasks:  make(map[string]*taskRecord),
	}
	go store.worker()
	return store
}

func (s *TaskStore) Submit(req TaskRequest) (TaskSnapshot, error) {
	normalized, err := normalizeTaskRequest(req)
	if err != nil {
		return TaskSnapshot{}, err
	}
	id, err := newTaskID()
	if err != nil {
		return TaskSnapshot{}, err
	}
	record := &taskRecord{
		request: normalized,
		snapshot: TaskSnapshot{
			ID:                 id,
			Kind:               normalized.Kind,
			State:              TaskStateQueued,
			DirectoryPath:      normalized.DirectoryPath,
			InformationRequest: normalized.InformationRequest,
			MaxOutputLength:    normalized.MaxOutputLength,
			SubmittedAt:        time.Now().UTC(),
		},
	}

	s.mu.Lock()
	s.tasks[id] = record
	s.pruneLocked(defaultTaskHistoryLimit)
	snapshot := cloneTask(record.snapshot)
	s.mu.Unlock()

	select {
	case s.queue <- id:
		return snapshot, nil
	default:
		s.mu.Lock()
		delete(s.tasks, id)
		s.mu.Unlock()
		return TaskSnapshot{}, ErrTaskQueueFull
	}
}

func (s *TaskStore) Get(id string) (TaskSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.tasks[id]
	if !ok {
		return TaskSnapshot{}, false
	}
	return cloneTask(record.snapshot), true
}

func (s *TaskStore) List(limit int) []TaskSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]TaskSnapshot, 0, len(s.tasks))
	for _, record := range s.tasks {
		tasks = append(tasks, cloneTaskSummary(record.snapshot))
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].SubmittedAt.After(tasks[j].SubmittedAt)
	})
	if limit > 0 && len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks
}

func (s *TaskStore) pruneLocked(max int) {
	if max <= 0 || len(s.tasks) <= max {
		return
	}
	type terminalTask struct {
		id          string
		submittedAt time.Time
	}
	var terminals []terminalTask
	for id, record := range s.tasks {
		if isTerminal(record.snapshot.State) {
			terminals = append(terminals, terminalTask{id: id, submittedAt: record.snapshot.SubmittedAt})
		}
	}
	sort.Slice(terminals, func(i, j int) bool {
		return terminals[i].submittedAt.Before(terminals[j].submittedAt)
	})
	for len(s.tasks) > max && len(terminals) > 0 {
		delete(s.tasks, terminals[0].id)
		terminals = terminals[1:]
	}
}

func (s *TaskStore) Cancel(id string) (TaskSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.tasks[id]
	if !ok {
		return TaskSnapshot{}, false
	}
	if record.snapshot.State == TaskStateCompleted || record.snapshot.State == TaskStateFailed || record.snapshot.State == TaskStateCancelled {
		return cloneTask(record.snapshot), true
	}
	if record.cancel != nil {
		record.cancel()
		record.cancel = nil
	}
	now := time.Now().UTC()
	record.snapshot.State = TaskStateCancelled
	record.snapshot.CompletedAt = &now
	record.snapshot.Error = "cancelled"
	return cloneTask(record.snapshot), true
}

func (s *TaskStore) worker() {
	for id := range s.queue {
		record, ctx, ok := s.start(id)
		if !ok {
			continue
		}
		result, err := s.runner(ctx, record.request)
		s.finish(id, result, err)
	}
}

func (s *TaskStore) start(id string) (*taskRecord, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.tasks[id]
	if !ok || record.snapshot.State != TaskStateQueued {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	record.cancel = cancel
	record.snapshot.State = TaskStateRunning
	record.snapshot.StartedAt = &now
	return record, ctx, true
}

func (s *TaskStore) finish(id string, result workspace.Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.tasks[id]
	if !ok || record.snapshot.State == TaskStateCancelled {
		return
	}
	now := time.Now().UTC()
	record.cancel = nil
	record.snapshot.CompletedAt = &now
	if err != nil {
		if errors.Is(err, context.Canceled) {
			record.snapshot.State = TaskStateCancelled
			record.snapshot.Error = "cancelled"
			return
		}
		record.snapshot.State = TaskStateFailed
		record.snapshot.Error = err.Error()
		return
	}
	record.snapshot.State = TaskStateCompleted
	record.snapshot.Result = &result
}

func normalizeTaskRequest(req TaskRequest) (TaskRequest, error) {
	req.DirectoryPath = strings.TrimSpace(req.DirectoryPath)
	req.InformationRequest = strings.TrimSpace(req.InformationRequest)
	switch strings.TrimSpace(string(req.Kind)) {
	case "sync", "sync_workspace", "sync-workspace":
		req.Kind = TaskKindSync
	case "retrieve", "codebase_retrieval", "codebase-retrieval":
		req.Kind = TaskKindRetrieve
	default:
		return TaskRequest{}, fmt.Errorf("unknown task kind: %s", req.Kind)
	}
	if req.DirectoryPath == "" {
		return TaskRequest{}, errors.New("directory_path is required")
	}
	if req.Kind == TaskKindRetrieve && req.InformationRequest == "" {
		return TaskRequest{}, errors.New("information_request is required")
	}
	return req, nil
}

func cloneTask(in TaskSnapshot) TaskSnapshot {
	out := in
	if in.StartedAt != nil {
		started := *in.StartedAt
		out.StartedAt = &started
	}
	if in.CompletedAt != nil {
		completed := *in.CompletedAt
		out.CompletedAt = &completed
	}
	if in.Result != nil {
		result := *in.Result
		out.Result = &result
	}
	return out
}

func cloneTaskSummary(in TaskSnapshot) TaskSnapshot {
	out := cloneTask(in)
	if out.Result != nil {
		out.Result.Text = ""
	}
	return out
}

func isTerminal(state TaskState) bool {
	return state == TaskStateCompleted || state == TaskStateFailed || state == TaskStateCancelled
}

func newTaskID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
