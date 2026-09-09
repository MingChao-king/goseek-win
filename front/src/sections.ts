// 摘要正文的七节结构。
//
// 压缩提示词要求模型按固定七节输出（见后端 contextmgr/summarize.go）。界面按节
// 分栏展示、逐节编辑，用户改"未完成 / 阻塞"时不必在一大段文字里找那一段。
//
// # 为什么解析而不是让后端给结构化字段
//
// 因为**七节是提示词里的约定，不是协议**。模型可能少写一节、多写一段、或者完全
// 不按格式来——那时后端要么报错（一次压缩就白做了），要么塞一堆空字段。而这里
// 解析失败只影响展示：降级成"未分节"，正文一个字不改地整段显示出来，用户仍然
// 能读、能编辑。
//
// 这也是"不做小节缺失的机械校验"那条决定在前端的落点：校验是给模型能力打补丁，
// 而且会掩盖"这个模型不够用"这个真信号。这里只是**如实展示模型写了什么**。

/** 七节的节名，顺序即渲染顺序。 */
export const sectionNames = [
  "主题",
  "目标与约束",
  "关键概念与决定",
  "涉及的文件与代码位置",
  "做成了什么",
  "失败与不该重走的路",
  "未完成 / 阻塞",
] as const;

/** 摘要正文解析出来的一节。 */
export interface Section {
  /** 节名。不在 sectionNames 里的是模型自己起的，照样展示。 */
  name: string;
  /** 这一节的正文，已去掉首尾空白。 */
  body: string;
}

/**
 * parseSections 把摘要正文拆成若干节。
 *
 * 识别的形式是行首的"节名：""节名:"，可以带序号前缀（"1. 主题："）——模型两种都
 * 会写，而这个差别对用户没有意义。
 *
 * **第一行必须是七节里认得的节名**，否则整段降级。理由是纯语法上分不清"节标题"
 * 和"一句以冒号结尾的话"（"好的，下面是摘要："）。用第一行锚定格式最省事也最好
 * 解释：合格式的摘要一定以"主题："开头，那是提示词的第 1 条。锚定之后，后面的
 * 节名可以是模型自己起的——那时已经确定这确实是一份分了节的摘要。
 *
 * **完全不符合格式时返回空数组**，调用方据此降级成整段展示。返回一个"名字为空的
 * 单节"看起来更统一，但那会让调用方分不清"模型没分节"和"模型写了一节叫空字符串"。
 */
export function parseSections(content: string): Section[] {
  const lines = content.split("\n");
  const sections: Section[] = [];
  let current: Section | null = null;

  for (const line of lines) {
    const heading = headingOf(line);
    // 第一行必须是认得的节名，否则这根本不是一份分节摘要。
    if (!current && (!heading || !isKnownSection(heading.name))) return [];
    if (heading) {
      if (current) sections.push(finish(current));
      current = { name: heading.name, body: heading.rest };
      continue;
    }
    current!.body += "\n" + line;
  }
  if (current) sections.push(finish(current));
  return sections;
}

/** finish 收尾一节，去掉正文首尾的空白。 */
function finish(section: Section): Section {
  return { name: section.name, body: section.body.trim() };
}

/**
 * headingOf 判断一行是不是节标题，是则返回节名和同行的剩余正文。
 *
 * 限制节名长度（24 字）是为了不把正文里的普通句子误判成标题——中文正文里冒号很
 * 常见（"结论：这条路走不通"）。真正的节名都很短，而正文句子往往更长。
 */
function headingOf(line: string): { name: string; rest: string } | null {
  const match = /^\s*(?:\d+[.、)]\s*)?([^：:]{1,24})[：:](.*)$/.exec(line);
  if (!match) return null;
  const name = match[1].trim();
  if (!name) return null;
  return { name, rest: match[2].trim() };
}

/**
 * serializeSections 把若干节拼回摘要正文。
 *
 * 它和 parseSections 互为逆运算：解析 → 改一节 → 序列化 → 再解析，得到的结构和
 * 改动一致。这条性质是逐节编辑能成立的前提——后端的 PATCH 接口收的是**整段正文**，
 * 界面上改一节，实际发出去的是重新拼出来的全文。往返不一致就意味着"改 A 节顺手
 * 改坏了 B 节"。
 *
 * 空节整个跳过：模型没写的东西不该因为界面渲染过一次就变成一个空标题。
 */
export function serializeSections(sections: Section[]): string {
  return sections
    .filter((section) => section.body.trim() !== "")
    .map((section) => `${section.name}：${section.body.trim()}`)
    .join("\n\n");
}

/** isKnownSection 判断一个节名是不是提示词里约定的那七个。 */
function isKnownSection(name: string): boolean {
  return (sectionNames as readonly string[]).includes(name);
}
