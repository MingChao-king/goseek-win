// 对话输入框里的斜杠命令。
//
// # 为什么解析放在前端
//
// 命令是**界面的输入约定**，不是协议的一部分。后端收到的永远是"一条用户消息"或
// "一次压缩请求"这样已经定性的东西，它不该去猜一段文字是不是命令。三个理由：
//
//   - 后端一旦开始解析 `/compact`，用户就再也无法把这六个字符**作为字面文字**发给
//     模型（比如问"`/compact` 这个命令是干嘛的"）。解析放在前端，前端可以提供
//     `//` 转义，而后端始终收到确定的东西；
//   - CLI 和面板是两个形态迥异的消费者，命令集合本来就可以不同（CLI 有 `/exit`，
//     面板没有窗口可退）。把命令塞进后端等于强迫两者统一；
//   - 命令的反馈是**即时的本地反馈**（"没有这个命令"、帮助列表），不需要往返。
//
// 不认识的 `/xxx` 就地报错，**绝不发给模型**——否则用户敲错一个命令，代价是一次
// 真实的模型调用（几秒钟加一笔费用），而且模型会一本正经地回答一个不存在的命令。

/** 命令解析的结果。调用方按 kind 分支。 */
export type ParsedInput =
  /** 作为普通消息发给模型。text 已经去掉了 `//` 转义的那个斜杠。 */
  | { kind: "message"; text: string }
  /** 一个已知命令。 */
  | { kind: "command"; name: CommandName }
  /** 形如 /xxx 但不认识。就地报错，不发请求。 */
  | { kind: "unknown"; name: string };

export interface SkillRef {
  kind: "skill";
  name: string;
  /** 用户在 skill 前缀之后输入的正文，可以为空。 */
  text: string;
}

/** 已知的命令名。 */
export type CommandName = "compact" | "memory" | "help";

/** 命令表。有意保持很小——四个命令，`/help` 够用，不做补全和历史回溯。 */
export const commands: { name: CommandName; usage: string; description: string }[] = [
  { name: "compact", usage: "/compact", description: "立刻压缩一次上下文，不必等它自动触发" },
  { name: "memory", usage: "/memory", description: "展开或收起摘要树" },
  { name: "help", usage: "/help", description: "列出可用命令" },
];

/**
 * parseInput 判断一行输入是命令还是普通消息。
 *
 * 规则（按顺序）：
 *
 *   1. `//foo` → 普通消息 `/foo`。转义，用来发送真的以斜杠开头的文字；
 *   2. `/foo`  → 命令 foo；不在命令表里就是 unknown；
 *   3. 其余    → 普通消息。
 *
 * 命令**不接受参数**：目前四个命令都不需要。多出来的东西一律当作不认识，
 * 而不是安静忽略——`/compact 全部` 安静地压了一次，用户会以为"全部"起了作用。
 */
export function parseInput(raw: string): ParsedInput {
  const text = raw.trim();

  // 转义要先判断，否则 `//` 会被当成一个名字为空的命令。
  if (text.startsWith("//")) {
    return { kind: "message", text: text.slice(1) };
  }
  if (!text.startsWith("/")) {
    return { kind: "message", text };
  }

  // 去掉斜杠之后的部分。带参数（含空格）时整段当作命令名，于是落进 unknown ——
  // 那正是我们想要的反馈："没有这个命令"，而不是忽略参数照常执行。
  const name = text.slice(1);
  if (commands.some((command) => command.name === name)) {
    return { kind: "command", name: name as CommandName };
  }
  return { kind: "unknown", name };
}

/**
 * parseSkillInput 识别 "/skill:name 要说的话" 格式。
 *
 * 和普通命令不同，skill 带参数是合法的——参数就是用户想让模型在这个 skill
 * 指导下做的事。没有参数也可以发送：模型会加载 skill 然后等待下一步。
 */
export function parseSkillInput(raw: string): SkillRef | null {
  const text = raw.trim();
  if (!text.startsWith("/skill:")) return null;
  const rest = text.slice("/skill:".length);
  const spaceIndex = rest.indexOf(" ");
  if (spaceIndex === -1) {
    return { kind: "skill", name: rest, text: "" };
  }
  return { kind: "skill", name: rest.slice(0, spaceIndex), text: rest.slice(spaceIndex + 1).trim() };
}

/** helpText 是 /help 的输出。 */
export function helpText(): string {
  const lines = commands.map((command) => `  ${command.usage.padEnd(10)} ${command.description}`);
  return ["可用命令：", ...lines, "  //…       以斜杠开头的普通消息，用两个斜杠转义"].join("\n");
}
