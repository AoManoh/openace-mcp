package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type fakeSyncer struct{}

func (fakeSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	return workspace.Result{CheckpointID: "checkpoint", FileCount: 1}, nil
}

func (fakeSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	if strings.Contains(dir, "bad") {
		return workspace.Result{}, errors.New("workspace failed")
	}
	return workspace.Result{Text: "retrieved " + dir, CheckpointID: "checkpoint", FileCount: 1}, nil
}

type fakeWorkspaceSyncer struct {
	fakeSyncer
}

func (fakeWorkspaceSyncer) ListWorkspaceStatuses(ctx context.Context) ([]workspace.WorkspaceStatus, error) {
	return []workspace.WorkspaceStatus{{
		DirectoryPath:          "/tmp/project",
		CheckpointID:           "checkpoint",
		FileCount:              3,
		UpstreamStatus:         "backoff",
		UpstreamLastStatusCode: 429,
		UpstreamRetryAfter:     "30s",
		UpstreamLastError:      "find-missing returned HTTP 429: quota exhausted",
	}}, nil
}

func (fakeWorkspaceSyncer) WorkspaceStatus(ctx context.Context, dir string) (workspace.WorkspaceStatus, error) {
	return workspace.WorkspaceStatus{
		DirectoryPath:          dir,
		CheckpointID:           "checkpoint",
		FileCount:              3,
		UpstreamStatus:         "backoff",
		UpstreamLastStatusCode: 429,
		UpstreamRetryAfter:     "30s",
		UpstreamLastError:      "find-missing returned HTTP 429: quota exhausted",
	}, nil
}

type fakeProviderWorkspaceSyncer struct {
	fakeWorkspaceSyncer
}

func (fakeProviderWorkspaceSyncer) SyncWithProvider(ctx context.Context, dir string, providerProfileID string) (workspace.Result, error) {
	return workspace.Result{ProviderProfileID: providerProfileID, CheckpointID: "checkpoint-" + providerProfileID, FileCount: 1}, nil
}

func (fakeProviderWorkspaceSyncer) RetrieveWithProvider(ctx context.Context, dir string, providerProfileID string, query string, maxOutputLen int) (workspace.Result, error) {
	return workspace.Result{Text: "retrieved " + providerProfileID, ProviderProfileID: providerProfileID, CheckpointID: "checkpoint-" + providerProfileID, FileCount: 1}, nil
}

func (fakeProviderWorkspaceSyncer) WorkspaceStatusWithProvider(ctx context.Context, dir string, providerProfileID string) (workspace.WorkspaceStatus, error) {
	return workspace.WorkspaceStatus{
		DirectoryPath:     dir,
		ProviderProfileID: providerProfileID,
		ProviderState:     "ready",
		CheckpointID:      "checkpoint-" + providerProfileID,
		FileCount:         1,
	}, nil
}

type fakeWatchWorkspaceSyncer struct {
	fakeSyncer
}

func (fakeWatchWorkspaceSyncer) WorkspaceChanged(context.Context, string) (bool, error) {
	return false, nil
}

func (fakeWatchWorkspaceSyncer) SyncBackground(context.Context, string) (workspace.Result, error) {
	return workspace.Result{CheckpointID: "checkpoint-background", FileCount: 3}, nil
}

func (fakeWatchWorkspaceSyncer) ListWorkspaceStatuses(context.Context) ([]workspace.WorkspaceStatus, error) {
	return []workspace.WorkspaceStatus{{
		DirectoryPath: "/tmp/project",
		CheckpointID:  "checkpoint",
		FileCount:     3,
	}}, nil
}

func (fakeWatchWorkspaceSyncer) WorkspaceStatus(ctx context.Context, dir string) (workspace.WorkspaceStatus, error) {
	return workspace.WorkspaceStatus{
		DirectoryPath: dir,
		CheckpointID:  "checkpoint",
		FileCount:     3,
	}, nil
}

func TestServerTaskLifecycle(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := newDaemonHTTPTestServer(t, fakeSyncer{})

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
	if !strings.Contains(completed.Result.Text, "retrieved") {
		t.Fatalf("unexpected task result: %+v", completed.Result)
	}
}

func TestServerListsTasks(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := newDaemonHTTPTestServer(t, fakeSyncer{})

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
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "local-test-token")
	server := newDaemonHTTPTestServer(t, fakeSyncer{})

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

