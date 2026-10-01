//go:build !windows

package clinical

import (
	"fmt"
	"os"
	"syscall"
)

// tryLockFile 对锁文件加排他咨询锁。同一目录被第二个 Store 打开时直接失败，
// 避免两个本地进程并发写同一份快照而互相覆盖。
func tryLockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("clinical: data directory is already in use: %w", err)
	}
	return nil
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
