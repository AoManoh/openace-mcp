//go:build windows

package managed

import "os/exec"

// applyDaemonSysProcAttr 在 Windows 为空实现(M6 的进程组语义是 Unix
// 信号模型;Windows 控制台事件另有隔离机制,维持现状)。
func applyDaemonSysProcAttr(cmd *exec.Cmd) {}
