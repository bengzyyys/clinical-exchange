//go:build windows

package clinical

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Windows 字节范围锁标志（kernel32 LockFileEx 的 dwFlags）。
const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// tryLockFile 立即对锁文件首字节加 Windows 排他锁（LockFileEx）：
//   - LOCKFILE_EXCLUSIVE_LOCK：同一区域只允许一个句柄持有，第二个 Store
//     （同一进程内的另一次 Open，或另一个独立进程）立即冲突；
//   - LOCKFILE_FAIL_IMMEDIATELY：冲突时立刻返回 ERROR_LOCK_VIOLATION，
//     不等待原使用者关闭。
//
// 锁与打开句柄绑定：句柄关闭（包括进程崩溃）时由操作系统自动释放。
// 因此目录里残留的锁文件本身只是占位标记，单凭标记存在不能断定目录仍被
// 占用；占用判断始终与实际持有的句柄有效期一致。
func tryLockFile(f *os.File) error {
	var ol syscall.Overlapped // Offset/OffsetHigh 为 0：锁定从首字节开始的区域
	// 只锁一个字节即可互斥；锁的是字节区域而非文件内容。
	r1, _, errno := procLockFileEx.Call(
		uintptr(f.Fd()),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,    // dwReserved，必须为 0
		1, 0, // 只锁定 1 个字节（低位/高位长度）
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return fmt.Errorf("clinical: data directory is already in use: %w", errno)
	}
	return nil
}

func unlockFile(f *os.File) {
	var ol syscall.Overlapped
	// 区域必须与加锁时一致；即便这里失败，句柄关闭后锁也会被系统释放。
	_, _, _ = procUnlockFileEx.Call(uintptr(f.Fd()), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
}
