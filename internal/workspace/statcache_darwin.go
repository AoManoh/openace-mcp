//go:build darwin

package workspace

import (
	"os"
	"syscall"
)

// StatSnapshotSupported 报告本平台能否使用 stat 快照（stat-snapshot.json）：要取得到
// ctime 与 inode，保留修改时间的等长改写在快照跨重启加载后才不会一直命中旧哈希。
const StatSnapshotSupported = true

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
