// 与后端交互的全部 HTTP 调用集中在这里。
//
// 集中的好处是错误处理只写一遍：后端的错误统一是 {"error":{"code","message"}}，
// 由 request 一处翻译成 JS 的异常，组件里就只需要 try/catch，不必每处都判 res.ok。

import type { MemoryTree, ModelInfo, SessionSnapshot, SessionSummary } from "./types";

/** ApiFailure 是一次失败的请求。 */
export class ApiFailure extends Error {
  /** code 是后端给的稳定错误标识，按它分支而不是按 message 的文案。 */
  readonly code: string;

  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}

/**
 * request 发一个请求并把 JSON 解出来。
 *
 * 请求路径都以 /api 开头，由 Vite 的 proxy 转发到后端（见 vite.config.ts）——
 * 对浏览器来说它们和页面同源，因此不涉及 CORS。
 */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });

  if (!response.ok) {
    // 错误响应**应当**是 JSON，但一个挂掉的代理可能返回 HTML。解析失败时退回
    // 状态码，总比抛一个 "Unexpected token <" 强。
    let code = "http_" + response.status;
    let message = `请求失败（HTTP ${response.status}）`;
    try {
      const body = await response.json();
      if (body?.error?.code) {
        code = body.error.code;
        message = body.error.message ?? message;
      }
    } catch {
      // 保持上面的默认值。
    }
    throw new ApiFailure(code, message);
  }

  return (await response.json()) as T;
}

/**
 * listSessions 取回会话列表，按最后活动倒序。
 *
 * 默认只给未归档的——归档的意思就是"从眼前拿走"，要显式索取才会出现。
 */
export function listSessions(includeArchived = false): Promise<{ sessions: SessionSummary[] }> {
  return request(`/api/v1/sessions${includeArchived ? "?include_archived=1" : ""}`);
}

/**
 * updateSession 归档、取消归档或重命名。
 *
 * 两个字段都是可选的，而且 `archived: false` 和 `title: ""` 都是**有意义的取值**
 * （取消归档 / 恢复派生标题），所以这里必须用 undefined 而不是零值表示"不改"。
 */
export function updateSession(
  id: string,
  changes: { archived?: boolean; title?: string; model?: string },
): Promise<unknown> {
  return request(`/api/v1/sessions/${id}`, {
    method: "PATCH",
    body: JSON.stringify(changes),
  });
}

/**
 * deleteSession 彻底删除一个会话，连同它的消息、事件、摘要。
 *
 * **不可逆。** 调用方必须先让用户确认，而且确认框里要显示会话标题和消息条数——
 * 只问"确定吗"等于没问。
 */
export function deleteSession(id: string): Promise<unknown> {
  return request(`/api/v1/sessions/${id}`, { method: "DELETE" });
}

/**
 * cancelTurn 中止当前这一轮。
 *
 * 它**不关闭会话**——只中止这一轮，之后还能接着聊。没有正在跑的一轮时后端返回
 * 409 `no_turn_running`。
 */
export function cancelTurn(id: string): Promise<unknown> {
  return request(`/api/v1/sessions/${id}/turns/cancel`, { method: "POST" });
}

/** createSession 新建一个会话。workspace 留空时由后端用它的当前目录。 */
export function createSession(workspace?: string, model?: string): Promise<SessionSummary> {
  return request("/api/v1/sessions", {
    method: "POST",
    body: JSON.stringify({ workspace: workspace ?? "", model: model ?? "" }),
  });
}

/** listModels 取回 GoSeek 支持的模型目录。 */
export function listModels(): Promise<{ models: ModelInfo[] }> {
  return request("/api/v1/models");
}

/** getDefaultWorkspace 取服务端默认工作区，供新建会话弹窗预填。 */
export function getDefaultWorkspace(): Promise<{ path: string }> {
  return request("/api/v1/workspace/default");
}

/** getSession 取回一个会话的完整快照。 */
export function getSession(id: string): Promise<SessionSnapshot> {
  return request(`/api/v1/sessions/${id}`);
}

/**
 * compactSession 请求立刻压缩一次上下文（面板上的 /compact）。
 *
 * 和 submitTurn 一样在 202 时就 resolve——压缩要跑几十秒，过程和结果通过事件流
 * 观察。没有请求体：压缩没有参数，范围由轮次边界和保留区决定。
 *
 * 一轮正在跑时后端返回 409 `turn_in_progress`——压缩改的是下一次请求要用的视图，
 * 和一轮交互并发做结果无从定义。调用方按 `code` 分支给出可操作的提示。
 */
export function compactSession(id: string): Promise<unknown> {
  return request(`/api/v1/sessions/${id}/compact`, { method: "POST" });
}

