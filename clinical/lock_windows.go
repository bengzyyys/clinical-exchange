//go:build windows

package clinical

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Windows 字节范围锁标志，见
// https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-lockfileex
const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	lockEntireFile          = ^uint32(0)
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// tryLockFile 通过 LockFileEx 对锁文件加排他字节范围锁（非阻塞）。
//
// 锁由操作系统按“打开的文件句柄”持有：与 Unix 的 flock 一样，同进程内的
// 另一个句柄以及另一个进程对同一区间加排他锁都会立即得到
// ERROR_LOCK_VIOLATION；句柄关闭（含进程退出）时锁自动释放。因此目录里
// 残留的 .clinical.lock 标记文件本身不会让重开失败——只有仍有存活句柄
// 持锁时第二个 Store 才会被拒绝。
func tryLockFile(f *os.File) error {
	// LockFileEx 即使在同步句柄上也要求传入 OVERLAPPED，其中给出锁区间
	// 起始偏移；偏移 0、长度取满 64 位即锁定整个文件。
	var ol syscall.Overlapped
	flags := uint32(lockfileExclusiveLock | lockfileFailImmediately)
	r1, _, callErr := procLockFileEx.Call(
		uintptr(syscall.Handle(f.Fd())),
		uintptr(flags),
		0,
		uintptr(lockEntireFile),
		uintptr(lockEntireFile),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return fmt.Errorf("clinical: data directory is already in use: %w", callErr)
	}
	return nil
}

func unlockFile(f *os.File) {
	var ol syscall.Overlapped
	// 区间必须与加锁时一致（偏移 0、整个文件）。
	procUnlockFileEx.Call(
		uintptr(syscall.Handle(f.Fd())),
		0,
		uintptr(lockEntireFile),
		uintptr(lockEntireFile),
		uintptr(unsafe.Pointer(&ol)),
	)
}

// removeLockFile 在 Windows 下保留锁文件标记：文件是否存在从不作为占用
// 判据，真正的互斥来自随句柄生效/释放的字节范围锁。保留残留标记也避免
// 去删除可能仍被其他进程以不共享删除方式打开的文件。
func removeLockFile(path string) error { return nil }
