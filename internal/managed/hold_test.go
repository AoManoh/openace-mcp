package managed

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/daemon"
)

// freeAddr 返回一个当前没有监听的本机地址：连接必然被拒。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// stubDaemonStart 替换 startDaemonProcess，记录调用次数并返回错误，不拉起
// 真实进程。
func stubDaemonStart(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	orig := startDaemonProcess
	startDaemonProcess = func(addr string) (string, error) {
		calls.Add(1)
		return "", errStubStart
	}
	t.Cleanup(func() { startDaemonProcess = orig })
	return &calls
}

var errStubStart = &stubError{"stub: daemon start is disabled in this test"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }

func holdTestEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "150ms")
	addr := freeAddr(t)
	t.Setenv("OPENACE_DAEMON_ADDR", addr)
	return addr
}

// TestConnectDoesNotStartDaemonWhileHeld：停用标记存在、daemon 不在运行时，
// wrapper 启动连接不拉起 daemon，错误写明停用原因与解除命令。
func TestConnectDoesNotStartDaemonWhileHeld(t *testing.T) {
	addr := holdTestEnv(t)
	started := stubDaemonStart(t)
	if _, err := writeHold(managedDaemonAddr(addr), HoldInfo{At: time.Now().UTC(), Reason: "停止计费的后台嵌入", PID: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "openace-mcp daemon resume") || !strings.Contains(err.Error(), "停止计费的后台嵌入") {
		t.Fatalf("应返回写明停用原因与解除命令的错误: %v", err)
	}
	if n := started.Load(); n != 0 {
		t.Fatalf("停用标记存在时不得拉起 daemon，实际拉起 %d 次", n)
	}
}

// TestConnectStartsDaemonWithoutHold：没有停用标记时照旧走拉起路径（对照）。
func TestConnectStartsDaemonWithoutHold(t *testing.T) {
	holdTestEnv(t)
	started := stubDaemonStart(t)
	if _, err := Connect(context.Background()); err == nil || !strings.Contains(err.Error(), errStubStart.msg) {
		t.Fatalf("没有标记时应走拉起路径并透出拉起错误: %v", err)
	}
	if started.Load() != 1 {
		t.Fatalf("没有标记时应拉起一次，实际 %d 次", started.Load())
	}
}

// TestRecoverHookDoesNotStartDaemonWhileHeld：会话中途 daemon 被停掉、连接被拒
// 时，自动恢复不再拉起 daemon。
func TestRecoverHookDoesNotStartDaemonWhileHeld(t *testing.T) {
	addr := holdTestEnv(t)
	started := stubDaemonStart(t)
	if _, err := writeHold(addr, HoldInfo{At: time.Now().UTC(), PID: 1}); err != nil {
		t.Fatal(err)
	}
	client := daemon.NewClient(addr)
	attachRecoverHook(client, addr, "local-hybrid", profileExpectation{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := client.Health(ctx)
	if err == nil || !strings.Contains(err.Error(), "openace-mcp daemon stop") {
		t.Fatalf("自动恢复应返回停用错误: %v", err)
	}
	if n := started.Load(); n != 0 {
		t.Fatalf("停用标记存在时自动恢复不得拉起 daemon，实际拉起 %d 次", n)
	}
}

// TestUnreadableHoldMarkerRefusesStart：标记存在但读不出时同样不拉起，避免用户
// 停用之后因为文件损坏又被拉起。
func TestUnreadableHoldMarkerRefusesStart(t *testing.T) {
	addr := holdTestEnv(t)
	started := stubDaemonStart(t)
	path := holdPath(managedDaemonAddr(addr))
	if err := os.MkdirAll(strings.TrimSuffix(path, "/"+lastPathElem(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("读不出的标记应报错: %v", err)
	}
	if started.Load() != 0 {
		t.Fatal("读不出的标记也不得拉起 daemon")
	}
}

func lastPathElem(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

// TestStopAndResumeWithoutRunningDaemon：daemon 不在运行时 stop 只写标记，
// resume 删除标记；再次 resume 报告标记不存在。
func TestStopAndResumeWithoutRunningDaemon(t *testing.T) {
	addr := holdTestEnv(t)
	result, err := StopDaemon(context.Background(), "测试")
	if err != nil || result.StoppedPID != 0 || result.HoldPath == "" {
		t.Fatalf("没有 daemon 时应只写标记: %+v err=%v", result, err)
	}
	info, held, err := ReadHold(managedDaemonAddr(addr))
	if err != nil || !held || info.Reason != "测试" || info.PID != os.Getpid() {
		t.Fatalf("标记内容不对: %+v held=%v err=%v", info, held, err)
	}
	path, existed, err := ResumeDaemon()
	if err != nil || !existed || path != result.HoldPath {
		t.Fatalf("resume 应删除标记: path=%s existed=%v err=%v", path, existed, err)
	}
	if _, held, _ := ReadHold(managedDaemonAddr(addr)); held {
		t.Fatal("resume 后标记仍在")
	}
	if _, existed, err := ResumeDaemon(); err != nil || existed {
		t.Fatalf("再次 resume 应报告标记不存在: existed=%v err=%v", existed, err)
	}
}

// TestStopDaemonSignalsRunningDaemon：daemon 在运行时，stop 先写标记，再终止
// status 报告的 pid 并等到它不再健康。
func TestStopDaemonSignalsRunningDaemon(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	var terminated atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if terminated.Load() {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "openace-daemon"})
	})
	mux.HandleFunc("/v1/daemon/status", func(w http.ResponseWriter, r *http.Request) {
		status := daemonStatusFixture("rev", "2020-01-01T00:00:00Z", 424242)
		status.Status = "ok"
		_ = json.NewEncoder(w).Encode(status)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	addr := server.Listener.Addr().String()
	t.Setenv("OPENACE_DAEMON_ADDR", addr)

	var killed atomic.Int64
	var heldAtKill atomic.Bool
	orig := terminateProcess
	terminateProcess = func(pid int) error {
		_, held, _ := ReadHold(managedDaemonAddr(addr))
		heldAtKill.Store(held)
		killed.Store(int64(pid))
		terminated.Store(true)
		return nil
	}
	t.Cleanup(func() { terminateProcess = orig })

	result, err := StopDaemon(context.Background(), "")
	if err != nil || result.StoppedPID != 424242 || killed.Load() != 424242 {
		t.Fatalf("应终止 status 报告的 pid: %+v killed=%d err=%v", result, killed.Load(), err)
	}
	if !heldAtKill.Load() {
		t.Fatal("停用标记必须在终止 daemon 之前写好，否则其他 wrapper 可能在空档里重新拉起")
	}
}

// TestDaemonEnvRecordsLauncher：被拉起的 daemon 从环境里拿到拉起者的进程号与
// 工作目录。
func TestDaemonEnvRecordsLauncher(t *testing.T) {
	env := daemonEnv("127.0.0.1:18765")
	var raw string
	for _, kv := range env {
		if strings.HasPrefix(kv, daemon.EnvLaunchedBy+"=") {
			raw = strings.TrimPrefix(kv, daemon.EnvLaunchedBy+"=")
		}
	}
	var info daemon.LaunchInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("拉起者信息应为 JSON: %q %v", raw, err)
	}
	cwd, _ := os.Getwd()
	if info.PID != os.Getpid() || info.Cwd != cwd || info.At.IsZero() {
		t.Fatalf("拉起者信息不对: %+v", info)
	}
}
