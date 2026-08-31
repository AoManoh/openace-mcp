package managed

// T5(docs/tasks/T5-connect-time-daemon-takeover.md):connect 时旧 daemon
// 自动接管。场景=wrapper 升级后,健康但 build 过期的 daemon 占住 managed
// 地址,此前只能报错并等待人工清场(外部灰度 2026-08-14 实录)。接管仅在
// 全部安全门通过时发生:managed 生命周期 + build 级不匹配 + 双方 VCSTime
// 齐备且 wrapper 不旧于 daemon + 目标确为 openace-daemon 且 pid 可用。
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
	wrapperAt, err := buildOrderTime(wrapper)
	if err != nil {
		return 0, fmt.Errorf("wrapper build time unknown (%v); cannot order builds", err)
	}
	daemonAt, err := buildOrderTime(status.Build)
	if err != nil {
		return 0, fmt.Errorf("daemon build time unknown (%v); cannot order builds", err)
	}
	if wrapperAt.Before(daemonAt) {
		return 0, fmt.Errorf("daemon build (%s) is newer than wrapper (%s); refusing takeover", daemonAt.Format(time.RFC3339), wrapperAt.Format(time.RFC3339))
	}
	return status.PID, nil
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
