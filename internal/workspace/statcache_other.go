//go:build !linux && !darwin

package workspace

import "os"

// StatSnapshotSupported 为 false：这些平台取不到 ctime 与 inode，stat 快照跨重启加载后，
// 保留修改时间的等长改写会一直命中旧哈希，所以不保存也不加载快照，重启后的第一次扫描
// 照旧重新哈希全部文件。
const StatSnapshotSupported = false

// fillPlatformStat 在 Linux 与 macOS 以外的平台不取 ctime 与 inode:Windows 的
// os.FileInfo 不提供这两项,命中条件保持大小与修改时间。
func fillPlatformStat(*statKey, os.FileInfo) {}
