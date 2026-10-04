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

// 锁文件在关闭后不删除（与 Windows 一致）：占用与否完全由 flock 锁决定，
// 文件是否存在从不作为判据。若在解锁后删除文件，已打开旧 inode 并完成
// 加锁的接手方会与随后新建锁文件的第三个调用方各持一把不同 inode 上的锁，
// 两个 Store 将同时操作同一目录。保留残留标记不会影响重开——没有存活句柄
// 持锁时 flock 立即成功。
