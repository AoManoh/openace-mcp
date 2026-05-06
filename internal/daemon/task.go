package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openace-mcp/internal/pathutil"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

const defaultTaskQueueSize = 256
const maxTaskQueueSize = 4096
const defaultTaskHistoryLimit = 1024
const maxTaskHistoryLimit = 8192
const defaultTaskWorkerCount = 4
const maxTaskWorkerCount = 32
const MaxMultiWorkspacePaths = 8

type TaskKind string

const (
	TaskKindSync          TaskKind = "sync_workspace"
	TaskKindRetrieve      TaskKind = "codebase_retrieval"
	TaskKindMultiRetrieve TaskKind = "multi_codebase_retrieval"
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
	DirectoryPaths     []string `json:"directory_paths,omitempty"`
	InformationRequest string   `json:"information_request,omitempty"`
	MaxOutputLength    int      `json:"max_output_length,omitempty"`
}

type TaskSnapshot struct {
	ID                 string            `json:"id"`
	Kind               TaskKind          `json:"kind"`
	State              TaskState         `json:"state"`
	DirectoryPath      string            `json:"directory_path"`
	DirectoryPaths     []string          `json:"directory_paths,omitempty"`
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
	mu              sync.Mutex
	runner          TaskRunner
	queue           chan string
	tasks           map[string]*taskRecord
	workerCount     int
	storeDir        string
	closed          bool
	workers         sync.WaitGroup
	persistCh       chan struct{}
	persistStop     chan struct{}
	persistStopOnce sync.Once
	persistWorkers  sync.WaitGroup
}

type taskRecord struct {
	snapshot TaskSnapshot
	request  TaskRequest
	cancel   context.CancelFunc
}

var ErrTaskQueueFull = errors.New("task queue is full")
var ErrTaskStoreClosed = errors.New("task store is shut down")

func NewTaskStore(runner TaskRunner, queueSize int) *TaskStore {
	if queueSize <= 0 {
		queueSize = taskQueueSize()
	}
	return NewTaskStoreWithWorkers(runner, queueSize, taskWorkerCount())
}

func NewTaskStoreWithWorkers(runner TaskRunner, queueSize int, workerCount int) *TaskStore {
	if queueSize <= 0 {
		queueSize = taskQueueSize()
	}
	workerCount = normalizeTaskWorkerCount(workerCount)
	store := &TaskStore{
		runner:      runner,
		queue:       make(chan string, queueSize),
		tasks:       make(map[string]*taskRecord),
		workerCount: workerCount,
		storeDir:    taskStoreDir(),
	}
	store.loadPersisted()
	store.startPersister()
	for i := 0; i < workerCount; i++ {
		store.workers.Add(1)
		go store.worker()
	}
	return store
}

func taskWorkerCount() int {
	value := strings.TrimSpace(os.Getenv("OPENACE_TASK_WORKERS"))
	if value == "" {
		return defaultTaskWorkerCount
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultTaskWorkerCount
	}
	return normalizeTaskWorkerCount(parsed)
}

func taskQueueSize() int {
	value := strings.TrimSpace(os.Getenv("OPENACE_TASK_QUEUE_SIZE"))
	if value == "" {
		return defaultTaskQueueSize
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultTaskQueueSize
	}
	if parsed > maxTaskQueueSize {
		return maxTaskQueueSize
	}
	return parsed
}

func normalizeTaskWorkerCount(value int) int {
	if value <= 0 {
		return defaultTaskWorkerCount
	}
	if value > maxTaskWorkerCount {
		return maxTaskWorkerCount
	}
	return value
}

func taskHistoryLimit() int {
	value := strings.TrimSpace(os.Getenv("OPENACE_TASK_HISTORY_LIMIT"))
	if value == "" {
		return defaultTaskHistoryLimit
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultTaskHistoryLimit
	}
	if parsed > maxTaskHistoryLimit {
		return maxTaskHistoryLimit
	}
	return parsed
}

