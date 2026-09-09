import { expect, test } from "bun:test";
import { parseInline, parseMarkdown, safeHref } from "./markdown";
import type { Block, Span } from "./markdown";

/** text 把 span 树压成纯文字，便于断言。 */
function text(spans: Span[]): string {
  return spans
    .map((span) => {
      switch (span.kind) {
        case "text":
        case "code":
          return span.text;
        default:
          return text(span.spans);
      }
    })
    .join("");
}

function kinds(blocks: Block[]): string[] {
  return blocks.map((block) => block.kind);
}

// —— 块级 ——

test("标题按 # 的个数定级", () => {
  const blocks = parseMarkdown("# 一级\n\n### 三级");
  expect(kinds(blocks)).toEqual(["heading", "heading"]);
  expect(blocks[0]).toMatchObject({ kind: "heading", level: 1 });
  expect(blocks[1]).toMatchObject({ kind: "heading", level: 3 });
  expect(text((blocks[0] as { spans: Span[] }).spans)).toBe("一级");
});

test("井号后面没有空格不是标题", () => {
  expect(kinds(parseMarkdown("#不是标题"))).toEqual(["paragraph"]);
});

test("围栏代码块带语言标记，里面的内容原样保留", () => {
  const blocks = parseMarkdown("```go\nfunc main() {\n\t// # 这不是标题\n}\n```");
  expect(blocks).toHaveLength(1);
  expect(blocks[0]).toMatchObject({
    kind: "code",
    language: "go",
    code: "func main() {\n\t// # 这不是标题\n}",
    closed: true,
  });
});

// 流式输出到一半时必然出现未闭合的围栏。它要照常渲染成代码块——否则写完最后
// 三个反引号的瞬间，整段会从段落跳变成代码块，读的人会看到画面抖一下。
test("未闭合的围栏仍然是代码块，并标记为未闭合", () => {
  const blocks = parseMarkdown("正文\n\n```go\nfunc main() {");
  expect(kinds(blocks)).toEqual(["paragraph", "code"]);
  expect(blocks[1]).toMatchObject({ kind: "code", code: "func main() {", closed: false });
});

test("无序列表与有序列表", () => {
  const unordered = parseMarkdown("- 甲\n- 乙\n- 丙");
  expect(unordered[0]).toMatchObject({ kind: "list", ordered: false });
  expect((unordered[0] as { items: { spans: Span[] }[] }).items).toHaveLength(3);

  const ordered = parseMarkdown("3. 甲\n4. 乙");
  expect(ordered[0]).toMatchObject({ kind: "list", ordered: true, start: 3 });
});

test("缩进的列表项记下层级", () => {
  const blocks = parseMarkdown("- 顶层\n  - 第二层\n    - 第三层");
  const items = (blocks[0] as { items: { depth: number }[] }).items;
  expect(items.map((item) => item.depth)).toEqual([0, 1, 2]);
});

test("水平线不会被当成列表", () => {
  expect(kinds(parseMarkdown("---"))).toEqual(["rule"]);
  expect(kinds(parseMarkdown("***"))).toEqual(["rule"]);
});

test("引用里可以嵌别的块", () => {
  const blocks = parseMarkdown("> ## 引用里的标题\n> - 引用里的列表");
  expect(blocks[0].kind).toBe("quote");
  expect(kinds((blocks[0] as { blocks: Block[] }).blocks)).toEqual(["heading", "list"]);
});

test("表格连同对齐一起解析", () => {
  const blocks = parseMarkdown("| 名称 | 值 |\n|:---|---:|\n| a | 1 |\n| b | 2 |");
  expect(blocks[0]).toMatchObject({ kind: "table", align: ["left", "right"] });
  const table = blocks[0] as { header: Span[][]; rows: Span[][][] };
  expect(table.header.map(text)).toEqual(["名称", "值"]);
  expect(table.rows).toHaveLength(2);
  expect(table.rows[1].map(text)).toEqual(["b", "2"]);
});

test("段落之间靠空行分隔，段内换行保留", () => {
  const blocks = parseMarkdown("第一段第一行\n第一段第二行\n\n第二段");
  expect(kinds(blocks)).toEqual(["paragraph", "paragraph"]);
  expect(text((blocks[0] as { spans: Span[] }).spans)).toBe("第一段第一行\n第一段第二行");
});

