//go:build linux

package workspace

import (
	"os"
	"syscall"
)

// fillPlatformStat 从 Linux 的 stat 结构取 ctime、inode 与设备号。
func fillPlatformStat(key *statKey, info os.FileInfo) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	key.ctimeNs = st.Ctim.Sec*1e9 + st.Ctim.Nsec
	key.ino = st.Ino
	key.dev = uint64(st.Dev)
}
