# GoSeek 前端

极简的执行过程可视化面板。用 Vite + React + TypeScript，bun 管理依赖。

## 跑起来

先起后端（另一个终端）：

```bash
cd ../backend && go run ./cmd/goseek serve
```

再起前端：

```bash
bun install
bun run dev        # http://127.0.0.1:5173
```

前端发往 `/api` 的请求由 Vite 代理到后端的 `127.0.0.1:8765`，因此浏览器眼里
所有请求都同源，不涉及 CORS。

## 代码结构

| 文件 | 作用 |
|---|---|
| `src/types.ts` | 后端 HTTP 契约在前端的镜像 |
| `src/payload.ts` | 把 `unknown` 的事件 payload 收窄成具体类型 |
| `src/api.ts` | 全部 HTTP 调用，统一错误处理 |
| `src/commands.ts` | 斜杠命令的解析（`/compact`、`/memory`、`/help`、`//` 转义） |
| `src/useEventStream.ts` | SSE 订阅，含连接生命周期管理 |
| `src/reducer.ts` | 会话状态与事件归约——快照是初始状态，每个事件 reduce 一次 |
| `src/gauge.ts` | 仪表盘的几何与分档（纯函数，可直接测） |
| `src/markdown.ts` | Markdown 解析成数据树（纯函数） |
| `src/MarkdownView.tsx` | 把那棵树渲染成 React 元素，**不碰 innerHTML** |
| `src/App.tsx` | 骨架：左侧常驻侧栏 + 右侧当前会话 |
| `src/SessionSidebar.tsx` | 左侧会话列表 |
| `src/SessionDetailPage.tsx` | 会话详情的编排：拉数据、管开关、分发命令 |
| `src/Transcript.tsx` | 对话流的渲染 |
| `src/MemoryPanel.tsx` | 摘要树：逐层展开与就地修订 |
| `src/ContextGauge.tsx` | 输入框下方的上下文仪表盘 |

## 当前能显示什么

左侧常驻会话列表；右侧是当前会话——工作目录与运行状态、对话流、输入框，输入框下方是
上下文占用仪表盘。

对话流里有：用户消息（右对齐气泡）、模型思考（默认折叠）、助手正文、工具调用卡片
（意图 + 参数 + 输出 + 结局）、压缩记录、折叠边界。

助手正文按 Markdown 渲染（标题、列表、表格、代码块带复制按钮）。解析器是自己写的，
输出 React 元素树而不是 HTML 字符串——**整个前端没有一处 `dangerouslySetInnerHTML`**。
模型回复里可以带着它刚 cat 出来的文件内容，走 innerHTML 那条路就得靠净化库一直正确；
走元素树这条，那个攻击面根本不存在。用户消息、思考、命令输出一律不渲染，保持逐字。

右上角"摘要树"打开侧面板：逐层展开摘要节点、查看任一节点覆盖的原始消息、
**就地修订活跃摘要**（改后的版本进入下一次请求，模型原始那一版仍可对照）。

审批交互要等 M5。

## 输入框里的命令

| 命令 | 行为 |
|---|---|
| `/compact` | 立刻压缩一次上下文，不必等它自动触发 |
| `/memory` | 展开或收起摘要树 |
| `/help` | 列出可用命令 |
| `//…` | 转义：去掉一个斜杠后作为普通消息发送 |

命令由**前端**解析，后端不认识它们——命令是界面的输入约定，不是协议的一部分。
不认识的 `/xxx` 就地报错，不发任何请求：敲错一个命令不该换来一次真实的模型调用。