func TestServerAllowsConcurrentSyncRequests(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	syncer := newConcurrentSyncer()
	server := newDaemonHTTPTestServer(t, syncer)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, dir := range []string{"/tmp/workspace-a", "/tmp/workspace-b"} {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			errs <- postSync(server.URL, dir)
		}(dir)
	}

	syncer.waitForOverlap(t)
	syncer.release()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestServerWorkspaceStatus(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := newDaemonHTTPTestServer(t, fakeWorkspaceSyncer{})

	resp, err := http.Get(server.URL + "/v1/workspaces")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected list status: %s", resp.Status)
	}
	var list struct {
		Workspaces []workspace.WorkspaceStatus `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Workspaces) != 1 || list.Workspaces[0].CheckpointID != "checkpoint" {
		t.Fatalf("unexpected workspace list: %+v", list)
	}
	if list.Workspaces[0].UpstreamStatus != "backoff" || list.Workspaces[0].UpstreamLastStatusCode != 429 {
		t.Fatalf("workspace list should include upstream health: %+v", list)
	}

	payload, err := json.Marshal(workspaceStatusRequest{DirectoryPath: "/tmp/project"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Post(server.URL+"/v1/workspace/status", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status response: %s", resp.Status)
	}
	var status workspace.WorkspaceStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.DirectoryPath != "/tmp/project" || status.FileCount != 3 {
		t.Fatalf("unexpected workspace status: %+v", status)
	}
	if status.UpstreamStatus != "backoff" || status.UpstreamRetryAfter != "30s" {
		t.Fatalf("workspace status should include upstream health: %+v", status)
	}
}

func TestServerRoutesProviderProfileRequests(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := newDaemonHTTPTestServer(t, fakeProviderWorkspaceSyncer{})

	retrievePayload, err := json.Marshal(retrieveRequest{
		DirectoryPath:      "/tmp/project",
		ProviderProfileID:  "standby",
		InformationRequest: "find code",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(server.URL+"/v1/retrieve", "application/json", bytes.NewReader(retrievePayload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected retrieve status: %s", resp.Status)
	}
	var result workspace.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.ProviderProfileID != "standby" || result.CheckpointID != "checkpoint-standby" {
		t.Fatalf("unexpected provider retrieve result: %+v", result)
	}

	statusPayload, err := json.Marshal(workspaceStatusRequest{DirectoryPath: "/tmp/project", ProviderProfileID: "standby"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Post(server.URL+"/v1/workspace/status", "application/json", bytes.NewReader(statusPayload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status response: %s", resp.Status)
	}
	var status workspace.WorkspaceStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.ProviderProfileID != "standby" || status.CheckpointID != "checkpoint-standby" {
		t.Fatalf("unexpected provider workspace status: %+v", status)
	}
}

func TestServerWorkspaceStatusIncludesWatchDiagnosticsForSeenWorkspace(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1h")
	syncer := fakeWatchWorkspaceSyncer{}
	server := newDaemonHTTPTestServer(t, syncer)

	if err := postSync(server.URL, "/tmp/project"); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workspaceStatusRequest{DirectoryPath: "/tmp/project"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(server.URL+"/v1/workspace/status", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status response: %s", resp.Status)
	}
	var status workspace.WorkspaceStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.WatchEnabled || !status.WatchScheduled || status.WatchRunning || status.NextWatchAt == nil {
		t.Fatalf("workspace status should include watch diagnostics: %+v", status)
	}
}

func TestServerMultiRetrieveRegistersWatchDiagnostics(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	t.Setenv("OPENACE_WATCH_MODE", "seen")
	t.Setenv("OPENACE_WATCH_DEBOUNCE", "1h")
	server := newDaemonHTTPTestServer(t, fakeWatchWorkspaceSyncer{})

	task := postTask(t, server.URL, TaskRequest{
		Kind:               TaskKindMultiRetrieve,
		DirectoryPaths:     []string{"/tmp/project", "/tmp/other"},
		InformationRequest: "find shared code",
	})
	pollHTTPTask(t, server.URL, task.ID, TaskStateCompleted)

	payload, err := json.Marshal(workspaceStatusRequest{DirectoryPath: "/tmp/project"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(server.URL+"/v1/workspace/status", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status response: %s", resp.Status)
	}
	var status workspace.WorkspaceStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.WatchEnabled || !status.WatchScheduled {
		t.Fatalf("multi retrieve should register workspace with watcher: %+v", status)
	}
}

func TestServerCompletesMultiRetrieveTask(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := newDaemonHTTPTestServer(t, fakeSyncer{})

	task := postTask(t, server.URL, TaskRequest{
		Kind:               TaskKindMultiRetrieve,
		DirectoryPaths:     []string{"/tmp/one", "/tmp/bad"},
		InformationRequest: "find shared code",
	})
	completed := pollHTTPTask(t, server.URL, task.ID, TaskStateCompleted)
	if completed.Result == nil {
		t.Fatal("completed multi retrieve task should include result")
	}
	if !strings.Contains(completed.Result.Text, "/tmp/one") || !strings.Contains(completed.Result.Text, "retrieved /tmp/one") {
		t.Fatalf("result should include successful workspace: %+v", completed.Result)
	}
	if !strings.Contains(completed.Result.Text, "/tmp/bad") || !strings.Contains(completed.Result.Text, "workspace failed") {
		t.Fatalf("result should include failed workspace error: %+v", completed.Result)
	}
	if len(completed.DirectoryPaths) != 2 {
		t.Fatalf("task should retain directory paths: %+v", completed)
	}
}

func TestServerFailsMultiRetrieveTaskWhenAllWorkspacesFail(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	server := newDaemonHTTPTestServer(t, fakeSyncer{})

	task := postTask(t, server.URL, TaskRequest{
		Kind:               TaskKindMultiRetrieve,
		DirectoryPaths:     []string{"/tmp/bad-one", "/tmp/bad-two"},
		InformationRequest: "find shared code",
	})
	failed := pollHTTPTask(t, server.URL, task.ID, TaskStateFailed)
	if !strings.Contains(failed.Error, "all workspace retrievals failed") {
		t.Fatalf("all-failed multi retrieve should fail task: %+v", failed)
	}
	if !strings.Contains(failed.Error, "/tmp/bad-one") || !strings.Contains(failed.Error, "/tmp/bad-two") {
		t.Fatalf("failure should retain workspace diagnostics: %+v", failed)
	}
}

func newDaemonHTTPTestServer(t *testing.T, syncer Syncer) *httptest.Server {
	t.Helper()
	openace := NewServer(syncer)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := openace.Shutdown(ctx); err != nil {
			t.Errorf("shutdown daemon server: %v", err)
		}
	})
	server := httptest.NewServer(openace.routes())
	t.Cleanup(server.Close)
	return server
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

func postSync(baseURL string, dir string) error {
	payload, err := json.Marshal(syncRequest{DirectoryPath: dir})
	if err != nil {
		return err
	}
	resp, err := http.Post(baseURL+"/v1/sync", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

type concurrentSyncer struct {
	mu          sync.Mutex
	releaseOnce sync.Once
	releaseCh   chan struct{}
	overlapCh   chan struct{}
	active      int
	maxActive   int
}

func newConcurrentSyncer() *concurrentSyncer {
	return &concurrentSyncer{
		releaseCh: make(chan struct{}),
		overlapCh: make(chan struct{}),
	}
}

func (s *concurrentSyncer) Sync(ctx context.Context, dir string) (workspace.Result, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	if s.active >= 2 {
		select {
		case <-s.overlapCh:
		default:
			close(s.overlapCh)
		}
	}
	s.mu.Unlock()

	select {
	case <-s.releaseCh:
	case <-ctx.Done():
		s.leave()
		return workspace.Result{}, ctx.Err()
	}

	s.leave()
	return workspace.Result{CheckpointID: "checkpoint", FileCount: 1}, nil
}

func (s *concurrentSyncer) Retrieve(ctx context.Context, dir string, query string, maxOutputLen int) (workspace.Result, error) {
	return s.Sync(ctx, dir)
}

func (s *concurrentSyncer) waitForOverlap(t *testing.T) {
	t.Helper()
	select {
	case <-s.overlapCh:
	case <-time.After(2 * time.Second):
		t.Fatal("sync requests did not overlap")
	}
}

func (s *concurrentSyncer) release() {
	s.releaseOnce.Do(func() { close(s.releaseCh) })
}

func (s *concurrentSyncer) leave() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
}
