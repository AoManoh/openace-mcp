package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/localengine"
)

// TestLocalEngineEndToEnd 是 Stage 2 北极星验收的自动化版本：
// 无任何 API key / 网络依赖，真实 localengine 走 MCP 工具面完成
// sync → retrieval → status 闭环，检索结果携带可验证 path:line。
func TestLocalEngineEndToEnd(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "e2e")
	t.Setenv(EnvMCPTools, "all") // 本测试断言完整能力面注册(P9),非默认面
	// 断言全程无凭据。
	for _, key := range []string{"VOYAGE_API_KEY"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	mainGo := `package app

// ResolveWorkspaceKey derives the cache key for a workspace.
func ResolveWorkspaceKey(path string) string {
	return "ws-" + path
}
`
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(mainGo), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := localengine.New(localengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	server := NewServer(service)

	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sync_workspace","arguments":{"directory_path":` + jsonString(root) + `}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":` + jsonString(root) + `}}}`,
	}
	var out bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	responses := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(responses) != 3 {
		t.Fatalf("应有 3 个响应: %d\n%s", len(responses), out.String())
	}
	if !strings.Contains(responses[0], "openace-codebase") {
		t.Fatalf("initialize 响应异常: %s", responses[0])
	}
	if !strings.Contains(responses[1], "Workspace synced.") {
		t.Fatalf("sync 响应异常: %s", responses[1])
	}
	retrieval := responses[2]
	if strings.Contains(retrieval, `"isError":true`) {
		t.Fatalf("retrieval 不应报错: %s", retrieval)
	}
	// Stage 4 D8/S19：local-hybrid 摘要行由空 checkpoint= 改为 revision=
	// 口径（golden 同步更新，K53）。
	for _, want := range []string{"main.go:", "ResolveWorkspaceKey", "revision="} {
		if !strings.Contains(retrieval, want) {
			t.Fatalf("retrieval 结果缺少 %q: %s", want, retrieval)
		}
	}
	if strings.Contains(retrieval, "[DEGRADED]") {
		t.Fatal("lexical-only 是完整能力，不得标记 DEGRADED（阶段计划 D2）")
	}

	// P9(review 2026-08-06):direct 模式注册只读状态面——旧契约
	// "与 legacy direct 一致"的依据随 Stage 7 删除失效;语义覆盖缺口
	// 在 direct 形态必须有处可查。
	toolsList := runMCP(t, server, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if !strings.Contains(toolsList, `"workspace_status"`) {
		t.Fatalf("direct 模式应注册 workspace_status 只读状态工具(P9): %s", toolsList)
	}
	if !strings.Contains(toolsList, `"codebase_retrieval"`) || !strings.Contains(toolsList, `"sync_workspace"`) {
		t.Fatalf("核心工具应注册: %s", toolsList)
	}
	wsStatus := runMCP(t, server, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"workspace_status","arguments":{"directory_path":`+strconv.Quote(root)+`}}}`)
	if !strings.Contains(wsStatus, "stage") || !strings.Contains(wsStatus, "file_count") {
		t.Fatalf("direct 模式 workspace_status 应返回状态字段: %s", wsStatus)
	}

	// 框架 18.2/S2:structuredContent 携带 hits 清单/展示统计/阶段耗时;
	// detail=paths 时正文只有 header 行(零代码围栏),内容由调用方
	// 按需 Read。
	structured := runMCP(t, server, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":`+jsonString(root)+`}}}`)
	for _, want := range []string{`"hits"`, `"display"`, `"timings"`, `"shown_blocks"`, `"rank"`, `diagnostics:`, `timings_ms[`, `display[candidates=`} {
		if !strings.Contains(structured, want) {
			t.Fatalf("结构化/文本诊断缺 %s: %s", want, structured)
		}
	}
	paths := runMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":`+jsonString(root)+`,"detail":"paths"}}}`)
	if !strings.Contains(paths, "## main.go:") || strings.Contains(paths, "```") {
		t.Fatalf("paths 模式应只回 header 行: %s", paths)
	}
	bad := runMCP(t, server, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"x","directory_path":`+jsonString(root)+`,"detail":"bogus"}}}`)
	if !strings.Contains(bad, `"isError":true`) || !strings.Contains(bad, "invalid detail") {
		t.Fatalf("非法 detail 应可行动报错: %s", bad)
	}

	// repo_map R1(D4):orientation 面经完整链路可用;完整能力面
	// (本测试 env=all)列出,处于 knownToolList。
	repoMap := runMCP(t, server, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"repo_map","arguments":{"directory_path":`+jsonString(root)+`}}}`)
	if !strings.Contains(repoMap, "repo map:") || !strings.Contains(repoMap, "main.go:1-") || !strings.Contains(repoMap, "ResolveWorkspaceKey") {
		t.Fatalf("repo_map 输出异常: %s", repoMap)
	}
	if !strings.Contains(toolsList, `"repo_map"`) {
		t.Fatalf("完整能力面应列出 repo_map: %s", toolsList)
	}
}

// TestLocalEngineProfileIDRejectedViaMCP 验证 provider_profile_id 在
// local-hybrid 下经 MCP 面返回明确工具错误（迁移方案 §7.3）。
func TestLocalEngineProfileIDRejectedViaMCP(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "e2e")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := localengine.New(localengine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	server := NewServer(service)
	response := runMCP(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"q","directory_path":`+jsonString(root)+`,"provider_profile_id":"p1"}}}`)
	if !strings.Contains(response, `"isError":true`) || !strings.Contains(response, "provider_profile_id") {
		t.Fatalf("应返回可解释的工具错误: %s", response)
	}
}

func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}
