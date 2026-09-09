//go:build windows

package store

import (
	"os"
	"syscall"
	"unsafe"
)

// lockFileRange 对已打开文件的 [1, 1) 字节区间加独占锁（非阻塞）。
//
// 直接调用 kernel32!LockFileEx。Go 的 syscall 包在 Windows 上不暴露这个函数，
// lazyproc 则要引新依赖；一个 NewLazySystemDLL 的调用是最轻的做法。
var (
	kernel32         = syscall.MustLoadDLL("kernel32.dll")
	procLockFileEx   = kernel32.MustFindProc("LockFileEx")
	procUnlockFileEx = kernel32.MustFindProc("UnlockFileEx")
)

func lockFileRange(file *os.File) error {
	const (
		lockfileExclusive   = 2
		lockfileFailImmedi  = 1
	)
	handle := syscall.Handle(file.Fd())
	// OFFSET 和长度都是 64 位，按低位在前传两个 32 位；区间取 [1,1) 避开字节 0。
	r1, _, errNo := procLockFileEx.Call(
		uintptr(handle),
		uintptr(lockfileExclusive|lockfileFailImmedi),
		0,              // 保留，必须 0
		1, 0,           // 长度 1（低/高 32 位）
		uintptr(unsafe.Pointer(&syscall.Overlapped{Offset: 1})),
	)
	if r1 == 0 {
		return errNo
	}
	return nil
}

func unlockFileRange(file *os.File) error {
	handle := syscall.Handle(file.Fd())
	r1, _, errNo := procUnlockFileEx.Call(
		uintptr(handle),
		0,
		1, 0,
		uintptr(unsafe.Pointer(&syscall.Overlapped{Offset: 1})),
	)
	if r1 == 0 {
		return errNo
	}
	return nil
}
