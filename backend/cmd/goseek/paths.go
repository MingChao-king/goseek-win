package main

import (
	"runtime"
	"fmt"
	"os"
	"path/filepath"
)

// GoSeek 的配置和数据按 XDG Base Directory 约定存放：
//
//	配置 ~/.config/goseek/
//	数据 ~/.local/share/goseek/
//
// 不用 macOS 的 ~/Library/Application Support——那是 GUI 程序的惯例，命令行工具
// 的通行做法是 ~/.config（git、gh、docker 都在那里），而且 macOS 与 Linux 一致，
// 用户也容易直接查看和编辑。
//
// XDG_CONFIG_HOME 与 XDG_DATA_HOME 有值时优先。它们的第一个调用者是测试：
// 测试必须把这两个目录指向临时目录，否则会污染真实配置和真实会话。
const applicationName = "goseek"

// configDir 返回配置目录，不创建它。
func configDir() (string, error) {
	return userDir("XDG_CONFIG_HOME", ".config")
}

// dataDir 返回数据目录，不创建它。
func dataDir() (string, error) {
	return userDir("XDG_DATA_HOME", filepath.Join(".local", "share"))
}

// legacySessionsDir 返回 M2 使用过的、每会话一个 JSON 文件的目录。
//
// M2.5 起会话存在数据库里，这个目录不再被读写。保留这个函数只为在启动时提醒
// 用户旧文件还在哪，避免他以为历史丢了。等到不再需要提醒时可以整个删掉。
func legacySessionsDir() (string, error) {
	base, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sessions"), nil
}

// countLegacySessionFiles 返回旧目录里还剩多少个会话 JSON 文件。
func countLegacySessionFiles() (int, string) {
	directory, err := legacySessionsDir()
	if err != nil {
		return 0, ""
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, ""
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count, directory
}

// userDir 按 XDG 规则解析一个用户目录。
//
// 只接受绝对路径的环境变量取值：XDG 规范如此要求，而相对路径会让配置位置随
// 启动目录漂移——GoSeek 恰恰要在任意目录启动。
func userDir(environmentVariable, fallbackRelative string) (string, error) {
	// Windows 没有 XDG 与点目录约定：用 %LocalAppData%\goseek。
	// XDG 变量在这套 fallback 下仍然可用（用户显式设置时优先），与 unix 行为一致。
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, applicationName), nil
		}
	}
	if base := os.Getenv(environmentVariable); filepath.IsAbs(base) {
		return filepath.Join(base, applicationName), nil
	}
	//获得用户主目录
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户主目录: %w", err)
	}
	return filepath.Join(home, fallbackRelative, applicationName), nil
}
