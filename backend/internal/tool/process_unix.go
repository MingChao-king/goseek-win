//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// pathListSeparator 是 PATH 环境变量里条目之间的分隔符。
const pathListSeparator = ":"

// shellPath 返回用于执行命令的 shell。
func shellPath() string { return "/bin/bash" }

// configureProcessGroup 让命令独立成一个进程组，并在取消时按进程组杀。
//
// 默认行为只杀 shell 自己这一个进程。但 `sleep 300 &` 这样的命令会让 shell
// 立刻退出、把 sleep 留给系统托管——那个孙进程会一直活着占用资源，而我们
// 连它的 PID 都不知道。设成独立进程组之后，整棵进程树共享一个 PGID，
// 一次信号就能全覆盖。
func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// kill(-pid) 是 POSIX 的约定：**负数**表示"发给这个 PGID 的所有进程"。
	// 因为设了 Setpgid，子进程的 PID 恰好就是新进程组的 PGID，所以取负即可。
	// 用 SIGKILL 而不是 SIGTERM：这条路径只在超时或取消时走到，那时已经不需要
	// 再给命令收尾的机会了。
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

// processAlive 探测进程是否仍然存在（信号 0 只检查权限与存在性）。
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// terminateProcess 强制终止一个进程。
func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
