# GoSeek Windows 桌面版

## 构成

| 文件 | 作用 |
|---|---|
| `GoSeek-win-launcher.exe` | 双击入口：启动 goseek serve → 打开 WebView2 窗口（约 9.8MB） |
| `goseek.exe` | 服务本体：API + 内嵌前端 + 内置插件全部打包（约 33MB） |
| `goseek-browser.exe` | 浏览器插件的 bin 工具（可选） |

## 安装（绿色部署）

1. 新建一个目录，如 `C:\Tools\GoSeek\`，把三个 exe 放进去；
2. 双击 `GoSeek-win-launcher.exe`——窗口打开即用；
3. 数据落在 `%LOCALAPPDATA%\goseek\`（数据库/会话/图片/插件），
   日志在 `%LOCALAPPDATA%\GoSeek\serve.log`，端口配置 `%LOCALAPPDATA%\GoSeek\port.conf`。

## 依赖

- Windows 10 1803+ / Windows 11（自带 WebView2 Evergreen Runtime）；
- 若提示 WebView2 初始化失败：从微软官网装 "Evergreen WebView2 Runtime"；
- 想让 bash 工具好用：安装 Git for Windows（提供 bash.exe）与 ripgrep（提供 rg）。

## 与 macOS 版的对应关系

| | macOS | Windows |
|---|---|---|
| 壳 | GoSeek.app（Swift + WKWebView） | GoSeek-win-launcher.exe（Go + WebView2） |
| 服务 | ~/go/bin/goseek | 同目录 goseek.exe |
| 数据目录 | ~/.local/share/goseek | %LOCALAPPDATA%\goseek |
| 端口配置 | ~/Library/Application Support/GoSeek/port.conf | %LOCALAPPDATA%\GoSeek\port.conf |
