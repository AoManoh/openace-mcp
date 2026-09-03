package managed

// T5(docs/tasks/T5-connect-time-daemon-takeover.md):connect 时旧 daemon
// 自动接管。场景=wrapper 升级后,健康但 build 过期的 daemon 占住 managed
// 地址,此前只能报错并等待人工清场(外部灰度 2026-08-14 实录)。接管仅在
// 全部安全门通过时发生:managed 生命周期 + 目标地址为本机回环 + build 级
// 不匹配 + 双方构建可排序且 wrapper 不旧于 daemon + 目标确为 openace-daemon
// 且 pid 可用。排序依据按可得性依次为 vcs.time 构建戳、伪版本时间戳、
// Go 模块版本序(发布 tag);任何一对无法可靠比较即拒绝接管。
// 优雅退出(SIGTERM)+ 有界等待;超时不强杀(SIGKILL 可能打断 manifest
// 原子发布窗口),回落原错误语义。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AoManoh/openace-mcp/internal/buildinfo"
	"github.com/AoManoh/openace-mcp/internal/daemon"
)

// takeoverExitWait 是接管时等待旧 daemon 优雅退出的上界。daemon 关停
// 会取消在飞 reconcile 与构建(付费嵌入有 journal 断点,零向量丢失),
// 常规窗口远小于此值。
const takeoverExitWait = 5 * time.Second

// terminateProcess 可注入(单测不发真实信号);默认实现按平台分文件。
var terminateProcess = defaultTerminateProcess

