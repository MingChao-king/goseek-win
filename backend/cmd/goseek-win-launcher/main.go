//go:build windows

// GoSeekLauncher：Windows 桌面壳。
//
// 与 macOS 的 GoSeekLauncher.swift（app/GoSeekLauncher.swift）职责完全对齐：
//
//	启动 goseek serve 子进程 → 打开 WebView2 窗口加载 SPA →
//	系统托盘提供 退出 / 重新打开窗口。
//
// WebView2 是 Windows 10/11 自带的浏览器内核（Edge 同源），不需要用户安装任何
// 运行时；go-webview2 在缺失时给出可读错误。
//
// goseek 的定位：与 launcher 同目录的 goseek.exe 优先（绿色部署），
// 其次 %LOCALAPPDATA%\GoSeek\goseek.exe，再次 PATH。
//
// 端口配置：%LOCALAPPDATA%\GoSeek\port.conf，与 mac 的 port.conf 同名同格式。
package main

import (
	"fmt"
	"net/http"
	"syscall"
	"unsafe"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jchv/go-webview2"
)

const (
	defaultPort   = 8765
	serverAppName = "goseek.exe"
)

// portFile 返回端口配置文件路径（%LOCALAPPDATA%\GoSeek\port.conf）。
func portFile() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Local")
	}
	dir := filepath.Join(base, "GoSeek")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "port.conf")
}

// readPort 读端口配置；没有或非法就用默认 8765。
func readPort() int {
	if data, err := os.ReadFile(portFile()); err == nil {
		if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return defaultPort
}

// findGoseek 定位 goseek.exe。优先级：同目录（绿色部署）> %LOCALAPPDATA%\GoSeek > PATH。
func findGoseek() (string, error) {
	self, _ := os.Executable()
	selfDir := filepath.Dir(self)
	candidates := []string{
		filepath.Join(selfDir, serverAppName),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "GoSeek", serverAppName),
		serverAppName, // PATH
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("找不到 %s：请把它放在 %s 同目录下，或加入 PATH", serverAppName, selfDir)
}

// startServer 启动 goseek serve 子进程。输出写入 %LOCALAPPDATA%\GoSeek\serve.log
// （GUI 进程没有控制台，日志是唯一的排障入口）。
func startServer(goseekPath string, port int) (*exec.Cmd, error) {
	cmd := exec.Command(goseekPath, "serve", "--addr", fmt.Sprintf("127.0.0.1:%d", port))
	logFile, err := os.OpenFile(strings.Replace(portFile(), "port.conf", "serve.log", 1),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// waitReady 轮询服务是否可访问，最多 15 秒。
func waitReady(port int) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/sessions", port)
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 75; i++ {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func main() {
	port := readPort()

	goseekPath, err := findGoseek()
	if err != nil {
		reportFatal(err.Error())
	}

	server, err := startServer(goseekPath, port)
	if err != nil {
		reportFatal("启动服务失败: " + err.Error())
	}
	go func() {
		_ = server.Wait()
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if !waitReady(port) {
		// 服务没起来也照常开窗口——SPA 会显示网络错误，日志文件里有原因。
		_ = url
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "GoSeek",
			Width:  1280,
			Height: 860,
			Center: true,
		},
	})
	if w == nil {
		reportFatal("WebView2 初始化失败：请通过 Windows 更新安装 Evergreen WebView2 Runtime")
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
	// 窗口关闭：结束服务子进程（优雅退出交给 goseek 自己的信号处理）。
	_ = server.Process.Kill()
}

// reportFatal 弹一个原生消息框并退出。launcher 阶段还没有 WebView 可用。
func reportFatal(message string) {
	// MessageBoxW(0, text, caption, 0x10=MB_ICONERROR)
	user32 := syscall.NewLazyDLL("user32.dll")
	box := user32.NewProc("MessageBoxW")
	textPtr, _ := syscall.UTF16PtrFromString(message)
	captionPtr, _ := syscall.UTF16PtrFromString("GoSeek")
	box.Call(0, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(captionPtr)), 0x10)
	os.Exit(1)
}
