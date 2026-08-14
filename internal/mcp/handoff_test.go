package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/engine"
)

// T2(docs/tasks/T2-wrapper-reexec-on-upgrade.md)单元面。红态证据=
// known-issue #13 的 3 个真实样本+本日会话内 identity 硬错实录(A1),
// exec 本体不可在进程内单测,由真实升级链路 E2E 验收(A2)。

func TestHandoffStateRoundTrip(t *testing.T) {
	state := handoffState{PendingLine: `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`, StdinRest: []byte("tail-bytes"), At: time.Now().UTC()}
	path, err := writeHandoffState(state)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("状态文件必须 0600(含用户查询原文): %v", info.Mode())
	}
	t.Setenv(EnvHandoffState, path)
	loaded := loadHandoffState()
	if loaded == nil || loaded.PendingLine != state.PendingLine || string(loaded.StdinRest) != "tail-bytes" {
		t.Fatalf("round-trip 失败: %+v", loaded)
	}
	if os.Getenv(EnvHandoffState) != "" {
		t.Fatal("resume 后 env 必须清除(防嵌套误继承)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("resume 后状态文件必须删除(不留用户查询残档)")
	}
	if loadHandoffState() != nil {
		t.Fatal("空 env 必须返回 nil")
	}
}

// TestRunResumesPendingAndStdinRest:resume 顺序契约=pending 行最先、
// stdin 残余其次、真实 stdin 最后;三段全部得到应答,零丢失。
func TestRunResumesPendingAndStdinRest(t *testing.T) {
	pending := `{"jsonrpc":"2.0","id":11,"method":"tools/list"}`
	rest := `{"jsonrpc":"2.0","id":12,"method":"tools/list"}` + "\n"
	live := `{"jsonrpc":"2.0","id":13,"method":"tools/list"}` + "\n"
	path, err := writeHandoffState(handoffState{PendingLine: pending, StdinRest: []byte(rest), At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHandoffState, path)
	server := NewServer(fakeMultiSyncer{})
	var out bytes.Buffer
	if err := server.Run(context.Background(), strings.NewReader(live), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("三段输入应得三条应答: %d\n%s", len(lines), out.String())
	}
	var ids []float64
	for _, line := range lines {
		var resp struct {
			ID float64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, resp.ID)
	}
	if ids[0] != 11 || ids[1] != 12 || ids[2] != 13 {
		t.Fatalf("应答顺序必须 pending→rest→live: %v", ids)
	}
	if server.handoffResumedAt.IsZero() {
		t.Fatal("resume 时刻必须记录(冷却窗锚点)")
	}
}

func TestIdentityChangedResponseDetector(t *testing.T) {
	id := json.RawMessage(`5`)
	hit := toolError(&id, "daemon /v1/retrieve: "+engine.DaemonIdentityChangedMarker+": wrapper revision a != daemon revision b; restart the MCP session or restore the expected daemon")
	if !identityChangedResponse(hit) {
		t.Fatal("identity 冲突响应必须被识别")
	}
	if identityChangedResponse(toolError(&id, "some other failure")) {
		t.Fatal("普通工具错误不得触发 handoff")
	}
	if identityChangedResponse(ok(&id, toolResult("fine", false))) {
		t.Fatal("成功响应不得触发 handoff")
	}
}

func TestTryHandoffGuards(t *testing.T) {
	server := NewServer(fakeMultiSyncer{})
	reader := bufio.NewReader(strings.NewReader(""))

	// 未启用:禁用态回落。
	if err := server.tryUpgradeHandoff("{}", reader); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("未启用必须回落: %v", err)
	}
	// 冷却窗:resume 后短窗内不再 exec。
	server.EnableUpgradeHandoff("/usr/bin/true")
	server.handoffResumedAt = time.Now()
	if err := server.tryUpgradeHandoff("{}", reader); err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("冷却窗内必须回落: %v", err)
	}
	// 自身路径失效:回落且不留状态残档。
	server.handoffResumedAt = time.Time{}
	server.EnableUpgradeHandoff("/nonexistent/openace-mcp-t2")
	if err := server.tryUpgradeHandoff("{}", reader); err == nil || !strings.Contains(err.Error(), "self path unavailable") {
		t.Fatalf("路径失效必须回落: %v", err)
	}
}

// TestBufferedRestExtraction:bufio 已缓冲字节必须完整转移。
func TestBufferedRestExtraction(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("first\nsecond\nthird\n"))
	line, _, err := readInputLine(reader, maxInputLineBytes)
	if err != nil || strings.TrimSpace(line) != "first" {
		t.Fatalf("预读失败: %q %v", line, err)
	}
	rest := bufferedRest(reader)
	if string(rest) != "second\nthird\n" {
		t.Fatalf("缓冲残余提取不完整: %q", rest)
	}
}