// takeoverOutdatedDaemon 判定并执行接管。返回 nil 表示旧 daemon 已退出、
// 调用方可继续走既有 startDaemon 路径;任何门不通过或执行失败返回原因,
// 调用方保持原错误。
func takeoverOutdatedDaemon(ctx context.Context, client *daemon.Client, mismatch error) error {
	if !errors.Is(mismatch, errDaemonBuildMismatch) {
		return fmt.Errorf("not a build mismatch")
	}
	// T5 边界 1(接管域=本机):SIGTERM 作用于本地 pid,status 报告的
	// pid 只在 daemon 与 wrapper 同机时有意义;endpoint 指向他机时同号
	// 本地进程与目标无关,任何探测/终止动作之前先拒绝。
	if endpoint := client.Endpoint(); !loopbackEndpoint(endpoint) {
		return fmt.Errorf("daemon endpoint %s is not loopback; refusing takeover of a possibly remote daemon", endpoint)
	}
	statusCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	status, err := client.DaemonStatus(statusCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("status refetch failed: %w", err)
	}
	pid, err := takeoverTarget(buildinfo.Current(), status)
	if err != nil {
		return err
	}
	if err := terminateProcess(pid); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(takeoverExitWait)
	for time.Now().Before(deadline) {
		if !healthy(ctx, client) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon pid %d still healthy after %s", pid, takeoverExitWait)
}

// takeoverTarget 校验接管安全门并返回目标 pid:服务身份、pid 有效、
// 构建时间双方可得且 wrapper 不旧于 daemon(旧 binary 无权拆新服务;
// 无法排序=不动手)。
func takeoverTarget(wrapper buildinfo.Info, status daemon.Status) (int, error) {
	if status.Service != "openace-daemon" {
		return 0, fmt.Errorf("target is not an openace daemon")
	}
	if status.PID <= 0 {
		return 0, fmt.Errorf("daemon pid unavailable")
	}
	wrapperAt, wrapperErr := buildOrderTime(wrapper)
	daemonAt, daemonErr := buildOrderTime(status.Build)
	if wrapperErr == nil && daemonErr == nil {
		if wrapperAt.Before(daemonAt) {
			return 0, fmt.Errorf("daemon build (%s) is newer than wrapper (%s); refusing takeover", daemonAt.Format(time.RFC3339), wrapperAt.Format(time.RFC3339))
		}
		return status.PID, nil
	}
	// 至少一方没有时间戳(发布 tag 的模块构建只有 vX.Y.Z),改按 Go 模块
	// 版本序比较;比较不成立时报出两侧各自缺什么。
	older, err := wrapperOlderByModuleVersion(wrapper.Version, status.Build.Version)
	if err != nil {
		return 0, fmt.Errorf("%v (wrapper: %s; daemon: %s); cannot order builds", err, describeBuildOrder(wrapper, wrapperErr), describeBuildOrder(status.Build, daemonErr))
	}
	if older {
		return 0, fmt.Errorf("daemon build %s is newer than wrapper %s by module version; refusing takeover", status.Build.Version, wrapper.Version)
	}
	return status.PID, nil
}

// wrapperOlderThanDaemon 判断 wrapper 构建是否旧于 daemon 构建,排序来源与
// takeoverTarget 一致(先 vcs/伪版本时间戳,再 Go 模块版本序)。known=false
// 表示两种来源都无法比较(如 v0.0.0 基底伪版本对正式 tag)。
func wrapperOlderThanDaemon(wrapper buildinfo.Info, daemonBuild buildinfo.Info) (older bool, known bool) {
	wrapperAt, wrapperErr := buildOrderTime(wrapper)
	daemonAt, daemonErr := buildOrderTime(daemonBuild)
	if wrapperErr == nil && daemonErr == nil {
		return wrapperAt.Before(daemonAt), true
	}
	older, err := wrapperOlderByModuleVersion(wrapper.Version, daemonBuild.Version)
	if err != nil {
		return false, false
	}
	return older, true
}

// buildMismatchGuidance 是 wrapper/daemon 构建不一致且自动接管未发生时给
// 用户的手工出路。外部反馈 2026-09-03(P16):旧 wrapper 连上刚升级的 daemon
// 时,此前一律写"kill <daemon pid>",照做会打掉新 daemon、拖垮其他会话。
// 指引按方向给:wrapper 更旧→重启会话或重装 wrapper,明确禁止停 daemon;
// wrapper 更新→保留 kill 指引(接管不可用时的确定出路);无法排序→先重启
// 会话(本机可能已装新 wrapper),同一错误在新会话复现才把 daemon 当旧方。
func buildMismatchGuidance(wrapper buildinfo.Info, daemonBuild buildinfo.Info, pid int) string {
	older, known := wrapperOlderThanDaemon(wrapper, daemonBuild)
	switch {
	case known && older:
		return fmt.Sprintf("this MCP wrapper (%s) is older than the daemon (%s): restart the MCP session or reinstall openace-mcp so a current wrapper starts; do not stop the daemon, other sessions may depend on it", describeVersion(wrapper), describeVersion(daemonBuild))
	case known:
		return fmt.Sprintf("fix: stop the outdated daemon (kill %d) and retry", pid)
	default:
		return fmt.Sprintf("fix: restart the MCP session first (a newer wrapper may already be installed; wrapper %s, daemon %s cannot be ordered); if the error persists in a fresh session the daemon is the outdated side: stop it (kill %d) and retry", describeVersion(wrapper), describeVersion(daemonBuild), pid)
	}
}

// describeVersion 给出一侧构建的可读标识(版本串,缺失时用 vcs 修订)。
func describeVersion(info buildinfo.Info) string {
	if info.Version != "" && info.Version != "(devel)" {
		return info.Version
	}
	if info.VCSRevision != "" {
		return info.VCSRevision
	}
	return "unknown build"
}

// describeBuildOrder 给出一侧构建在排序上的可用信息,用于拒绝时的说明。
func describeBuildOrder(info buildinfo.Info, timeErr error) string {
	if timeErr == nil {
		return "timestamp available, version " + strconv.Quote(info.Version)
	}
	return "no timestamp, version " + strconv.Quote(info.Version)
}

// moduleVersion 是 Go 模块版本串中参与排序的部分。
type moduleVersion struct {
	major, minor, patch int
	// pseudo 表示伪版本(vX.Y.Z-0.<时间戳>-<hash> 及 pre-release 基底
	// 变体):它排在 vX.Y.Z 之前、vX.Y.(Z-1) 之后。
	pseudo bool
}

var releaseVersionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-(.+))?$`)

// parseModuleVersion 只接受两种可排序形态:正式发布 tag vX.Y.Z,以及带
// 时间戳的伪版本。rc/beta 等非伪版本的预发布串、(devel)、空串都返回
// false,调用方按不可排序处理。+dirty 等 build metadata 先剥离。
func parseModuleVersion(version string) (moduleVersion, bool) {
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	match := releaseVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return moduleVersion{}, false
	}
	var parsed moduleVersion
	for i, field := range []*int{&parsed.major, &parsed.minor, &parsed.patch} {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return moduleVersion{}, false
		}
		*field = value
	}
	if match[4] != "" {
		if !pseudoVersionTimestamp.MatchString(version) {
			return moduleVersion{}, false
		}
		parsed.pseudo = true
	}
	return parsed, true
}

// wrapperOlderByModuleVersion 按 Go 模块版本序判断 wrapper 是否旧于
// daemon。规则与 go 命令解析 @latest/@main 时的版本序一致:
//   - 两个 tag 按 major/minor/patch 数值比较;
//   - 伪版本 vX.Y.Z-0.<时间戳>-<hash> 是 tag vX.Y.(Z-1) 之后的提交,排在
//     vX.Y.(Z-1) 之后、vX.Y.Z 之前;
//   - 基底为 v0.0.0 的伪版本表示该提交可达范围内没有任何 tag,它与 tag
//     之间没有可靠顺序(本仓库曾因历史改写让 v0.1.0 脱离 main 谱系,
//     main 上更新的提交反而得到 v0.0.0 基底),这种组合拒绝比较;
//   - 两个伪版本之间由时间戳比较负责,不进入本函数。
func wrapperOlderByModuleVersion(wrapperVersion, daemonVersion string) (bool, error) {
	wrapper, ok := parseModuleVersion(wrapperVersion)
	if !ok {
		return false, fmt.Errorf("wrapper version %q is neither a release tag nor a pseudo-version", wrapperVersion)
	}
	daemon, ok := parseModuleVersion(daemonVersion)
	if !ok {
		return false, fmt.Errorf("daemon version %q is neither a release tag nor a pseudo-version", daemonVersion)
	}
	for _, side := range []moduleVersion{wrapper, daemon} {
		if side.pseudo && side.major == 0 && side.minor == 0 && side.patch == 0 {
			return false, fmt.Errorf("a v0.0.0-based pseudo-version has no tag base and cannot be ordered against a release tag")
		}
	}
	switch {
	case wrapper.major != daemon.major:
		return wrapper.major < daemon.major, nil
	case wrapper.minor != daemon.minor:
		return wrapper.minor < daemon.minor, nil
	case wrapper.patch != daemon.patch:
		return wrapper.patch < daemon.patch, nil
	default:
		// 同一 vX.Y.Z:伪版本是该正式发布之前的提交,比正式发布旧。
		return wrapper.pseudo && !daemon.pseudo, nil
	}
}

// pseudoVersionTimestamp 匹配 Go 模块伪版本的时间戳段:
// vX.0.0-yyyymmddhhmmss-hash / vX.Y.Z-pre.0.yyyymmddhhmmss-hash /
// vX.Y.Z-0.yyyymmddhhmmss-hash,末段恒为 12 位十六进制提交前缀。
var pseudoVersionTimestamp = regexp.MustCompile(`[.-](\d{14})-[0-9a-f]{12}$`)

// buildOrderTime 提取可比较的构建时间:优先 vcs.time 戳(源码构建);
// 缺失时回退解析伪版本时间戳——go install 模块构建无任何 vcs.* 戳
// (模块 zip 不含 .git),而伪版本时间戳与 vcs.time 同源同刻度(提交
// 时间 UTC)。T5 首版把缺 VCSTime 等同 devel 裸构建,导致其立项动机
// 场景(@main 追新的外部消费者)恰好永不触发接管(2026-08-31 外部
// 反馈实录)。两源皆缺(tag 发布版/devel 测试二进制)返回错误,调用方
// 维持"无法排序就不动手"。
func buildOrderTime(info buildinfo.Info) (time.Time, error) {
	if info.VCSTime != "" {
		at, err := time.Parse(time.RFC3339, info.VCSTime)
		if err != nil {
			return time.Time{}, fmt.Errorf("vcs time unparsable: %w", err)
		}
		return at, nil
	}
	version := info.Version
	// 源码构建可携带 +dirty 等 build metadata 后缀,先剥离再匹配。
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	match := pseudoVersionTimestamp.FindStringSubmatch(version)
	if match == nil {
		return time.Time{}, fmt.Errorf("no vcs stamp and version %q carries no pseudo-version timestamp", info.Version)
	}
	at, err := time.Parse("20060102150405", match[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("pseudo-version timestamp unparsable: %w", err)
	}
	return at.UTC(), nil
}

// loopbackEndpoint 判定 endpoint 主机是否本机回环(daemon.Client 的
// baseURL 恒带 scheme)。解析失败或主机非 loopback 一律按不可接管处理。
func loopbackEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultTerminateProcessPortable 供不支持信号语义的平台使用。
func takeoverUnsupported(int) error {
	return fmt.Errorf("in-place daemon takeover unsupported on this platform: %w", os.ErrInvalid)
}
