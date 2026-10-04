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

// removeLockFile 在 Unix 下保留锁文件标记：占用与否完全由 flock 锁决定，
// 文件是否存在从不作为判据，残留标记不会阻止后续打开。
//
// 不能在关闭时解除链接，否则会出现交接竞态：旧 Store 释放 flock 后、
// 新 Store 已在同一 inode 上取得 flock 时，若旧 Store 再 unlink 路径，
// 第三个调用方会因 O_CREATE 建出全新 inode 并 flock 成功，导致两个
// 存活 Store 同时占用同一目录。保留路径让所有人始终 flock 同一个
// inode，交接过程中独占性不会断档。
func removeLockFile(path string) error { return nil }
