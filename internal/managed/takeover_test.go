package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// moduleBuildStatus 模拟 go install 模块构建的 daemon:模块 zip 无 .git,
// 二进制没有任何 vcs.* 构建戳,身份只有伪版本(外部 @main 消费通道的
// 常态,2026-08-31 外部反馈实录形态)。
func moduleBuildStatus(version string, pid int) daemon.Status {
	var status daemon.Status
	status.Service = "openace-daemon"
	status.PID = pid
	status.Build = buildinfo.Info{Version: version}
	return status
}

func TestTakeoverTargetOrdersModuleBuildsByPseudoVersion(t *testing.T) {
	// T5 立项动机场景="使用 @main 追新的用户每次升级必撞",但该通道的
	// 二进制无 VCSTime;伪版本时间戳(vX.Y.Z-yyyymmddhhmmss-hash)与
	// vcs.time 同源同刻度(提交时间 UTC),必须可作排序回退。
	newerModule := buildinfo.Info{Version: "v0.0.0-20260826103859-6b8b59b80877"}
	olderModule := buildinfo.Info{Version: "v0.0.0-20260820075351-0b2f077945a6"}

	if pid, err := takeoverTarget(newerModule, moduleBuildStatus(olderModule.Version, 4242)); err != nil || pid != 4242 {
		t.Fatalf("双伪版本可排序且 wrapper 更新,应放行接管: pid=%d err=%v", pid, err)
	}
	if _, err := takeoverTarget(olderModule, moduleBuildStatus(newerModule.Version, 4242)); err == nil || !strings.Contains(err.Error(), "refusing takeover") {
		t.Fatalf("wrapper 伪版本更旧必须拒绝: %v", err)
	}
	// 混合通道:一侧源码构建(VCS 戳)、一侧模块构建(伪版本),同刻度可比。
	stamped := wrapperInfo("new", "2026-08-26T10:38:59Z")
	if pid, err := takeoverTarget(stamped, moduleBuildStatus(olderModule.Version, 4242)); err != nil || pid != 4242 {
		t.Fatalf("VCS 戳×伪版本混合应可排序: pid=%d err=%v", pid, err)
	}
	if _, err := takeoverTarget(olderModule, daemonStatusFixture("newer", "2026-08-26T10:38:59Z", 4242)); err == nil || !strings.Contains(err.Error(), "refusing takeover") {
		t.Fatalf("伪版本 wrapper 旧于 VCS 戳 daemon 必须拒绝: %v", err)
	}
	// 源码构建带未提交变更的 +dirty 后缀不阻碍解析。
	dirty := buildinfo.Info{Version: "v0.0.0-20260826103859-6b8b59b80877+dirty"}
	if pid, err := takeoverTarget(dirty, moduleBuildStatus(olderModule.Version, 4242)); err != nil || pid != 4242 {
		t.Fatalf("+dirty 伪版本应可排序: pid=%d err=%v", pid, err)
	}
	// tag 之后提交的伪版本形态(vX.Y.Z-0.yyyymmddhhmmss-hash,点前缀)。
	tagBased := buildinfo.Info{Version: "v0.2.0-0.20260826103859-6b8b59b80877"}
	if pid, err := takeoverTarget(tagBased, moduleBuildStatus(olderModule.Version, 4242)); err != nil || pid != 4242 {
		t.Fatalf("tag 基底伪版本应可排序: pid=%d err=%v", pid, err)
	}
	// tag 发布版(无时间戳)不可排序:维持"无法排序就不动手"。
	if _, err := takeoverTarget(buildinfo.Info{Version: "v0.1.0"}, moduleBuildStatus(olderModule.Version, 4242)); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("tag 发布版无时间戳必须拒绝: %v", err)
	}
}

