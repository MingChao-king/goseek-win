# GoSeek for Windows

用 Go 从零实现的单 Agent 编码助手 **GoSeek** 的 **Windows 发行版**：**模型上下文的完整治理**
（分层压缩 + 无损回查 + 超大输出分页）+ **多模态浏览器操作** + **可扩展的插件/MCP/Skill 体系**，
以「桌面窗口 + 本地服务」的形式运行在 Windows 10/11 上。

- 代码与 macOS 版同源（Vite/React 前端 `go:embed` 进服务二进制，单文件即完整产品）；
- 本仓库是**独立发布仓库**，提供 Windows 安装包与 Windows 专属的桌面壳（WebView2）；
- **默认的模型目录只是示例**。作者自用版接的是自己 ai-switch 中转里的 4 个模型，
  与你无关——你需要按 [「为你的模型定制」](#为你的模型定制ai-中转api--写给-ai-agent-的引导章节)
  接入自己的中转或官方 API。那一章是**写给 AI Agent 看的**：把本 README 交给你的
  Agent（Claude Code、Codex、GoSeek 等），它会一步步引导你完成适配。

---

## 这是什么 / 不是什么

| | 说明 |
|---|---|
| ✅ 是 | 一个跑在你 Windows 机器上的编码 Agent：本地服务 + 桌面窗口，模型走你自己的 API |
| ✅ 是 | OpenAI 兼容客户端：任何中转（ai-switch / one-api / new-api …）或官方 API 都能接 |
| ❌ 不是 | 云服务。数据全部在本机，配置文件里是你的 key，不出本机 |
| ❌ 不是 | 免配置开箱即用。**必须先配置模型接入**（见下一节），否则无法对话 |

---

## 快速开始

### 1. 下载安装包

下载 `GoSeek-win-amd64.zip`（约 45MB）：本仓库 `release/` 目录里就有（也发布在 Releases 页），解压到任意目录，例如 `C:\Tools\GoSeek\`：

```
C:\Tools\GoSeek\
├── GoSeek-win-launcher.exe   ← 双击这个启动（桌面窗口入口）
├── goseek.exe                ← 服务本体（API + 内嵌前端 + 内置插件）
├── goseek-browser.exe        ← 浏览器插件的工具（可选）
└── mock-provider.exe         ← 假供应商，用于离线自检（可选，见下文）
```

无需安装器、无需管理员权限；不想用了删掉目录即可（数据在 `%LOCALAPPDATA%`，见下文）。

### 2.（推荐）先跑离线自检

不消耗任何 API 额度，验证安装包在你机器上能完整工作：

```powershell
cd C:\Tools\GoSeek
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 `
    -GoseekBin .\goseek.exe -MockBin .\mock-provider.exe
```

看到 `PASS` 即链路完好（会话创建 → 流式模型请求 → 工具调用真实写盘 → 文本回复）。

### 3. 配置你的模型（必做）

把 README（本文件）交给你的 AI Agent，让它引导你完成 [模型定制](#为你的模型定制ai-中转api--写给-ai-agent-的引导章节)；
或者你手动配置最小三项——编辑 `%LOCALAPPDATA%\goseek\config.env`（没有就新建）：

```bash
GOSEEK_API_KEY=sk-你的key
GOSEEK_BASE_URL=https://你的中转地址/v1     # 到 /v1 或 /v4 这一层，不带 /chat/completions
GOSEEK_MODEL=你的模型名
GOSEEK_CONTEXT_WINDOW=128000                # 模型上下文窗口，见定制章节第 3 步
```

### 4. 启动

双击 `GoSeek-win-launcher.exe`：窗口打开、左下角仪表出现数字，就可以开始对话了。

> 如果双击后 Windows SmartScreen 提示"已保护你的电脑"：点"更多信息 → 仍要运行"。
> 安装包未做代码签名（签名需要付费证书）；不放心可以先跑第 2 步的离线自检，
> 或只用 `goseek.exe serve`（见下文"不装桌面壳直接用 CLI"）。

---

## 为你的模型定制（AI 中转/API）——写给 AI Agent 的引导章节

> **给 Agent 的指令**：你（Agent）正在帮一位用户把 GoSeek for Windows 接到 TA 自己的
> 模型服务上。GoSeek 走 **OpenAI 兼容协议**（`/chat/completions`），任何中转
> （ai-switch、one-api、new-api…）或官方 API（DeepSeek、GLM、Qwen、OpenAI…）都能接。
> 按下面五步引导，**每步验证通过再进下一步**。不要替用户编造凭据——key 和 URL
> 必须由用户提供。你在什么环境跑（macOS/Linux/Windows 都行），就在什么环境按对应路径操作。

### 第 1 步：收集用户的三项信息

向用户确认（示例问法）：

1. "你的中转 / 官方 API 的 **Base URL** 是什么？"（形如 `https://xxx.com/v1`，
   **不含** `/chat/completions`；中转面板的"接口地址 / API 地址"就是它）
2. "**API Key**？"（只让 TA 填进配置文件；不要让 TA 把 key 明文贴到对话里还存到别处）
3. "打算用哪个**模型名**？"（中转面板的模型列表里能看到，如 `deepseek-v4-pro`、`glm-5.3`）

### 第 2 步：写入配置

**Windows（PowerShell）**——目标文件 `%LOCALAPPDATA%\goseek\config.env`：

```powershell
New-Item -Force -ItemType Directory "$env:LOCALAPPDATA\goseek" | Out-Null
@'
GOSEEK_API_KEY=<用户提供>
GOSEEK_BASE_URL=<用户提供>
GOSEEK_MODEL=<用户提供>
GOSEEK_CONTEXT_WINDOW=<第 3 步定，可先占位>
'@ | Out-File "$env:LOCALAPPDATA\goseek\config.env" -Encoding utf8
```

**macOS / Linux（Agent 自己的机器上测试时）**：

```bash
mkdir -p ~/.config/goseek && cat > ~/.config/goseek/config.env <<EOF
GOSEEK_API_KEY=<用户提供>
GOSEEK_BASE_URL=<用户提供>
GOSEEK_MODEL=<用户提供>
GOSEEK_CONTEXT_WINDOW=<第 3 步定，可先占位>
EOF
```

取值顺序是 **环境变量 > config.env > 内置默认**，键名完全一致：临时换模型测试可以
`GOSEEK_MODEL=xxx` 环境变量覆盖，不用改文件。

### 第 3 步：定上下文窗口（关键——影响压缩触发与费用）

GoSeek 的压缩由窗口三条线驱动（硬边界 95% / 触发线 80% / 目标 20%）。
**窗口值必须真实**：配大了请求会被供应商拒绝；配小了压缩过频，多花钱还把原文提前变摘要。
不配 `GOSEEK_CONTEXT_WINDOW` 时**压缩完全不触发**（这是刻意的安全设计：宁可直连失败，
也不基于编造的窗口做有损压缩）——所以这一步是**必做项**。

两条确认途径，按实际情况选：

- **途径 A：用户自己知道 / 能查到**。中转面板或模型文档里写着窗口大小
  （如 128000、65536、1M）——和用户确认后直接写入 `GOSEEK_CONTEXT_WINDOW`。
- **途径 B：拉取中转实测**。用一个长 prompt 真实调一次用户的中转
  （Agent 可以用 curl 直接打 `<BASE_URL>/chat/completions`，让模型重复一段长文本，
  或把一段长材料发给模型），观察：
  - 返回 200：GoSeek 面板右下角会显示供应商实测 `prompt_tokens`，取其峰值 ×1.2 作为窗口；
  - 返回 400 且报 `maximum context length ... tokens`：错误信息里的就是真值，直接用它；
  - 报余额/权限错：先解决凭据问题，回到第 1 步。

写入后**必须完全重启**（托盘退出 launcher 再重开，或重启 `goseek serve`）才生效。

### 第 4 步：把模型登记进目录（一行配置 + 可选的一处代码）

**先理解两套机制，再决定要不要改代码：**

| 机制 | 来源 | 作用 |
|---|---|---|
| `GOSEEK_MODEL` + `GOSEEK_CONTEXT_WINDOW` | config.env / 环境变量 | **默认模型**：所有新会话默认用它，对话、工具、压缩全部正常 |
| `backend/internal/model/catalog.go` 的 `Catalog` | 编译进二进制 | **模型目录**：界面"新会话/模型下拉"能选哪些模型 |

**只改配置（不改代码）就够用的场景**：用户只用一个模型。此时 UI 的模型下拉会显示
目录里的示例条目，但只要不手动切换，请求始终走 `GOSEEK_MODEL`。告诉用户：
**下拉里的示例模型不是你的，别选**。

**需要登记目录的场景**：用户想用多个模型、或想让下拉里显示自己的模型。
编辑 `backend/internal/model/catalog.go`，按同样格式加一条：

```go
{
    Name:              "<GOSEEK_MODEL 用的那个名字>",  // 必须与 GOSEEK_MODEL 一致
    Display:           "<下拉里显示的名字>",
    ContextWindow:     <第 3 步定的窗口>,              // 与 GOSEEK_CONTEXT_WINDOW 一致
    WindowSource:      "user-configured",
    SupportsTools:     true,      // 该模型支持 function calling 吗？不支持别用它跑 Agent
    SupportsStreaming: true,      // 支持流式吗？
    SupportsVision:    false,     // 能收 image_url（看图）才设 true
},
```

改完**重新编译**（在任意有 Go 工具链的机器上交叉编译，见下文「从源码构建」），
用新二进制替换旧的。**三处一致性检查（Agent 逐项核对）**：

- `Catalog[].Name` == `GOSEEK_MODEL`（模型能力判定靠名字精确匹配）；
- `Catalog[].ContextWindow` == `GOSEEK_CONTEXT_WINDOW`（估算与压缩判据同源）；
- 模型真实支持 tools——不支持的话工具调用会一直失败，换模型或别开工具。

### 第 5 步：验证闭环

引导用户依次验证，**每步让用户报告现象**（Windows 上由用户操作；Agent 在
macOS/Linux 上也可以用同一份配置先自测一遍）：

1. 双击 launcher（或 `goseek.exe serve` 后浏览器开 `http://127.0.0.1:8765`）
   → 发一句"你好" → 有正常回复即连通；
2. 让它 `ls` 一下当前目录 → 工具调用正常（说明 tools 协议通）；
3. 看面板右下角上下文仪表 → 数字在涨，且**供应商实测值会覆盖本地估算**；
   （若实测长期 > 估算 15% 以上，估算安全系数会自动上调并大声报警，无需干预）
4. 长对话把占用推过 80% → 面板出现"压缩中" → 完成后回到 20% 左右 → 压缩闭环工作；
5. （可选，模型需 `SupportsVision: true`）拖一张图进输入框 → 模型能描述图片。

### 常见故障对照（Agent 排查表）

| 现象 | 原因 | 处理 |
|---|---|---|
| 401 / 403 | key 错，或中转要求不同认证头 | 核对 key；GoSeek 默认发 `Authorization: Bearer <key>` |
| 404 | Base URL 多了/少了路径 | 确认最终请求 URL 是 `<BASE_URL>/chat/completions`；Base URL 填到 `/v1` 这层 |
| 400 invalid tools | 模型不支持 function calling | 换支持 tools 的模型（工具调用是 Agent 的硬依赖） |
| 400 content parts | 模型不支持多模态但发了图 | 该模型 `SupportsVision` 必须为 false（自动降级会剥掉图片） |
| 上下文超限被供应商拒 | `GOSEEK_CONTEXT_WINDOW` 配大了 | 调小为实测峰值 ×1.2，重启 |
| 压缩频繁触发 | 窗口配小了 | 调大窗口；或接受"每轮压缩一次"的成本 |
| 切了下拉里的模型就报错 | 选中了目录示例条目，不是用户自己的模型 | 按第 4 步把用户模型登记进目录再切；或只用默认模型 |
| 双击 launcher 无窗口 | WebView2 缺失（老系统） | Windows 更新装 Evergreen WebView2 Runtime，或先用 CLI 方式 |
| 对话正常但 bash 工具报错 | Windows 没有原生 bash | 装 Git for Windows（提供 bash.exe）与 ripgrep（提供 rg），重开 launcher |

### 设计边界（Agent 应向用户说明）

- **命令以当前用户身份直接执行，无沙箱**——敏感操作（删文件、支付类点击）模型会先
  描述再执行，但最终防线是用户自己；
- **压缩是分层的、可回查的**——被摘要的内容没有丢，模型可用 `conversation_history`
  取回原文；
- **token 估算被约定为上界**——实测超估算时系数自动上调，用户无感；
- **服务只监听 127.0.0.1**——不做鉴权，而它能在本机执行命令，不要试图改成 0.0.0.0。

---

## 日常使用

### 端口与多实例

- 服务默认监听 `127.0.0.1:8765`，只绑回环地址；
- 改端口：编辑 `%LOCALAPPDATA%\GoSeek\port.conf`（纯数字，如 `8877`），重启 launcher；
- 同机跑多个实例：复制一份目录，改不同的 `port.conf` 即可，互不干扰；
- **不装桌面壳直接用 CLI**：`goseek.exe serve --addr 127.0.0.1:8765`，然后浏览器开
  同地址——功能与桌面壳完全一致（桌面壳 = 自动起服务 + WebView2 窗口 + 托盘）。

### 数据都在哪

| 内容 | 位置 |
|---|---|
| 配置（key/模型/窗口） | `%LOCALAPPDATA%\goseek\config.env` |
| 会话数据库、图片、已装载插件 | `%LOCALAPPDATA%\goseek\` |
| 自己的 Skill | `%LOCALAPPDATA%\goseek\skills\<名字>\SKILL.md` |
| 服务日志（排障入口） | `%LOCALAPPDATA%\GoSeek\serve.log` |
| 端口配置 | `%LOCALAPPDATA%\GoSeek\port.conf` |

> 测试或试用时不想污染真实数据：设置 `XDG_DATA_HOME` / `XDG_CONFIG_HOME`
> 环境变量指向临时目录，GoSeek 会改用它们（内置自检脚本就是这么做的）。

### 输入框里的命令

| 命令 | 行为 |
|---|---|
| `/compact` | 立刻压缩一次上下文 |
| `/memory` | 展开或收起摘要树 |
| `/help` | 列出可用命令 |
| `//…` | 转义：去掉一个斜杠后作为普通消息发送 |

---

## 离线自检（不消耗 API 额度）

安装包自带 `mock-provider.exe`——一个假的 OpenAI-compatible 流式供应商。自检脚本会
在本地起它和被测服务，验证完整链路（会话 → 流式补全 → **工具调用真实写盘** →
观察回填 → 文本回复）后自动清理：

```powershell
# Windows
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -GoseekBin .\goseek.exe -MockBin .\mock-provider.exe
```

```bash
# macOS / Linux（源码仓库内，自动构建）
scripts/smoke-test.sh
```

自检刻意使用 **18765 / 19876** 端口——你正在运行的 GoSeek（8765）不受任何影响。

## 从源码构建

任意一台有 Go 1.26+ 与 [bun](https://bun.sh) 的机器（macOS / Linux / Windows 都行），
交叉编译出 Windows 三件套：

```bash
make release        # 产物在 backend/release/
make smoke          # （macOS/Linux）对刚构建的代码跑离线自检
make vet-win        # 用 GOOS=windows 做静态检查
```

手动等价命令：

```bash
cd front && bun install && bun run build
rm -rf backend/cmd/goseek/dist && mkdir -p backend/cmd/goseek/dist
cp -r front/dist/* backend/cmd/goseek/dist/
cd ../backend
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o cmd/goseek/plugins/browser/bin/goseek-browser.exe ./cmd/goseek-browser
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o release/goseek.exe ./cmd/goseek
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o release/GoSeek-win-launcher.exe ./cmd/goseek-win-launcher
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o release/mock-provider.exe ./cmd/mock-provider
cp cmd/goseek/plugins/browser/bin/goseek-browser.exe release/goseek-browser.exe
```

要点：

- `goseek-browser.exe` 是内置 browser 插件的 bin 工具，**必须在主程序之前、以
  Windows 为目标平台编译**并放进 `cmd/goseek/plugins/browser/bin/`——它会被
  `go:embed` 打进 goseek.exe，运行时自动解包到数据目录；
- WebView2 壳（`cmd/goseek-win-launcher`）只在 `GOOS=windows` 下编译；
- 纯 Go、无 CGO（SQLite 走 modernc 纯 Go 实现），任何平台都能交叉编译。

## 目录结构

```
backend/
  cmd/goseek/               服务本体（CLI + serve，embed 前端与插件）
  cmd/goseek-browser/       浏览器工具（截图/read-page/click/type/set-cookie）
  cmd/goseek-win-launcher/  Windows 桌面壳（Go + WebView2，托盘常驻）
  cmd/mock-provider/        假供应商（离线自检用）
  internal/
    agent/                  编排循环、压缩触发、图片登记
    contextmgr/             上下文视图 + 分层压缩器（核心）
    tool/                   内置工具 + 平台进程管理（bash / cmd 回退）
    plugin/ mcp/ browser/ store/ domain/ model/ httpapi/
front/                      Vite + React + TS 面板（embed 进服务二进制）
scripts/                    离线自检（PowerShell + bash）
```

## 与 macOS 版的关系

| | macOS 版（作者自用） | 本仓库（Windows 发行版） |
|---|---|---|
| 桌面壳 | GoSeek.app（Swift + WKWebView） | GoSeek-win-launcher.exe（Go + WebView2） |
| 模型目录 | 作者自己的 4 个网关模型 | **示例条目**，按定制章节换成你的 |
| 数据目录 | `~/.local/share/goseek` | `%LOCALAPPDATA%\goseek` |
| 分发 | 源码 + 自行构建 | 安装包（Releases）+ 源码 |

## 许可

个人项目，未附带开源许可证（默认保留所有权利）。代码与作者的上游 GoSeek 仓库同源。
