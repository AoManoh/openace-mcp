package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestArtifactKindForwardedThroughDaemon(D1 H1 2026-09-03):artifact_kind 经
// /v1/retrieve 同步接口与异步任务两条路都原样到达引擎的 SearchRequest;
// 任务快照也保留该字段(task_status 可见)。
func TestArtifactKindForwardedThroughDaemon(t *testing.T) {
	useTempTaskStore(t)
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	syncer := &capturingSearchSyncer{}
	server := newDaemonHTTPTestServer(t, syncer)

	body, _ := json.Marshal(retrieveRequest{DirectoryPath: "/tmp/one", InformationRequest: "find code", ArtifactKind: "code"})
	resp, err := http.Post(server.URL+"/v1/retrieve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("同步检索应成功: %d", resp.StatusCode)
	}

	task := postTask(t, server.URL, TaskRequest{
		Kind:               TaskKindRetrieve,
		DirectoryPath:      "/tmp/two",
		InformationRequest: "find docs",
		ArtifactKind:       "docs",
	})
	snapshot := pollHTTPTask(t, server.URL, task.ID, TaskStateCompleted)
	if snapshot.ArtifactKind != "docs" {
		t.Fatalf("任务快照应保留 artifact_kind: %+v", snapshot)
	}

	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if len(syncer.requests) != 2 {
		t.Fatalf("expected 2 search requests, got %d", len(syncer.requests))
	}
	got := map[string]string{}
	for _, req := range syncer.requests {
		got[req.Workspace.DirectoryPath] = req.ArtifactKind
	}
	if got["/tmp/one"] != "code" || got["/tmp/two"] != "docs" {
		t.Fatalf("artifact_kind 应原样到达引擎: %v", got)
	}
}
