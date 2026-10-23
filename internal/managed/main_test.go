package managed

import (
	"os"
	"testing"
)

// TestMain 把缓存目录指向临时位置。本包的测试对 httptest 服务地址创建
// daemon 客户端，客户端按地址读取或创建 token 文件（daemon.LoadOrCreateToken），
// 不隔离时每次运行都在真实用户缓存目录留下 daemon-token-* 文件。逐用例
// t.Setenv 的 XDG_CACHE_HOME 优先于这里的进程级设置；internal/daemon 包用
// 同样的做法。
func TestMain(m *testing.M) {
	cleanup := func() {}
	if os.Getenv("XDG_CACHE_HOME") == "" {
		if dir, err := os.MkdirTemp("", "openace-managed-test-cache-*"); err == nil {
			os.Setenv("XDG_CACHE_HOME", dir)
			cleanup = func() { os.RemoveAll(dir) }
		}
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