/**
 * getMemory 取回完整的摘要树。
 *
 * 只在用户主动展开记忆面板时调用：节点正文可能上千字，不该塞进每次快照。
 * 它走后端的只读路径，因此一轮正在跑时也能立刻返回。
 */
export function getMemory(id: string): Promise<MemoryTree> {
  return request(`/api/v1/sessions/${id}/memory`);
}

/**
 * editMemory 把一段活跃摘要换成人工修订版，返回修订之后的整棵树。
 *
 * 它是唯一一个**等到做完才返回结果**的写操作：只是一次校验加一次 UPDATE，
 * 毫秒级，而调用方需要立刻知道改没改成。提交消息和压缩要跑几十秒，那两个返回 202、
 * 结果走事件流。
 *
 * `content` 传空串表示**撤销修订**，回到模型原始生成的那一版。
 *
 * 两种 409 要分开处理（按 code，不要匹配文案）：
 *   - `turn_in_progress`：一轮在跑，它也写 session.Memory，不能并发；
 *   - `batch_not_active`：这个节点已经被合并进上层，改了模型也看不到。
 */
export function editMemory(
  id: string,
  batchID: string,
  content: string,
): Promise<MemoryTree> {
  return request(`/api/v1/sessions/${id}/memory/${batchID}`, {
    method: "PATCH",
    body: JSON.stringify({ content }),
  });
}

/**
 * submitTurn 提交一条用户消息。
 *
 * 它在后端返回 202 时就 resolve——那只表示这一轮**开始了**，不表示跑完了。
 * 过程和结果通过事件流观察，因此这里没有返回值可用。
 *
 * 一轮正在跑时提交**不会被拒**：消息进队列，在下一次请求模型之前被注入到正在进行
 * 的那一轮里。所以调用方不需要先检查忙不忙。
 */
export interface SidebarContextItem {
  kind: "page" | "file";
  title?: string;
  url?: string;
  path?: string;
}

export function submitTurn(
  id: string,
  content: string,
  imageIds?: string[],
  sidebarItems?: SidebarContextItem[],
): Promise<unknown> {
  return request(`/api/v1/sessions/${id}/turns`, {
    method: "POST",
    body: JSON.stringify({ content, image_ids: imageIds ?? [], sidebar_items: sidebarItems }),
  });
}

/** ImageUpload 是图片上传成功的返回。 */
export interface ImageUpload {
  id: string;
  media_type: string;
  byte_size: number;
  width: number;
  height: number;
}

/**
 * uploadImage 上传一张图片到指定会话。
 * 返回的 ID 供后续 submitTurn 的 image_ids 使用。
 */
export function uploadImage(id: string, file: File): Promise<ImageUpload> {
  const body = new FormData();
  body.append("image", file);
  return fetch(`/api/v1/sessions/${id}/images`, { method: "POST", body }).then(
    (response) => {
      if (!response.ok) {
        return response.json().then((data) => {
          throw new ApiFailure(data?.error?.code ?? "upload_failed", data?.error?.message ?? "上传失败");
        });
      }
      return response.json() as Promise<ImageUpload>;
    },
  );
}

/** SkillSummary 是一个 skill 的名称和描述。 */
export interface SkillSummary {
  name: string;
  description: string;
  /** 来源：user（用户导入）或插件名。插件 skill 只读。 */
  source: string;
}

/** SkillDetail 是一个 skill 的完整正文。 */
export interface SkillDetail {
  name: string;
  content: string;
}

/** listSkills 返回所有已导入的 skill。 */
export function listSkills(): Promise<{ skills: SkillSummary[] }> {
  return request("/api/v1/skills");
}

/** getSkill 返回一个 skill 的完整正文。 */
export function getSkill(name: string): Promise<SkillDetail> {
  return request(`/api/v1/skills/${encodeURIComponent(name)}`);
}

/** uploadSkill 导入（或覆盖）一个 skill。 */
export function uploadSkill(name: string, content: string): Promise<unknown> {
  return request(`/api/v1/skills/${encodeURIComponent(name)}`, {
    method: "POST",
    body: JSON.stringify({ content }),
  });
}

/** deleteSkill 删除一个 skill。 */
export function deleteSkill(name: string): Promise<unknown> {
  return request(`/api/v1/skills/${encodeURIComponent(name)}`, {
    method: "DELETE",
  });
}

/** PluginInfo 是一个已安装插件的完整信息。 */
export interface PluginInfo {
  name: string;
  display_name: string;
  version: string;
  description: string;
  author: string;
  enabled: boolean;
  skills: string[];
  commands: string[];
  has_bin: boolean;
}

/** listPlugins 返回全部已安装插件。 */
export function listPlugins(): Promise<{ plugins: PluginInfo[] }> {
  return request("/api/v1/plugins");
}