// —— 行内 ——

test("粗体、斜体、删除线、行内代码", () => {
  expect(parseInline("**粗**")[0]).toMatchObject({ kind: "strong" });
  expect(parseInline("*斜*")[0]).toMatchObject({ kind: "em" });
  expect(parseInline("~~删~~")[0]).toMatchObject({ kind: "strike" });
  expect(parseInline("`码`")[0]).toMatchObject({ kind: "code", text: "码" });
});

// 反引号里的东西一律原样，否则一段 C 代码里的 **ptr 会被当成粗体开头。
test("行内代码优先级最高", () => {
  const spans = parseInline("看 `**ptr` 这个写法");
  expect(spans[1]).toMatchObject({ kind: "code", text: "**ptr" });
  expect(spans.some((span) => span.kind === "strong")).toBe(false);
});

test("粗体优先于斜体，不会被拆成两个空斜体", () => {
  const spans = parseInline("**加粗**");
  expect(spans).toHaveLength(1);
  expect(spans[0]).toMatchObject({ kind: "strong" });
});

test("孤立的星号当乘号，不当强调", () => {
  const spans = parseInline("a * b * c");
  expect(spans.every((span) => span.kind === "text")).toBe(true);
});

test("反斜杠转义", () => {
  expect(text(parseInline("\\*不是斜体\\*"))).toBe("*不是斜体*");
});

test("强调可以嵌套", () => {
  const spans = parseInline("**粗里带`码`**");
  expect(spans[0].kind).toBe("strong");
  const inner = (spans[0] as { spans: Span[] }).spans;
  expect(inner.some((span) => span.kind === "code")).toBe(true);
});

// —— 链接与安全 ——

test("正常链接被解析", () => {
  const spans = parseInline("见 [文档](https://example.com/a)");
  expect(spans[1]).toMatchObject({ kind: "link", href: "https://example.com/a" });
});

// 这是整个解析器唯一的安全判断：javascript: 是唯一一个点一下就执行代码的协议。
test("javascript: 链接降级成纯文字，而不是被吞掉", () => {
  const spans = parseInline("[点我](javascript:alert(1))");
  expect(spans.every((span) => span.kind === "text")).toBe(true);
  expect(text(spans)).toBe("[点我](javascript:alert(1))");
});

test("协议白名单", () => {
  expect(safeHref("https://a.com")).toBe("https://a.com");
  expect(safeHref("http://a.com")).toBe("http://a.com");
  expect(safeHref("mailto:a@b.com")).toBe("mailto:a@b.com");
  expect(safeHref("/相对路径")).toBe("/相对路径");
  expect(safeHref("javascript:alert(1)")).toBeNull();
  expect(safeHref("data:text/html,<script>")).toBeNull();
});

// 浏览器在解析 href 之前会先剥掉制表符、换行和回车（HTML 标准要求），
// 所以这些写法在浏览器眼里都是 javascript:。按原样做前缀匹配会当成"没有协议"
// 的相对链接放行——这一条最初真的漏了，是被这条测试抓出来的。
test("靠控制字符绕过协议检查的老把戏", () => {
  for (const attempt of [
    "java\tscript:alert(1)",
    "java\nscript:alert(1)",
    "java\rscript:alert(1)",
    "\u0000javascript:alert(1)",
    "  javascript:alert(1)",
    "JaVaScRiPt:alert(1)",
  ]) {
    expect(safeHref(attempt)).toBeNull();
  }
});

// —— 真实输出 ——

// 拿一段真实的模型回复过一遍，确认块的顺序是对的。
test("真实回复的块序列", () => {
  const blocks = parseMarkdown(
    [
      "正如你看到的，我上下文顶部挂着这个节点：",
      "",
      "```",
      "[较早对话的摘要 batch_id=mem_17c1 level=0 覆盖第 1 到 38 条消息]",
      "```",
      "",
      "刚才 inspect 的结果证实了它：",
      "",
      "- **层级 0，叶子节点**——说明只压了一层；",
      "- **覆盖第 1 到 38 条消息**。",
      "",
      "## 一个有意思的差异",
      "",
      "仓库的设计是**分层合并**：叶子→父节点。",
    ].join("\n"),
  );
  expect(kinds(blocks)).toEqual([
    "paragraph", "code", "paragraph", "list", "heading", "paragraph",
  ]);
});
