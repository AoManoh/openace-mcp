package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

	// 状态查询走 direct 模式的 inspector 不可用路径以外的分支：
	// direct localengine 无 Tasker，workspace_status 工具应不注册（与 legacy direct 一致）。
	toolsList := runMCP(t, server, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if strings.Contains(toolsList, `"workspace_status"`) {
		t.Fatalf("direct 模式不应注册 workspace_status 工具: %s", toolsList)
	}
	if !strings.Contains(toolsList, `"codebase_retrieval"`) || !strings.Contains(toolsList, `"sync_workspace"`) {
		t.Fatalf("核心工具应注册: %s", toolsList)
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
