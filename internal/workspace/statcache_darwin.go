//go:build darwin

package workspace

import (
	"os"
	"syscall"
)

// fillPlatformStat 从 macOS 的 stat 结构取 ctime、inode 与设备号。
func fillPlatformStat(key *statKey, info os.FileInfo) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	key.ctimeNs = st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
	key.ino = st.Ino
	key.dev = uint64(st.Dev)
}
