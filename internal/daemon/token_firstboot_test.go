package daemon

import (
	"os"
	"testing"
)

// TestClientTokenFirstBootConvergence 复现全新机器首启竞态(review -15
// 衍生,灰度前置缺陷):wrapper 先于 daemon 构造 client 时 token 文件
// 尚不存在,历史实现缓存空 token,daemon 随后自建随机 token,健康探测
// 永远 401。修复后:客户端在文件缺失时走同一 load-or-create 路径,
// 双方无论谁先启动都收敛到同一 token。
func TestClientTokenFirstBootConvergence(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("OPENACE_DAEMON_TOKEN", "")
	addr := "127.0.0.1:18776"

	if _, err := os.Stat(TokenFilePath(addr)); !os.IsNotExist(err) {
		t.Fatalf("前置:token 文件不应存在: %v", err)
	}
	// wrapper 侧先构造(模拟 daemon 尚未启动)。
	clientSide := clientToken(addr)
	// daemon 侧随后解析(load-or-create)。
	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", addr)
	serverSide, err := resolveAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if serverSide == "" {
		t.Fatal("默认档服务端 token 不应为空")
	}
	if clientSide != serverSide {
		t.Fatalf("首启 token 不收敛:client=%q server=%q(401 死循环成因)", clientSide, serverSide)
	}
	// 反向顺序(daemon 先启)同样收敛。
	addr2 := "127.0.0.1:18777"
	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", addr2)
	serverFirst, err := resolveAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if got := clientToken(addr2); got != serverFirst {
		t.Fatalf("daemon 先启不收敛:client=%q server=%q", got, serverFirst)
	}
	// 文件权限维持 0600。
	info, err := os.Stat(TokenFilePath(addr))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token 文件权限: %v", info.Mode().Perm())
	}
}
