//go:build windows

package tool

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// pathListSeparator 是 Windows PATH 环境变量的分隔符。
const pathListSeparator = ";"

// shellPath 返回用于执行命令的 shell。
//
// Windows 没有 /bin/bash。cmd.exe 是保底选择；如果装了 Git for Windows，
// bash.exe 通常在 PATH 里，优先用它——模型给出的命令大多带 unix 风味
//（管道、rm -rf、grep），cmd 的语义差异会造成大量静默失败。
func shellPath() string {
	for _, name := range []string{"bash.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return "cmd.exe"
}

// configureProcessGroup 在 Windows 上挂接进程树的终止。
//
// Windows 没有进程组语义，taskkill /T 是官方等价物：/T 连同子进程一起终止，
// /F 强制（对应 SIGKILL 的语义——取消时不需要给命令收尾机会）。
// command.Cancel 在 CommandContext 的 ctx 被取消时由 os/exec 调用。
func configureProcessGroup(command *exec.Cmd) {
	command.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", itoa(command.Process.Pid))
		kill.Stdout = nil
		kill.Stderr = nil
		return kill.Run()
	}
	// TaskKill 需要一点时间枚举并终止整棵树；WaitDelay 仍由调用方设置，
	// 这里不再额外等待。
	_ = strings.TrimSpace // 保持 strings 引用（未来扩展用）
}

// itoa 是 strconv.Itoa 的本地别名，避免只为一个调用引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// processAlive 探测进程是否仍然存在。
//
// OpenProcess 失败即视为不存在（权限不足的场景在 goseek 的用法里不会出现：
// 目标进程是它自己的子进程）。
func processAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// terminateProcess 强制终止一个进程。
func terminateProcess(pid int) error {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", itoa(pid))
	kill.Stdout = os.Stdout
	kill.Stderr = os.Stderr
	return kill.Run()
}

var _ = syscall.GetExitCodeProcess
