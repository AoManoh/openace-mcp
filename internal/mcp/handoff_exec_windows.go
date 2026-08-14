//go:build !unix

package mcp

import "errors"

// execSelf 在非 Unix 平台不可用(无 exec(2) 语义):保持 known-issue #13
// 的既有硬错文案(可行动:重启 MCP 会话)。
func execSelf(string, string) error {
	return errors.New("in-place exec unsupported on this platform; restart the MCP session")
}