func TestTakeoverTargetOrdersReleaseTagsByModuleVersion(t *testing.T) {
	// 发布 tag 的模块构建(go install @v0.2.0)版本串没有时间戳,只能按 Go
	// 模块版本序比较:tag 之间按 vX.Y.Z 数值;tag 之后的提交在模块代理中
	// 得到 vX.Y.(Z+1)-0.<时间戳>-<hash> 形态的伪版本,排在 vX.Y.Z 之后、
	// vX.Y.(Z+1) 之前。以 v0.0.0 为基底的伪版本(该提交可达范围内没有任何
	// tag)与 tag 之间没有可靠顺序,必须拒绝。
	tag := func(v string) buildinfo.Info { return buildinfo.Info{Version: v} }
	pid := 4242

	if got, err := takeoverTarget(tag("v0.2.0"), moduleBuildStatus("v0.1.0", pid)); err != nil || got != pid {
		t.Fatalf("tag 更新的 wrapper 应放行: pid=%d err=%v", got, err)
	}
	if _, err := takeoverTarget(tag("v0.1.0"), moduleBuildStatus("v0.2.0", pid)); err == nil || !strings.Contains(err.Error(), "refusing takeover") {
		t.Fatalf("tag 更旧的 wrapper 必须拒绝: %v", err)
	}
	if _, err := takeoverTarget(tag("v0.2.0"), moduleBuildStatus("v0.2.0", pid)); err != nil {
		t.Fatalf("同 tag 不算更旧,放行由构建一致性检查决定: %v", err)
	}
	// tag 之后的伪版本(基底 v0.2.0)相对 tag v0.2.0 更新;相对其后续正式
	// 发布 v0.2.1 更旧。
	after := tag("v0.2.1-0.20260901120000-0123456789ab")
	if got, err := takeoverTarget(after, moduleBuildStatus("v0.2.0", pid)); err != nil || got != pid {
		t.Fatalf("tag 之后的伪版本相对该 tag 应放行: pid=%d err=%v", got, err)
	}
	if got, err := takeoverTarget(tag("v0.2.1"), moduleBuildStatus(after.Version, pid)); err != nil || got != pid {
		t.Fatalf("正式发布相对其前置伪版本应放行: pid=%d err=%v", got, err)
	}
	if _, err := takeoverTarget(after, moduleBuildStatus("v0.2.1", pid)); err == nil || !strings.Contains(err.Error(), "refusing takeover") {
		t.Fatalf("前置伪版本相对正式发布必须拒绝: %v", err)
	}
	if _, err := takeoverTarget(after, moduleBuildStatus("v0.3.0", pid)); err == nil || !strings.Contains(err.Error(), "refusing takeover") {
		t.Fatalf("更高 minor 的 tag 更新,必须拒绝: %v", err)
	}
	// 源码构建在 tag 提交上带未提交改动:v0.2.0+dirty,剥离后按 tag 比较。
	if got, err := takeoverTarget(tag("v0.2.0+dirty"), moduleBuildStatus("v0.1.0", pid)); err != nil || got != pid {
		t.Fatalf("+dirty tag 版本应可比较: pid=%d err=%v", got, err)
	}
	// 无 tag 基底的伪版本与 tag 之间不可排序(两个方向都拒绝)。
	orphan := "v0.0.0-20260826103859-6b8b59b80877"
	if _, err := takeoverTarget(tag(orphan), moduleBuildStatus("v0.1.0", pid)); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("v0.0.0 基底伪版本 vs tag 必须拒绝: %v", err)
	}
	if _, err := takeoverTarget(tag("v0.2.0"), moduleBuildStatus(orphan, pid)); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("tag vs v0.0.0 基底伪版本必须拒绝: %v", err)
	}
	// 非伪版本的预发布 tag(rc 等)不在接受的形态内,拒绝。
	if _, err := takeoverTarget(tag("v0.2.0-rc.1"), moduleBuildStatus("v0.1.0", pid)); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("rc 预发布 tag 必须拒绝: %v", err)
	}
	// tag 构建与只有 vcs.time、版本串为 (devel) 的源码构建之间没有共同
	// 比较口径,拒绝。
	devel := daemonStatusFixture("dev", "2026-08-26T10:38:59Z", pid)
	devel.Build.Version = "(devel)"
	if _, err := takeoverTarget(tag("v0.2.0"), devel); err == nil || !strings.Contains(err.Error(), "cannot order builds") {
		t.Fatalf("tag vs 仅 vcs.time 的 devel 构建必须拒绝: %v", err)
	}
}

func TestTakeoverRefusesNonLoopbackEndpoint(t *testing.T) {
	// T5 边界 1 代码化:接管以 SIGTERM 本地 pid 实施,status 报告的 pid
	// 只在 daemon 与 wrapper 同机时有意义;endpoint 指向他机(auto 模式
	// 误配远程地址)时,同号本地进程与目标无关,必须在任何网络探测与
	// 终止动作之前拒绝。192.0.2.1=TEST-NET-1 不可路由:守卫缺失时本测试
	// 会走网络探测超时而非快速拒绝。
	t.Setenv("OPENACE_DAEMON_TOKEN", "off")
	client := daemon.NewClient("http://192.0.2.1:8765")
	orig := terminateProcess
	terminateProcess = func(pid int) error {
		t.Fatal("非 loopback endpoint 不得触发本地终止")
		return nil
	}
	t.Cleanup(func() { terminateProcess = orig })
	err := takeoverOutdatedDaemon(context.Background(), client, fmt.Errorf("%w: test", errDaemonBuildMismatch))
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("非 loopback endpoint 必须拒绝接管: %v", err)
	}
}
