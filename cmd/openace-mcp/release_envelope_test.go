package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReleaseBinaryEnvelope 是真实发布产物的 stdio envelope 验收
// (方案分类终表 D 项):上游 CBM v0.10.0 的 {} 静默成功事故证明
// envelope 合同必须在真实 release artifact 上验证,进程内测试无法
// 覆盖 main() 装配层与真实 stdio 管道。
//
// 默认跳过;发布验收时:
//
//	go build -tags "grammar_subset,..." -o /tmp/openace-mcp-rc ./cmd/openace-mcp
//	OPENACE_E2E_RELEASE_BINARY=/tmp/openace-mcp-rc go test ./cmd/openace-mcp -run TestReleaseBinaryEnvelope -count=1
func TestReleaseBinaryEnvelope(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("OPENACE_E2E_RELEASE_BINARY"))
	if bin == "" {
		t.Skip("set OPENACE_E2E_RELEASE_BINARY=<built binary> to run the release envelope acceptance")
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
	rootJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}

	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"str-id","method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":` + string(rootJSON) + `}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"codebase_retrieval","arguments":{"information_request":"ResolveWorkspaceKey","directory_path":` + string(rootJSON) + `,"detail":"paths"}}}`,
	}

	cmd := exec.Command(bin)
	// 发布验收固定 direct 零凭据形态:不拉起/复用 daemon,纯词法完整可用。
	cmd.Env = append(os.Environ(),
		"OPENACE_MODE=direct",
		"OPENACE_CACHE_DIR="+t.TempDir(),
		"OPENACE_CACHE_NAMESPACE=release-envelope",
		"VOYAGE_API_KEY=",
		"OPENACE_EMBEDDING_API_KEY=",
		"OPENACE_RERANK_API_KEY=",
	)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("release binary 退出异常: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("release binary 未在 60s 内完成 stdio 会话\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}

	responses := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(responses) != 3 {
		t.Fatalf("应有 3 个响应(notification 零响应): %d\nstdout: %s\nstderr: %s", len(responses), stdout.String(), stderr.String())
	}

	type envelope struct {
		ID     json.RawMessage `json:"id"`
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	wantIDs := []string{`1`, `"str-id"`, `3`}
	for i, line := range responses {
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("响应第 %d 行不是完整 JSON(framing): %v\n%s", i+1, err, line)
		}
		if got := string(env.ID); got != wantIDs[i] {
			t.Fatalf("响应 %d 的 id 对账失败: got %s want %s", i+1, got, wantIDs[i])
		}
		if env.Error != nil {
			t.Fatalf("响应 %d 不应携带协议错误: %s", i+1, line)
		}
	}
	if !strings.Contains(responses[0], "openace-codebase") {
		t.Fatalf("initialize serverInfo 异常: %s", responses[0])
	}
	full := responses[1]
	if strings.Contains(full, `"isError":true`) || !strings.Contains(full, "main.go:") || !strings.Contains(full, "structuredContent") {
		t.Fatalf("默认检索应返回非空 text+structuredContent: %s", full)
	}
	paths := responses[2]
	if !strings.Contains(paths, "## main.go:") || strings.Contains(paths, "```") {
		t.Fatalf("detail=paths 在 release binary 上必须真实生效: %s", paths)
	}
}
