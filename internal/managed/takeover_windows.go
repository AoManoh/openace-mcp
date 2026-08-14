//go:build !unix

package managed

// defaultTerminateProcess:Windows 无优雅信号语义,首版不做自动接管
// (保持报错+pid 修复指引,与 wrapper 升级自愈的平台门控同口径)。
func defaultTerminateProcess(pid int) error {
	return takeoverUnsupported(pid)
}
