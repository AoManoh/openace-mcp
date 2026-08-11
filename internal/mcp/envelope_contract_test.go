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

// envelope 合同回归(方案分类终表 D 项,2026-08-11 用户批准):
// 上游 CBM v0.10.0 曾出现 schema-honoring client 收到 {} 静默成功;
// 本组测试以真实 localengine + stdio 循环锁定 openACE 的 client envelope:
// 逐行 framing、request ID 对账(含类型保真)、notification 零响应、
// 非空 text/structuredContent、schema 已声明字段(detail/path_prefix)
// 在单仓与 multi 两条链上真实生效、unknown 额外字段容忍。
func TestEnvelopeContractFramingIDsAndDeclaredFields(t *testing.T) {
	t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
	t.Setenv("OPENACE_CACHE_NAMESPACE", "envelope")
	t.Setenv(EnvMCPTools, "all")
	t.Setenv("VOYAGE_API_KEY", "")

	rootOne := t.TempDir()
	rootTwo := t.TempDir()
	mainGo := `package app

// ResolveWorkspaceKey derives the cache key for a workspace.
func ResolveWorkspaceKey(path string) string {
	return "ws-" + path
}
`
	subGo := `package sub

// ResolveWorkspaceKey variant kept inside the sub tree for prefix tests.
func ResolveWorkspaceKey(path string) string {
	return "sub-" + path
}
`
	if err := os.WriteFile(filepath.Join(rootOne, "main.go"), []byte(mainGo), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootOne, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootOne, "sub", "inner.go"), []byte(subGo), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootTwo, "main.go"), []byte(mainGo), 0o600); err != nil {
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
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"str-id","method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":` + jsonString(rootOne) + `}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"multi_codebase_retrieval","arguments":{"directory_paths":[` + jsonString(rootOne) + `,` + jsonString(rootTwo) + `],"information_request":"ResolveWorkspaceKey","detail":"paths"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":` + jsonString(rootOne) + `,"future_hint":"ignored-extra-field"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":` + jsonString(rootOne) + `,"detail":"paths","path_prefix":"sub"}}}`,
	}
	var out bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}

	responses := strings.Split(strings.TrimSpace(out.String()), "\n")
	// notification 不得产生响应:5 个带 id 请求 = 5 行响应。
	if len(responses) != 5 {
		t.Fatalf("应有 5 个响应(notification 零响应): %d\n%s", len(responses), out.String())
	}

	// framing:每行必须是独立完整 JSON;ID 对账:顺序与类型保真。
	type envelope struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	wantIDs := []string{`1`, `"str-id"`, `4`, `5`, `6`}
	decoded := make([]envelope, len(responses))
	for i, line := range responses {
		if err := json.Unmarshal([]byte(line), &decoded[i]); err != nil {
			t.Fatalf("响应第 %d 行不是完整 JSON: %v\n%s", i+1, err, line)
		}
		if got := string(decoded[i].ID); got != wantIDs[i] {
			t.Fatalf("响应 %d 的 id 对账失败: got %s want %s", i+1, got, wantIDs[i])
		}
		if decoded[i].Error != nil {
			t.Fatalf("响应 %d 不应携带协议错误: %s", i+1, responses[i])
		}
	}

	// schema-honoring client 视角:text 与 structuredContent 双非空。
	var single struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured map[string]any `json:"structuredContent"`
		IsError    bool           `json:"isError"`
	}
	if err := json.Unmarshal(decoded[1].Result, &single); err != nil {
		t.Fatal(err)
	}
	if single.IsError || len(single.Content) == 0 || strings.TrimSpace(single.Content[0].Text) == "" {
		t.Fatalf("单仓检索应返回非空 text: %s", responses[1])
	}
	for _, key := range []string{"engine", "index_revision", "hits"} {
		if _, exists := single.Structured[key]; !exists {
			t.Fatalf("structuredContent 缺声明字段 %s: %s", responses[1], key)
		}
	}

	// P0 修复锁定(multi 链):detail=paths 必须作用于每个 workspace——
	// 每仓段落只有 header 行,零代码围栏。
	multi := responses[2]
	if strings.Contains(multi, `"isError":true`) {
		t.Fatalf("multi 检索不应报错: %s", multi)
	}
	if !strings.Contains(multi, "## main.go:") || strings.Contains(multi, "```") {
		t.Fatalf("multi detail=paths 应只回 header 行(声明字段必须生效): %s", multi)
	}

	// unknown 额外字段按当前 schema 允许,不得拒绝或静默失败。
	if strings.Contains(responses[3], `"isError":true`) {
		t.Fatalf("unknown 额外字段应被容忍: %s", responses[3])
	}
	if !strings.Contains(responses[3], "main.go:") {
		t.Fatalf("带额外字段的检索应返回真实结果: %s", responses[3])
	}

	// path_prefix 在单仓 paths 模式下真实生效:只回 sub/ 子树。
	prefixed := responses[4]
	if !strings.Contains(prefixed, "## sub/inner.go:") {
		t.Fatalf("path_prefix=sub 应命中子树: %s", prefixed)
	}
	if strings.Contains(prefixed, "## main.go:") {
		t.Fatalf("path_prefix=sub 不应返回子树外文件: %s", prefixed)
	}
}