func (s *TaskStore) WorkerCount() int {
	return s.workerCount
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
			DirectoryPaths:     append([]string(nil), normalized.DirectoryPaths...),
			InformationRequest: normalized.InformationRequest,
			MaxOutputLength:    normalized.MaxOutputLength,
			SubmittedAt:        time.Now().UTC(),
		},
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return TaskSnapshot{}, ErrTaskStoreClosed
	}
	s.tasks[id] = record
	s.pruneLocked(taskHistoryLimit())
	s.persistLocked()
	snapshot := cloneTask(record.snapshot)
	select {
	case s.queue <- id:
		s.mu.Unlock()
		return snapshot, nil
	default:
		delete(s.tasks, id)
		s.persistLocked()
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
		if isPrunableTask(record.snapshot) {
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
	s.persistLocked()
	return cloneTask(record.snapshot), true
}

func (s *TaskStore) worker() {
	defer s.workers.Done()
	for id := range s.queue {
		record, ctx, ok := s.start(id)
		if !ok {
			continue
		}
		result, err := s.runner(ctx, record.request)
		s.finish(id, result, err)
	}
}

func (s *TaskStore) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
		now := time.Now().UTC()
		for _, record := range s.tasks {
			if isTerminal(record.snapshot.State) {
				continue
			}
			if record.cancel != nil {
				record.cancel()
				record.cancel = nil
			}
			record.snapshot.State = TaskStateCancelled
			record.snapshot.CompletedAt = &now
			record.snapshot.Error = "shutdown"
		}
		s.persistNowLocked()
	}
	s.mu.Unlock()

	if err := s.stopPersister(ctx); err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
	s.persistLocked()
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
			s.persistLocked()
			return
		}
		record.snapshot.State = TaskStateFailed
		record.snapshot.Error = err.Error()
		s.persistLocked()
		return
	}
	record.snapshot.State = TaskStateCompleted
	record.snapshot.Result = &result
	s.persistLocked()
}

func (s *TaskStore) startPersister() {
	if s.storeDir == "" {
		return
	}
	s.persistCh = make(chan struct{}, 1)
	s.persistStop = make(chan struct{})
	s.persistWorkers.Add(1)
	go s.persistWorker()
}

func (s *TaskStore) stopPersister(ctx context.Context) error {
	if s.persistStop == nil {
		return nil
	}
	s.persistStopOnce.Do(func() {
		close(s.persistStop)
	})
	done := make(chan struct{})
	go func() {
		s.persistWorkers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *TaskStore) persistWorker() {
	defer s.persistWorkers.Done()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false
	for {
		select {
		case <-s.persistCh:
			if !pending {
				pending = true
				timer.Reset(25 * time.Millisecond)
			}
		case <-timer.C:
			s.persistNow()
			pending = false
		case <-s.persistStop:
			if pending {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			s.persistNow()
			return
		}
	}
}

type taskManifest struct {
	Tasks []TaskSnapshot `json:"tasks"`
}

func (s *TaskStore) loadPersisted() {
	if s.storeDir == "" {
		return
	}
	path := filepath.Join(s.storeDir, "tasks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var manifest taskManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		_ = os.Rename(path, path+".corrupt-"+time.Now().UTC().Format("20060102150405"))
		return
	}
	now := time.Now().UTC()
	for _, snapshot := range manifest.Tasks {
		if snapshot.ID == "" {
			continue
		}
		snapshot = cloneTask(snapshot)
		if !isTerminal(snapshot.State) {
			snapshot.State = TaskStateFailed
			snapshot.Error = "abandoned after daemon restart"
			snapshot.CompletedAt = &now
		}
		s.tasks[snapshot.ID] = &taskRecord{
			request:  requestFromSnapshot(snapshot),
			snapshot: snapshot,
		}
	}
	s.pruneLocked(taskHistoryLimit())
	s.persistLocked()
}

func (s *TaskStore) persistLocked() {
	if s.storeDir == "" {
		return
	}
	if s.persistCh != nil {
		select {
		case s.persistCh <- struct{}{}:
		default:
		}
		return
	}
	s.persistNowLocked()
}

func (s *TaskStore) persistNow() {
	if s.storeDir == "" {
		return
	}
	s.mu.Lock()
	manifest := s.manifestLocked()
	s.mu.Unlock()
	_ = saveTaskManifest(s.storeDir, manifest)
}

func (s *TaskStore) persistNowLocked() {
	if s.storeDir == "" {
		return
	}
	_ = saveTaskManifest(s.storeDir, s.manifestLocked())
}

func (s *TaskStore) manifestLocked() taskManifest {
	protected := make([]TaskSnapshot, 0, len(s.tasks))
	prunable := make([]TaskSnapshot, 0, len(s.tasks))
	for _, record := range s.tasks {
		snapshot := cloneTask(record.snapshot)
		if isPrunableTask(snapshot) {
			prunable = append(prunable, snapshot)
			continue
		}
		protected = append(protected, snapshot)
	}
	sort.Slice(prunable, func(i, j int) bool {
		return prunable[i].SubmittedAt.After(prunable[j].SubmittedAt)
	})
	limit := taskHistoryLimit()
	if remaining := limit - len(protected); remaining < len(prunable) {
		if remaining <= 0 {
			prunable = nil
		} else {
			prunable = prunable[:remaining]
		}
	}
	tasks := append(protected, prunable...)
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].SubmittedAt.After(tasks[j].SubmittedAt)
	})
	return taskManifest{Tasks: tasks}
}

