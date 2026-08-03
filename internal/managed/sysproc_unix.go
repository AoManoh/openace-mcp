//go:build unix

package managed

import (
	"os/exec"
	"syscall"
)

// applyDaemonSysProcAttr 使 managed daemon 脱离 wrapper 的进程组/会话
// (M6):Setsid 后前台 Ctrl+C(SIGINT 发往前台进程组)不再波及跨会话
// 共享的 daemon;显式停止走 daemon 停机管道。
func applyDaemonSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
