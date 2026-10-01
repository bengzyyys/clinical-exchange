//go:build windows

package clinical

import "os"

// Windows 下使用简单占位（本程序主要在本地 Unix 环境运行）。
func tryLockFile(f *os.File) error { return nil }

func unlockFile(f *os.File) {}
