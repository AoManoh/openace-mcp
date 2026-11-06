package mcp

import (
	"strings"
	"testing"
)

// TestPingReturnsEmptyResult：MCP 2025-11-25 规范要求收到 ping 后立即回一个与请求 id
// 对应的空 result。没有 ping 分支时返回 -32601（method not found），用 ping 检查连接的
// 客户端会把服务当成不可用。
func TestPingReturnsEmptyResult(t *testing.T) {
	out := runMCP(t, NewServer(fakeSyncer{}),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if !strings.Contains(out, `{"jsonrpc":"2.0","id":2,"result":{}}`) {
		t.Fatalf("ping 应返回与请求 id 对应的空 result: %s", out)
	}
}
