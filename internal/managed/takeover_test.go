package managed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/daemon"
)

// T5 单元面(docs/tasks/T5-connect-time-daemon-takeover.md)。红态证据=
// 外部灰度 2026-08-14 实录 + 本日 8768 隔离复现(revision 不匹配→降级
// 会话,零补救)。真实 SIGTERM 链由隔离端口 E2E 验收。

func wrapperInfo(revision, at string) buildinfo.Info {
	return buildinfo.Info{VCSRevision: revision, VCSTime: at, Version: "v0.0.0-test"}
}

func daemonStatusFixture(revision, at string, pid int) daemon.Status {
	var status daemon.Status
	status.Service = "openace-daemon"
	status.PID = pid
	status.Build = buildinfo.Info{VCSRevision: revision, VCSTime: at, Version: "v0.0.0-test"}
	return status
}

func TestCompatibleDaemonBuildMismatchIsTyped(t *testing.T) {
	err := compatibleDaemonBuild(wrapperInfo("aaa", "2026-08-14T10:00:00Z"), buildinfo.Info{VCSRevision: "bbb", VCSTime: "2026-08-13T10:00:00Z"})
	if err == nil || !errors.Is(err, errDaemonBuildMismatch) {
		t.Fatalf("build 不匹配必须携带类型标记: %v", err)
	}
	if err := compatibleDaemonBuild(wrapperInfo("aaa", ""), buildinfo.Info{VCSRevision: "aaa"}); err != nil {
		t.Fatalf("同 revision 不得报错: %v", err)
	}
}

func TestTakeoverTargetGates(t *testing.T) {
	wrapper := wrapperInfo("new", "2026-08-14T10:00:00Z")

	if pid, err := takeoverTarget(wrapper, daemonStatusFixture("old", "2026-08-13T09:00:00Z", 4242)); err != nil || pid != 4242 {
		t.Fatalf("wrapper 新于 daemon 应放行接管: pid=%d err=%v", pid, err)
	}
	if _, err := takeoverTarget(wrapper, daemonStatusFixture("newer", "2026-08-15T09:00:00Z", 4242)); err == nil || !strings.Contains(err.Error(), "refusing takeover") {
		t.Fatalf("daemon 更新时必须拒绝接管: %v", err)
	}
	if _, err := takeoverTarget(wrapperInfo("new", ""), daemonStatusFixture("old", "2026-08-13T09:00:00Z", 4242)); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("wrapper 缺 VCSTime 必须拒绝: %v", err)
	}
	if _, err := takeoverTarget(wrapper, daemonStatusFixture("old", "", 4242)); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("daemon 缺 VCSTime 必须拒绝: %v", err)
	}
	if _, err := takeoverTarget(wrapper, daemonStatusFixture("old", "2026-08-13T09:00:00Z", 0)); err == nil || !strings.Contains(err.Error(), "pid unavailable") {
		t.Fatalf("无 pid 必须拒绝: %v", err)
	}
	other := daemonStatusFixture("old", "2026-08-13T09:00:00Z", 4242)
	other.Service = "something-else"
	if _, err := takeoverTarget(wrapper, other); err == nil || !strings.Contains(err.Error(), "not an openace daemon") {
		t.Fatalf("非本产品服务必须拒绝: %v", err)
	}
}

// fakeOutdatedDaemon 模拟健康但 build 过期的 daemon:收到"终止"后翻转
// 为不健康(不发真实信号)。
func fakeOutdatedDaemon(t *testing.T, buildAt string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var terminated atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if terminated.Load() {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/daemon/status", func(w http.ResponseWriter, r *http.Request) {
		status := daemonStatusFixture("old-rev", buildAt, 987654)
		status.Status = "ok"
		_ = json.NewEncoder(w).Encode(status)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &terminated
}

func TestTakeoverOutdatedDaemonHappyPath(t *testing.T) {
	if buildinfo.Current().VCSTime == "" {
		t.Skip("test binary lacks vcs stamping; ordering gate untestable here")
	}
	server, terminated := fakeOutdatedDaemon(t, "2020-01-01T00:00:00Z")
	client := daemon.NewClient(server.URL)

	var killedPID atomic.Int64
	orig := terminateProcess
	terminateProcess = func(pid int) error {
		killedPID.Store(int64(pid))
		terminated.Store(true)
		return nil
	}
	t.Cleanup(func() { terminateProcess = orig })

	mismatch := compatibleDaemonBuild(buildinfo.Current(), buildinfo.Info{VCSRevision: "old-rev", VCSTime: "2020-01-01T00:00:00Z"})
	if err := takeoverOutdatedDaemon(context.Background(), client, mismatch); err != nil {
		t.Fatalf("接管应成功: %v", err)
	}
	if killedPID.Load() != 987654 {
		t.Fatalf("应终止 status 报告的 pid: %d", killedPID.Load())
	}
}

func TestTakeoverSkipsNonBuildMismatch(t *testing.T) {
	server, _ := fakeOutdatedDaemon(t, "2020-01-01T00:00:00Z")
	client := daemon.NewClient(server.URL)
	orig := terminateProcess
	terminateProcess = func(pid int) error {
		t.Fatal("profile 不匹配不得触发接管")
		return nil
	}
	t.Cleanup(func() { terminateProcess = orig })
	if err := takeoverOutdatedDaemon(context.Background(), client, errors.New("wrapper engine profile a != daemon engine profile b")); err == nil {
		t.Fatal("非 build 不匹配必须拒绝接管")
	}
}
