//go:build unix

package managed

import "syscall"

// defaultTerminateProcess 以 SIGTERM 请求旧 daemon 优雅关停(任务标
// abandoned、嵌入进度 journal 断点保护;T5 边界 5/7)。
func defaultTerminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
