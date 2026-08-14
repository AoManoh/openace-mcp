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
	"os"
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
// VCSTime 双方齐备且 wrapper 不旧于 daemon(旧 binary 无权拆新服务;
// 无法排序=不动手)。
func takeoverTarget(wrapper buildinfo.Info, status daemon.Status) (int, error) {
	if status.Service != "openace-daemon" {
		return 0, fmt.Errorf("target is not an openace daemon")
	}
	if status.PID <= 0 {
		return 0, fmt.Errorf("daemon pid unavailable")
	}
	if wrapper.VCSTime == "" || status.Build.VCSTime == "" {
		return 0, fmt.Errorf("vcs time unavailable on wrapper or daemon; cannot order builds")
	}
	wrapperAt, err := time.Parse(time.RFC3339, wrapper.VCSTime)
	if err != nil {
		return 0, fmt.Errorf("wrapper vcs time unparsable: %w", err)
	}
	daemonAt, err := time.Parse(time.RFC3339, status.Build.VCSTime)
	if err != nil {
		return 0, fmt.Errorf("daemon vcs time unparsable: %w", err)
	}
	if wrapperAt.Before(daemonAt) {
		return 0, fmt.Errorf("daemon build (%s) is newer than wrapper (%s); refusing takeover", status.Build.VCSTime, wrapper.VCSTime)
	}
	return status.PID, nil
}

// defaultTerminateProcessPortable 供不支持信号语义的平台使用。
func takeoverUnsupported(int) error {
	return fmt.Errorf("in-place daemon takeover unsupported on this platform: %w", os.ErrInvalid)
}
