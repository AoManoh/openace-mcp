//go:build unix

package index

import (
	"os"
	"syscall"
)

// fileIdentity 是判断分段文件是否被改写的依据：大小、修改时间、inode 与设备号。
// 原地改写会改变大小或修改时间，替换文件会改变 inode。
type fileIdentity struct {
	size    int64
	mtimeNs int64
	inode   uint64
	device  uint64
}

func identityOf(info os.FileInfo) fileIdentity {
	identity := fileIdentity{size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity.inode = uint64(stat.Ino)
		identity.device = uint64(stat.Dev)
	}
	return identity
}
