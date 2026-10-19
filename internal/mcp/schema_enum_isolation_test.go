package mcp

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

func schemaEnums(tool map[string]any) ([]string, []string) {
	props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	return props["detail"].(map[string]any)["enum"].([]string), props["artifact_kind"].(map[string]any)["enum"].([]string)
}

func TestRetrievalSchemaEnumMutationStaysLocal(t *testing.T) {
	detail, kind := schemaEnums(retrievalTool())
	wantDetail := strings.Join(detail, ",")
	wantKind := strings.Join(kind, ",")
	detail[0] = "mutated-detail"
	kind[0] = "mutated-kind"
	for _, tool := range []map[string]any{retrievalTool(), multiRetrievalTool(), startRetrievalTool(), startMultiRetrievalTool()} {
		gotDetail, gotKind := schemaEnums(tool)
		if strings.Join(gotDetail, ",") != wantDetail || strings.Join(gotKind, ",") != wantKind {
			t.Fatalf("%v: mutating one schema changed another: detail=%v kind=%v want detail=%s kind=%s", tool["name"], gotDetail, gotKind, wantDetail, wantKind)
		}
		for _, value := range gotDetail {
			if got, err := engine.NormalizeDetail(value); err != nil || got != value {
				t.Fatalf("schema detail value %q must be accepted as itself: got %q err=%v", value, got, err)
			}
		}
		for _, value := range gotKind {
			if got, err := engine.NormalizeArtifactKind(value); err != nil || got != value {
				t.Fatalf("schema artifact_kind value %q must be accepted as itself: got %q err=%v", value, got, err)
			}
		}
	}
	if _, err := engine.NormalizeDetail("mutated-detail"); err == nil || !engine.IsInvalidRequest(err) {
		t.Fatalf("unlisted detail must stay invalid: %v", err)
	}
	if _, err := engine.NormalizeArtifactKind("mutated-kind"); err == nil || !engine.IsInvalidRequest(err) {
		t.Fatalf("unlisted artifact_kind must stay invalid: %v", err)
	}
}
