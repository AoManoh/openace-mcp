package mcp

import (
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// 方案③(2026-08-02 批准)文案契约:工具描述引擎中立、参数描述齐备、
// 长度守 1024 上限(部分客户端截断阈值)。
func TestToolDescriptionsContract(t *testing.T) {
	tools := []map[string]any{
		retrievalTool(), multiRetrievalTool(), syncTool(),
		startRetrievalTool(), startMultiRetrievalTool(), startSyncTool(),
		taskStatusTool(), listTasksTool(), cancelTaskTool(),
		daemonStatusTool(), listWorkspacesTool(), workspaceStatusTool(),
	}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)
		if desc == "" {
			t.Fatalf("%s: description 不得为空", name)
		}
		if len(desc) > 1024 {
			t.Fatalf("%s: description 超 1024 字符(%d)", name, len(desc))
		}
		// 引擎中立:默认引擎已是 local-hybrid,工具面不得再写上游品牌
		// 或 "ACE 检索流" 叙事("openACE" 产品名除外)。
		neutral := strings.ReplaceAll(desc, "openACE", "")
		if strings.Contains(neutral, "Augment") || strings.Contains(neutral, "ACE") {
			t.Fatalf("%s: description 残留上游品牌叙事: %q", name, desc)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if p, ok := props["information_request"].(map[string]any); ok {
			d, _ := p["description"].(string)
			if !strings.Contains(d, "identifiers") {
				t.Fatalf("%s: information_request 参数描述缺失或未含措辞引导: %q", name, d)
			}
			if len(d) > 1024 {
				t.Fatalf("%s: information_request 描述超长(%d)", name, len(d))
			}
		}
	}
	if len(serverInstructions) > 1024 {
		t.Fatalf("instructions 超 1024 字符(%d)", len(serverInstructions))
	}
	if !strings.Contains(serverInstructions, "codebase_retrieval") {
		t.Fatal("instructions 应指名检索工具")
	}
}

// 巨量灰度回归:path_prefix 不得从截断目录图推断排他子树。
func TestPathPrefixDescriptionGuardsAgainstTruncatedMapInference(t *testing.T) {
	for _, want := range []string{"ONLY", "truncated repo_map", "ambiguous conceptual"} {
		if !strings.Contains(pathPrefixDescription, want) {
			t.Fatalf("path_prefix 防误收窄契约缺 %q: %s", want, pathPrefixDescription)
		}
	}
}

// 跨 profile 复用量须在同步结构化结果中可机读。
func TestSyncStructuredIncludesCrossProfileReuse(t *testing.T) {
	fields := syncStructured(engine.Result{
		Engine: engine.EngineLocalHybrid, IndexRevision: "rev-1", FileCount: 3,
		Added: 4, BuildMode: "full:first-build", CrossProfileReused: 99,
		SemanticCoverage: "100%",
	})
	if fields["cross_profile_reused"] != 99 || fields["build_mode"] != "full:first-build" || fields["semantic_coverage"] != "100%" {
		t.Fatalf("sync structured 复用字段缺失: %+v", fields)
	}
}

// D2 防漂移:knownToolList(工具面允许列表校验依据)必须与 handler 表
// 键集逐一对应——新增工具漏登记会让 OPENACE_MCP_TOOLS 误报未知。
func TestKnownToolListMatchesHandlers(t *testing.T) {
	handlers := (&Server{}).toolHandlers()
	known := knownToolList()
	if len(known) != len(handlers) {
		t.Fatalf("knownToolList(%d) 与 handler 表(%d)数量不一致", len(known), len(handlers))
	}
	for _, name := range known {
		if _, ok := handlers[name]; !ok {
			t.Fatalf("knownToolList 含 handler 表外的 %q", name)
		}
	}
}
