package managed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/daemon"
)

func TestDaemonAddrFromEnv(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_ADDR", "")
	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", "")
	if got := daemonAddrFromEnv(); got != daemon.DefaultAddr {
		t.Fatalf("default addr = %q, want %q", got, daemon.DefaultAddr)
	}

	t.Setenv("OPENACE_DAEMON_LISTEN_ADDR", "127.0.0.1:9000")
	if got := daemonAddrFromEnv(); got != "127.0.0.1:9000" {
		t.Fatalf("listen addr = %q", got)
	}

	t.Setenv("OPENACE_DAEMON_ADDR", "http://127.0.0.1:9999")
	if got := daemonAddrFromEnv(); got != "http://127.0.0.1:9999" {
		t.Fatalf("daemon addr should win, got %q", got)
	}
}

func TestListenAddr(t *testing.T) {
	for input, want := range map[string]string{
		"":                         daemon.DefaultAddr,
		"127.0.0.1:8765":           "127.0.0.1:8765",
		"http://127.0.0.1:8765/":   "127.0.0.1:8765",
		"https://localhost:9876/x": "localhost:9876",
	} {
		if got := listenAddr(input); got != want {
			t.Fatalf("listenAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestManagedDaemonAddrUsesPlainListenAddress(t *testing.T) {
	for input, want := range map[string]string{
		"https://127.0.0.1:8765": "127.0.0.1:8765",
		"http://localhost:9876/": "localhost:9876",
		"127.0.0.1:7654":         "127.0.0.1:7654",
	} {
		if got := managedDaemonAddr(input); got != want {
			t.Fatalf("managedDaemonAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConnectFallsBackToPlainHTTPForManagedDaemonURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/v1/daemon/status" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon","engine":"local-hybrid","capabilities":{"runtime_identity":true}}`))
	}))
	defer server.Close()

	// 本测试只验 URL scheme 回退:fake daemon 广播 local-hybrid 与零值
	// 配置指纹对齐 wrapper 默认档(Stage 7 后唯一引擎)。
	t.Setenv("OPENACE_ENGINE", "local-hybrid")
	t.Setenv("OPENACE_DAEMON_ADDR", "https://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "200ms")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConnectRejectsHealthyDaemonWithoutRuntimeIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	t.Setenv("OPENACE_DAEMON_ADDR", server.URL)
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "200ms")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Connect(ctx)
	if err == nil || !strings.Contains(err.Error(), "runtime identity") {
		t.Fatalf("expected runtime identity compatibility error, got %v", err)
	}
}

func TestCompatibleDaemonBuildRejectsMismatchedRevision(t *testing.T) {
	err := compatibleDaemonBuild(
		buildinfo.Info{Version: "v0.0.0-test", VCSRevision: "wrapper-revision"},
		buildinfo.Info{Version: "v0.0.0-test", VCSRevision: "daemon-revision"},
	)
	if err == nil || !strings.Contains(err.Error(), "wrapper revision") {
		t.Fatalf("expected revision mismatch error, got %v", err)
	}
}

func TestCompatibleDaemonBuildAllowsUnknownDevelopmentRevision(t *testing.T) {
	if err := compatibleDaemonBuild(buildinfo.Info{Version: "(devel)"}, buildinfo.Info{Version: "(devel)"}); err != nil {
		t.Fatalf("development builds without VCS metadata should not be rejected: %v", err)
	}
}

func TestStartupTimeout(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "")
	if got := startupTimeout(); got != defaultStartupTimeout {
		t.Fatalf("default timeout = %s", got)
	}
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "250ms")
	if got := startupTimeout(); got.String() != "250ms" {
		t.Fatalf("custom timeout = %s", got)
	}
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "invalid")
	if got := startupTimeout(); got != defaultStartupTimeout {
		t.Fatalf("invalid timeout should fallback, got %s", got)
	}
}

func TestAcquireStartupLockSerializesByAddress(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	release, err := acquireStartupLock(context.Background(), "127.0.0.1:9911", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := acquireStartupLock(ctx, "127.0.0.1:9911", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock should wait for context deadline, got %v", err)
	}

	release()
	releaseAgain, err := acquireStartupLock(context.Background(), "127.0.0.1:9911", time.Second)
	if err != nil {
		t.Fatalf("lock should be acquirable after release: %v", err)
	}
	releaseAgain()
}

// L14 收尸路径:死 wrapper 留下的过期启动锁必须被 rename 原子抢占并
// 清理,新 wrapper 得以继续;认领产物(.stale-*)不得残留。
func TestAcquireStartupLockReclaimsStaleLock(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	addr := "127.0.0.1:9912"
	dir := startupLockDir(addr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(dir, stale, stale); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := acquireStartupLock(ctx, addr, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("过期锁应被抢占收尸后正常获取: %v", err)
	}
	release()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".stale-") {
			t.Fatalf("stale 认领目录未清理: %s", entry.Name())
		}
	}
}