/** togglePlugin 启用或禁用插件。 */
export function togglePlugin(name: string, enabled: boolean): Promise<unknown> {
  return request(`/api/v1/plugins/${encodeURIComponent(name)}`, {
    method: "PATCH",
    body: JSON.stringify({ enabled }),
  });
}

/** deletePlugin 卸载插件。 */
export function deletePlugin(name: string): Promise<unknown> {
  return request(`/api/v1/plugins/${encodeURIComponent(name)}`, { method: "DELETE" });
}

/** MCPServerConfig 是一个 MCP server 的连接配置。 */
export interface MCPServerConfig {
  name: string;
  command: string;
  args?: string[];
  env?: Record<string, string>;
}

/** listMCP 返回当前配置的 MCP server 列表。 */
export function listMCP(): Promise<{ servers: MCPServerConfig[] }> {
  return request("/api/v1/mcp");
}

/** saveMCP 整体保存 MCP 配置。 */
export function saveMCP(servers: MCPServerConfig[]): Promise<{ servers: MCPServerConfig[] }> {
  return request("/api/v1/mcp", {
    method: "POST",
    body: JSON.stringify({ servers }),
  });
}

/** deleteMCP 删除一个 MCP server 配置。 */
export function deleteMCP(name: string): Promise<{ servers: MCPServerConfig[] }> {
  return request(`/api/v1/mcp/${encodeURIComponent(name)}`, { method: "DELETE" });
}

/** BrowserTab 是侧栏可见的一个浏览器页面。 */
export interface BrowserTab {
  id: string;
  url: string;
  title: string;
}

/** browserTabs 轮询当前会话的浏览器页面列表。 */
export function browserTabs(sessionID: string): Promise<{ tabs: BrowserTab[] }> {
  return request(`/api/v1/browser/tabs?sessionId=${encodeURIComponent(sessionID)}`);
}

/** browserScreenshotURL 构造当前画面的截图地址（img src 用，t 用于破缓存）。 */
export function browserScreenshotURL(sessionID: string): string {
  return `/api/v1/browser/screenshot?sessionId=${encodeURIComponent(sessionID)}&t=${Date.now()}`;
}

export function browserStreamURL(sessionID: string, targetId: string, nonce = 0): string {
  return `/api/v1/browser/stream?sessionId=${encodeURIComponent(sessionID)}&targetId=${encodeURIComponent(targetId)}&v=${nonce}`;
}

/** browserClick 把侧栏画面上的点击转发给浏览器（坐标已换算成视口坐标）。 */
export function browserClick(sessionID: string, targetId: string, x: number, y: number): Promise<unknown> {
  return request(`/api/v1/browser/click?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ targetId, x, y }),
  });
}

/** browserScroll 转发滚轮。 */
export function browserScroll(
  sessionID: string,
  targetId: string,
  x: number,
  y: number,
  deltaY: number,
): Promise<unknown> {
  return request(`/api/v1/browser/scroll?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ targetId, x, y, deltaY }),
  });
}

export function browserSetViewport(
  sessionID: string,
  targetId: string,
  width: number,
  height: number,
  scale = 1,
  fit = false,
): Promise<BrowserViewport> {
  return request(`/api/v1/browser/viewport?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ targetId, width, height, scale, fit }),
  });
}

export interface BrowserViewport {
  width: number;
  height: number;
  scale: number;
  fit: boolean;
}

export function browserInsertText(sessionID: string, targetId: string, text: string): Promise<unknown> {
  return request(`/api/v1/browser/text?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ targetId, text }),
  });
}

export function browserPressKey(sessionID: string, targetId: string, key: string): Promise<unknown> {
  return request(`/api/v1/browser/key?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ targetId, key }),
  });
}

/** browserNavigate 新开页面并返回后端创建的权威 target ID。 */
export function browserNavigate(
  sessionID: string,
  url: string,
  waitSeconds = 3,
): Promise<{ url: string; targetId: string }> {
  return request(`/api/v1/browser/navigate?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ url, waitSeconds }),
  });
}

/** browserActivateTab 让某个页面成为当前页面（截图流跟着它走）。 */
export function browserActivateTab(sessionID: string, targetId: string): Promise<unknown> {
  return request(`/api/v1/browser/tabs/activate?sessionId=${encodeURIComponent(sessionID)}`, {
    method: "POST",
    body: JSON.stringify({ targetId }),
  });
}

/** browserCloseTab 关闭当前会话中的真实浏览器页面并返回最新页面列表。 */
export function browserCloseTab(sessionID: string, targetId: string): Promise<{ tabs: BrowserTab[] }> {
  return request(
    `/api/v1/browser/tabs/${encodeURIComponent(targetId)}?sessionId=${encodeURIComponent(sessionID)}`,
    { method: "DELETE" },
  );
}
