//go:build !linux && !darwin

package workspace

import "os"

// fillPlatformStat 在 Linux 与 macOS 以外的平台不取 ctime 与 inode:Windows 的
// os.FileInfo 不提供这两项,命中条件保持大小与修改时间。
func fillPlatformStat(*statKey, os.FileInfo) {}
