//go:build !unix

package index

import "os"

// fileIdentity 是判断分段文件是否被改写的依据。非 Unix 平台（Windows）的
// os.FileInfo 不带 inode，只比较大小与修改时间。
type fileIdentity struct {
	size    int64
	mtimeNs int64
}

func identityOf(info os.FileInfo) fileIdentity {
	return fileIdentity{size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
}
