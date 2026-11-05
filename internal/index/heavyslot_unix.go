//go:build unix

package index

import (
	"fmt"
	"os"
	"syscall"
)

// checkDirOwner 要求名额目录属于当前用户。
func checkDirOwner(dir string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("重型构建名额目录 %s 属于用户 %d，不是当前用户 %d，不使用", dir, st.Uid, uid)
	}
	return nil
}
