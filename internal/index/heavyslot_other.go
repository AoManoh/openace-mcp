//go:build !unix

package index

import "os"

// checkDirOwner 在非 Unix 平台不检查属主：Windows 的临时目录本来就在用户目录下。
func checkDirOwner(string, os.FileInfo) error { return nil }
