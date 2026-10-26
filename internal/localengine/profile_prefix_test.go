package localengine

import "github.com/AoManoh/openace-mcp/internal/chunk"

// currentProfilePrefix 是当前 chunk profile 的子树名前缀（如 default-v9）。
// 模拟旧版本子树的测试把它替换成更早的版本号，不随 profile 升版而失效。
func currentProfilePrefix() string {
	return chunk.DefaultProfile().ID + "-v" + chunk.DefaultProfile().Version
}
