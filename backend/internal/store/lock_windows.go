//go:build windows

package store

import (
	"errors"
	"fmt"
	"os"
)

// ErrSessionBusy 表示会话正被另一个进程持有。
//
// 与 unix 版（lock_unix.go，flock 实现）语义一致。
var ErrSessionBusy = errors.New("会话正在另一个进程中使用")

// lock 是一个会话的独占锁（Windows 实现）。
//
// Windows 没有 flock。用 LockFileEx 的独占区间锁达到同等语义：
//
//   - 锁随句柄关闭由操作系统释放，进程被任务管理器杀掉也不会留下陈旧锁——
//     这正是 unix 版选 flock 而不是数据库记录的理由，在这里同样成立；
//   - LOCKFILE_FAIL_IMMEDIATELY 对应 unix 版的 LOCK_NB（非阻塞）；
//   - 锁区间用 [1, 1)：字节 0 常被一些工具当作"文件为空"的哨兵，避开它。
type lock struct {
	file *os.File
}

// acquireLock 以非阻塞方式取得锁文件的独占锁。
//
// LockFileEx 失败时 Windows 不区分"别人持有"和其他错误——但在这个用法里
// 锁文件只被 goseek 自己的进程触碰，竞争失败是最常见的失败原因，
// 因此统一映射为 ErrSessionBusy（与 unix 版的 EWOULDBLOCK 路径一致）。
func acquireLock(path string) (*lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败: %w", err)
	}
	if err := lockFileRange(file); err != nil {
		file.Close()
		return nil, ErrSessionBusy
	}
	return &lock{file: file}, nil
}

// release 释放锁并关闭锁文件。
//
// 锁文件本身保留在磁盘上，理由与 unix 版相同：删除会产生孤立 inode（Windows 上
// 是共享冲突）竞态。Unlock 之后句柄关闭时锁也会自动释放，这里显式 Unlock
// 是为了和 unix 版行为严格对齐：下一个进程立刻能看到锁已空闲。
func (held *lock) release() error {
	if err := unlockFileRange(held.file); err != nil {
		held.file.Close()
		return fmt.Errorf("释放锁失败: %w", err)
	}
	return held.file.Close()
}
