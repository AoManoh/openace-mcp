//go:build unix

package mcp

import (
	"os"
	"syscall"
)

// execSelf 以自身路径原地 exec(保留 pid 与 stdio FD;升级后同路径已是
// 新 binary)。成功不返回。
func execSelf(argv0 string, statePath string) error {
	env := append(os.Environ(), EnvHandoffState+"="+statePath)
	return syscall.Exec(argv0, os.Args, env)
}
