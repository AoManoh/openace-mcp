package engine

import (
	"encoding/json"
	"testing"
)

func TestResultSummary(t *testing.T) {
	result := Result{CheckpointID: "cp-1", FileCount: 3, Uploaded: 2, Added: 1, Deleted: 0}
	want := "checkpoint=cp-1 files=3 uploaded=2 added=1 deleted=0"
	if got := result.Summary(); got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
	result.ProviderProfileID = "p1"
	want = "provider_profile_id=p1 checkpoint=cp-1 files=3 uploaded=2 added=1 deleted=0"
	if got := result.Summary(); got != want {
		t.Fatalf("Summary() with provider = %q, want %q", got, want)
	}
}

// TestResultJSONWireCompatibility 锁定迁移自 workspace.Result 的 wire 形状：
// 旧字段键名不变，新通用字段在 legacy 路径（空值）下不出现。
func TestResultJSONWireCompatibility(t *testing.T) {
	payload, err := json.Marshal(Result{Text: "t", CheckpointID: "cp", FileCount: 1, Uploaded: 2, Added: 3, Deleted: 4})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("unmarshal result keys: %v", err)
	}
	for _, want := range []string{"Text", "CheckpointID", "FileCount", "Uploaded", "Added", "Deleted"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("result JSON 缺少既有键 %q: %s", want, payload)
		}
	}
	for _, absent := range []string{"index_revision", "engine", "provider_profile_id", "multi_status", "served_by"} {
		if _, ok := keys[absent]; ok {
			t.Fatalf("result JSON 不应输出空的 %q: %s", absent, payload)
		}
	}
}

func TestWorkspaceStatusJSONWireCompatibility(t *testing.T) {
	payload, err := json.Marshal(WorkspaceStatus{DirectoryPath: "/w", Stage: IndexStageReady, FileCount: 1})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("unmarshal status keys: %v", err)
	}
	for _, want := range []string{"directory_path", "file_count", "in_flight", "stage"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("status JSON 缺少既有键 %q: %s", want, payload)
		}
	}
	for _, absent := range []string{"index_revision", "engine", "checkpoint_id"} {
		if _, ok := keys[absent]; ok {
			t.Fatalf("status JSON 不应输出空的 %q: %s", absent, payload)
		}
	}
	if keys["stage"] != string(IndexStageReady) {
		t.Fatalf("stage = %v, want %q", keys["stage"], IndexStageReady)
	}
}
