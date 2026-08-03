package daemon

import (
	"os"
	"testing"
)

// TestMain:daemon 包测试默认以 off 档运行(M5 引入默认 token 后,存量
// 测试的无凭据请求语义保持不变);token 行为本身由 token_test.go 以
// t.Setenv 逐用例覆盖(t.Setenv 优先于此处的进程级默认)。同时把缓存
// 目录指向临时位置,防默认档在真实用户缓存生成 token 文件。
func TestMain(m *testing.M) {
	if os.Getenv("OPENACE_DAEMON_TOKEN") == "" {
		os.Setenv("OPENACE_DAEMON_TOKEN", "off")
	}
	if os.Getenv("XDG_CACHE_HOME") == "" {
		if dir, err := os.MkdirTemp("", "openace-daemon-test-cache-*"); err == nil {
			os.Setenv("XDG_CACHE_HOME", dir)
			defer os.RemoveAll(dir)
		}
	}
	os.Exit(m.Run())
}
