//go:build !windows

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrSessionBusy 表示会话正被另一个进程持有。
//
// 它是一个哨兵错误，让入口能够用 errors.Is 判断并给出"请换一个会话"这类提示，
// 而不是把底层的 EWOULDBLOCK 直接抛给用户。
var ErrSessionBusy = errors.New("会话正在另一个进程中使用")

// lock 是一个会话的独占锁。
//
// 锁没有放进数据库。SQLite 自己的锁保护的是文件，不是"这个会话正被谁用"；而用表里
// 的一行加 pid 表示持有者，进程被 kill -9 之后会留下陈旧记录，就得回去做存活探测。
// flock 的锁随 fd 关闭由内核释放，kill -9 也不会留下陈旧锁，正好避开这一摊。
//
// 锁文件是每个会话一个、独立于数据库文件的空文件。它不参与任何替换或删除，
// 因此始终是同一个 inode。
type lock struct {
	file *os.File
}

// acquireLock 以非阻塞方式取得锁文件的独占锁。
//
// 非阻塞是有意的：另一个终端正在用这个会话时，应该立刻明确地告诉用户，
// 而不是让他对着一个没有反应的提示符等待。
func acquireLock(path string) (*lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrSessionBusy
		}
		return nil, fmt.Errorf("加锁失败: %w", err)
	}
	return &lock{file: file}, nil
}

// release 释放锁并关闭锁文件。
//
// 锁文件本身保留在磁盘上：删除它会产生竞态——另一个进程可能已经打开了这个
// inode 并在上面等待，删掉之后它锁住的就是一个谁也看不到的孤立 inode。
func (held *lock) release() error {
	if err := syscall.Flock(int(held.file.Fd()), syscall.LOCK_UN); err != nil {
		held.file.Close()
		return fmt.Errorf("释放锁失败: %w", err)
	}
	return held.file.Close()
}
