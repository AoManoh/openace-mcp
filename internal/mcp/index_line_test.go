package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 第 5 步 T5.5：用旧 revision 降级作答时，正文第二行的 [INDEX] 行与结构化的
// index_freshness 在单工作区与跨工作区检索结果里都保留下来。

const testIndexLine = "[INDEX] snapshot=rev-old published=2026-11-05T01:02:03Z scanned=2026-11-05T01:07:03Z pending=2: main.go, old.md (deleted)"

func testFreshness() *engine.IndexFreshness {
	published := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	scanned := published.Add(5 * time.Minute)
	return &engine.IndexFreshness{
		SnapshotRevision: "rev-old", PublishedAt: &published, ScannedAt: &scanned,
		PendingKnown: true, PendingCount: 2,
		PendingFiles: []engine.PendingFile{{Path: "main.go", Change: "modified"}, {Path: "old.md", Change: "deleted"}},
	}
}

// indexLineSyncer 让路径含 building 的工作区返回 index-building 降级结果，其余健康。
type indexLineSyncer struct{}

func (indexLineSyncer) Search(ctx context.Context, req engine.SearchRequest) (engine.Result, error) {
	dir := req.Workspace.DirectoryPath
	if strings.Contains(dir, "building") {
		return engine.Result{
			Text:             "[DEGRADED] index-building; mode=hybrid; semantic_coverage=100%\n" + testIndexLine + "\n\nretrieved from " + dir,
			Engine:           engine.EngineLocalHybrid,
			IndexRevision:    "rev-old",
			RetrievalMode:    "hybrid",
			DegradedReason:   "index-building",
			SemanticCoverage: "100%",
			IndexFreshness:   testFreshness(),
		}, nil
	}
	return engine.Result{Text: "retrieved from " + dir, RetrievalMode: "hybrid", SemanticCoverage: "100%"}, nil
}

func (indexLineSyncer) Sync(ctx context.Context, req engine.SyncRequest) (engine.Result, error) {
	return engine.Result{FileCount: 1}, nil
}

type indexLineResponse struct {
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]json.RawMessage `json:"structuredContent"`
	} `json:"result"`
}

func decodeIndexLineResponse(t *testing.T, out string) indexLineResponse {
	t.Helper()
	var response indexLineResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("decode MCP response: %v\n%s", err, out)
	}
	if len(response.Result.Content) == 0 {
		t.Fatalf("MCP response has no text content: %s", out)
	}
	return response
}

func TestCodebaseRetrievalKeepsIndexLine(t *testing.T) {
	out := runMCP(t, NewServer(indexLineSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"directory_path":"/tmp/building","information_request":"find code"}}}`)
	response := decodeIndexLineResponse(t, out)
	lines := strings.SplitN(response.Result.Content[0].Text, "\n", 3)
	if len(lines) < 3 || lines[0] != "[DEGRADED] index-building; mode=hybrid; semantic_coverage=100%" || lines[1] != testIndexLine {
		t.Fatalf("单工作区正文前两行应是 [DEGRADED] 横幅与 [INDEX] 行: %q", lines)
	}
	var freshness engine.IndexFreshness
	if err := json.Unmarshal(response.Result.StructuredContent["index_freshness"], &freshness); err != nil {
		t.Fatalf("structuredContent 应带 index_freshness: %v\n%s", err, out)
	}
	if freshness.SnapshotRevision != "rev-old" || !freshness.PendingKnown || freshness.PendingCount != 2 || len(freshness.PendingFiles) != 2 || freshness.PendingFiles[1].Change != "deleted" {
		t.Fatalf("index_freshness 字段不符: %+v", freshness)
	}
}

func TestMultiCodebaseRetrievalKeepsIndexLine(t *testing.T) {
	out := runMCP(t, NewServer(indexLineSyncer{}), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":["/tmp/fresh","/tmp/building"],"information_request":"find shared code"}}}`)
	response := decodeIndexLineResponse(t, out)
	text := response.Result.Content[0].Text
	section := text[strings.Index(text, "## /tmp/building\n"):]
	if !strings.HasPrefix(section, "## /tmp/building\n[DEGRADED] index-building; mode=hybrid; semantic_coverage=100%\n"+testIndexLine+"\n") {
		t.Fatalf("聚合正文应保留该工作区的 [INDEX] 行: %q", text)
	}
	if strings.Count(text, "[INDEX]") != 1 {
		t.Fatalf("健康的工作区不应出现 [INDEX] 行: %q", text)
	}
	var status engine.MultiRetrievalStatus
	if err := json.Unmarshal(response.Result.StructuredContent["multi_status"], &status); err != nil {
		t.Fatalf("decode multi_status: %v\n%s", err, out)
	}
	for _, ws := range status.Workspaces {
		switch ws.DirectoryPath {
		case "/tmp/building":
			if ws.IndexFreshness == nil || ws.IndexFreshness.SnapshotRevision != "rev-old" || ws.IndexFreshness.PendingCount != 2 {
				t.Fatalf("multi_status 的降级工作区应带 index_freshness: %+v", ws)
			}
		default:
			if ws.IndexFreshness != nil {
				t.Fatalf("健康的工作区不应带 index_freshness: %+v", ws)
			}
		}
	}
}

func TestRetrievalDescriptionExplainsIndexLine(t *testing.T) {
	for _, want := range []string{"[INDEX]", "index-building", "index-refreshing", "stale-index"} {
		if !strings.Contains(retrievalDescription, want) {
			t.Fatalf("codebase_retrieval 的描述应说明 [INDEX] 行，缺 %q: %s", want, retrievalDescription)
		}
	}
}