func isPrunableTask(snapshot TaskSnapshot) bool {
	if !isTerminal(snapshot.State) {
		return false
	}
	return snapshot.Error != "shutdown" && snapshot.Error != "abandoned after daemon restart"
}

func saveTaskManifest(dir string, manifest taskManifest) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tasks-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(dir, "tasks.json"))
}

func requestFromSnapshot(snapshot TaskSnapshot) TaskRequest {
	return TaskRequest{
		Kind:               snapshot.Kind,
		DirectoryPath:      snapshot.DirectoryPath,
		DirectoryPaths:     append([]string(nil), snapshot.DirectoryPaths...),
		InformationRequest: snapshot.InformationRequest,
		MaxOutputLength:    snapshot.MaxOutputLength,
	}
}

func taskStoreDir() string {
	if dir := strings.TrimSpace(os.Getenv("OPENACE_TASK_STORE_DIR")); dir != "" {
		return absoluteExpandedPath(dir)
	}
	if dir := strings.TrimSpace(os.Getenv("OPENACE_CACHE_DIR")); dir != "" {
		base := absoluteExpandedPath(dir)
		if base == "" {
			return ""
		}
		return filepath.Join(base, "tasks", cacheNamespace())
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cache, "openace-mcp", "tasks", cacheNamespace())
}

func absoluteExpandedPath(path string) string {
	expanded, err := pathutil.ExpandUser(path)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return ""
	}
	return abs
}

func cacheNamespace() string {
	namespace := strings.TrimSpace(os.Getenv("OPENACE_CACHE_NAMESPACE"))
	if namespace == "" {
		return "default"
	}
	namespace = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, namespace)
	namespace = strings.Trim(namespace, ".-")
	if namespace == "" {
		return "default"
	}
	return namespace
}

func normalizeTaskRequest(req TaskRequest) (TaskRequest, error) {
	req.DirectoryPath = strings.TrimSpace(req.DirectoryPath)
	req.InformationRequest = strings.TrimSpace(req.InformationRequest)
	switch strings.TrimSpace(string(req.Kind)) {
	case "sync", "sync_workspace", "sync-workspace":
		req.Kind = TaskKindSync
	case "retrieve", "codebase_retrieval", "codebase-retrieval":
		req.Kind = TaskKindRetrieve
	case "multi", "multi_codebase_retrieval", "multi-codebase-retrieval":
		req.Kind = TaskKindMultiRetrieve
	default:
		return TaskRequest{}, fmt.Errorf("unknown task kind: %s", req.Kind)
	}
	if req.Kind == TaskKindMultiRetrieve {
		paths, err := normalizeTaskDirectoryPaths(req.DirectoryPaths)
		if err != nil {
			return TaskRequest{}, err
		}
		req.DirectoryPaths = paths
		if req.InformationRequest == "" {
			return TaskRequest{}, errors.New("information_request is required")
		}
		return req, nil
	}
	if req.DirectoryPath == "" {
		return TaskRequest{}, errors.New("directory_path is required")
	}
	if req.Kind == TaskKindRetrieve && req.InformationRequest == "" {
		return TaskRequest{}, errors.New("information_request is required")
	}
	return req, nil
}

func normalizeTaskDirectoryPaths(paths []string) ([]string, error) {
	normalized := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		normalized = append(normalized, path)
	}
	if len(normalized) == 0 {
		return nil, errors.New("directory_paths is required")
	}
	if len(normalized) > MaxMultiWorkspacePaths {
		return nil, fmt.Errorf("directory_paths supports at most %d workspaces", MaxMultiWorkspacePaths)
	}
	return normalized, nil
}

func cloneTask(in TaskSnapshot) TaskSnapshot {
	out := in
	out.DirectoryPaths = append([]string(nil), in.DirectoryPaths...)
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