// 启动竞态:多 wrapper 同时抢同一地址的启动锁,任意时刻至多一个持有者,
// 且全部最终获得锁(无饿死/死锁)。
func TestAcquireStartupLockMutualExclusionUnderContention(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	addr := "127.0.0.1:9913"
	var holders atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireStartupLock(context.Background(), addr, 5*time.Second)
			if err != nil {
				errs <- err
				return
			}
			if n := holders.Add(1); n != 1 {
				errs <- fmt.Errorf("互斥破坏: %d 个并发持有者", n)
			} else {
				errs <- nil
			}
			time.Sleep(5 * time.Millisecond)
			holders.Add(-1)
			release()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// waitReady 就绪轮询:daemon 启动期短暂不健康后转好,应在多轮探测后
// 返回就绪。
func TestWaitReadyPollsUntilHealthy(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, `{"error":"starting"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"openace-daemon"}`))
	}))
	defer server.Close()

	if err := waitReady(context.Background(), daemon.NewClient(server.URL), 5*time.Second); err != nil {
		t.Fatalf("转好后应就绪: %v", err)
	}
	if calls.Load() < 3 {
		t.Fatalf("应经历多轮探测,实际 %d 次", calls.Load())
	}
}

// waitReady 超时:错误须携带就绪语义与最后一次失败原因(可诊断)。
func TestWaitReadyTimesOutWithLastError(t *testing.T) {
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bind failed"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	// 250ms 刻意错开 100ms 轮询节拍:最后一次完成的探测(t=200ms)是
	// 503,不会与截止时刻并发开启新探测而把 lastErr 覆盖为 ctx 超时。
	err := waitReady(context.Background(), daemon.NewClient(server.URL), 250*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("超时错误应含就绪语义与最后失败: %v", err)
	}
}

// daemonLogFile 收尸配套:stderr 落盘文件应创建在 openace-mcp 缓存目录
// 下(waitReady 失败时经 withDaemonLog 提取尾巴)。
func TestDaemonLogFileCreatesUnderCacheDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file, path := daemonLogFile()
	if file == nil || path == "" {
		t.Fatalf("log 文件应创建: path=%q", path)
	}
	defer file.Close()
	if !strings.Contains(path, filepath.Join("openace-mcp", "daemon-logs")) {
		t.Fatalf("log 应落在 openace-mcp 缓存目录: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// 恢复回调的锁竞争分支:他 wrapper 正持锁重拉时,本 wrapper 的回调不得
// 抢启动(防双启动),而是回退复用探测并透出其结论——绝不静默。
func TestRecoverHookProbesWhenStartupLockContended(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	t.Setenv("OPENACE_DAEMON_START_TIMEOUT", "150ms")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 端口空置:连接必然被拒,触发恢复回调。

	release, err := acquireStartupLock(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	client := daemon.NewClient(addr)
	attachRecoverHook(client, addr, "local-hybrid", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = client.Health(ctx)
	if err == nil {
		t.Fatal("daemon 不在且锁被他人持有时必须失败")
	}
	if !strings.Contains(err.Error(), "runtime identity") {
		t.Fatalf("锁竞争分支应回退复用探测并透出其结论: %v", err)
	}
}

func TestWithDaemonLogAppendsCapturedStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("real validation error\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := withDaemonLog(errors.New("not ready"), path)
	if !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "real validation error") {
		t.Fatalf("error should include readiness and stderr details: %v", err)
	}
}

func TestUpsertEnv(t *testing.T) {
	got := upsertEnv([]string{"A=1", "OPENACE_DAEMON_LISTEN_ADDR=old"}, "OPENACE_DAEMON_LISTEN_ADDR", "new")
	if got[1] != "OPENACE_DAEMON_LISTEN_ADDR=new" {
		t.Fatalf("existing env not replaced: %v", got)
	}
	got = upsertEnv(nil, "OPENACE_TEST_KEY", "value")
	if len(got) != 1 || got[0] != "OPENACE_TEST_KEY=value" {
		t.Fatalf("new env not appended: %v", got)
	}
}
