// 把 markdown.ts 解析出来的数据树渲染成 React 元素。
//
// **整个文件里没有 dangerouslySetInnerHTML，也不会有。** 这是这套做法的全部意义：
// 文字一律作为 React 的文本节点输出，而文本节点天生转义——模型回复里就算带着
// `<img onerror=…>`，它也只会显示成那几个字符，不可能变成一个标签。
//
// 这不是"过滤掉了危险内容"，是**根本没有把内容当成 HTML 执行过**。

import { CopyButton } from "./CopyButton";
import { parseMarkdown } from "./markdown";
import type { Block, Span } from "./markdown";

// 文件名不叫 Markdown.tsx：macOS 的文件系统不区分大小写，它会和解析器
// markdown.ts 撞在一起（TS 会直接报 "differs only in casing"）。

/** MarkdownView 渲染一段模型输出。 */
export function MarkdownView({
  text,
  onFileLinkClick,
}: {
  text: string;
  /** file:// 链接被点击时回调（参数是链接目标路径）。不传则按普通链接打开。 */
  onFileLinkClick?: (path: string) => void;
}) {
  // 每次渲染都重新解析。流式输出时正文一直在变，缓存的收益是负的——
  // 一段几千字的回复解析一次是微秒级，而 memo 的比较开销和失效逻辑不是。
  const blocks = parseMarkdown(text);
  return <div className="markdown">{blocks.map((b, i) => renderBlock(b, i, onFileLinkClick))}</div>;
}

/** renderBlock 渲染一个块级元素。 */
function renderBlock(block: Block, index: number, onFileLinkClick?: (path: string) => void) {
  switch (block.kind) {
    case "heading": {
      // 标题层级映射到 h2–h6：h1 留给页面本身，模型回复里的 # 从 h2 起，
      // 免得一段回复在文档结构上盖过整个页面。
      const Tag = `h${Math.min(block.level + 1, 6)}` as "h2";
      return <Tag key={index}>{block.spans.map((s, spanIdx) => renderSpan(s, spanIdx, onFileLinkClick))}</Tag>;
    }

    case "paragraph":
      return <p key={index}>{block.spans.map((s, spanIdx) => renderSpan(s, spanIdx, onFileLinkClick))}</p>;

    case "code":
      return <CodeBlock key={index} language={block.language} code={block.code} />;

    case "list": {
      const items = block.items.map((item, itemIndex) => (
        // depth 只用来做视觉缩进：AST 是平的，不生成嵌套 <ul>（见 markdown.ts）。
        <li key={itemIndex} style={item.depth > 0 ? { marginLeft: `${item.depth * 1.2}rem` } : undefined}>
          {item.spans.map((s, spanIdx) => renderSpan(s, spanIdx, onFileLinkClick))}
        </li>
      ));
      return block.ordered ? (
        <ol key={index} start={block.start}>{items}</ol>
      ) : (
        <ul key={index}>{items}</ul>
      );
    }

    case "quote":
      return <blockquote key={index}>{block.blocks.map((b, i) => renderBlock(b, i, onFileLinkClick))}</blockquote>;

    case "rule":
      return <hr key={index} />;

    case "table":
      return (
        // 宽表格在自己的容器里横向滚动，不让整个页面跟着横滚。
        <div key={index} className="table-scroll">
          <table>
            <thead>
              <tr>
                {block.header.map((cell, cellIndex) => (
                  <th key={cellIndex} style={{ textAlign: block.align[cellIndex] ?? "left" }}>
                    {cell.map((s, i) => renderSpan(s, i, onFileLinkClick))}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {block.rows.map((row, rowIndex) => (
                <tr key={rowIndex}>
                  {row.map((cell, cellIndex) => (
                    <td key={cellIndex} style={{ textAlign: block.align[cellIndex] ?? "left" }}>
                      {cell.map((s, i) => renderSpan(s, i, onFileLinkClick))}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
  }
}

/** renderSpan 渲染一个行内元素。 */
function renderSpan(span: Span, index: number, onFileLinkClick?: (path: string) => void): React.ReactNode {
  switch (span.kind) {
    case "text":
      // 就是这里：一个普通的 React 文本节点，浏览器不会把它当标签解析。
      return <span key={index}>{span.text}</span>;
    case "code":
      return <code key={index}>{span.text}</code>;
    case "strong":
      return <strong key={index}>{span.spans.map((s, i) => renderSpan(s, i, onFileLinkClick))}</strong>;
    case "em":
      return <em key={index}>{span.spans.map((s, i) => renderSpan(s, i, onFileLinkClick))}</em>;
    case "strike":
      return <del key={index}>{span.spans.map((s, i) => renderSpan(s, i, onFileLinkClick))}</del>;
    case "link":
      // file:// 链接 → 侧栏文件查看（不跳走）。
      if (onFileLinkClick && span.href?.startsWith("file://")) {
        const filePath = decodeURIComponent(span.href.slice("file://".length));
        return (
          <a
            key={index}
            href="#"
            onClick={(e) => {
              e.preventDefault();
              onFileLinkClick(filePath);
            }}
            className="file-link"
            title={`在侧栏查看 ${filePath}`}
          >
            {span.spans.map((s, i) => renderSpan(s, i, onFileLinkClick))}
          </a>
        );
      }
      // 协议已经在 safeHref 里过过一遍。target=_blank 必须配 noreferrer noopener：
      // 没有它，被打开的页面能通过 window.opener 反过来操作这一页。
      return (
        <a key={index} href={span.href} target="_blank" rel="noreferrer noopener">
          {span.spans.map((s, i) => renderSpan(s, i, onFileLinkClick))}
        </a>
      );
  }
}

/** CodeBlock 是带语言标记和复制按钮的代码块。 */
function CodeBlock({ language, code }: { language: string; code: string }) {
  return (
    <div className="code-block">
      <div className="code-block-head">
        <span className="code-lang">{language || "text"}</span>
        <CopyButton text={code} className="link icon-btn" />
      </div>
      <pre>
        <code>{code}</code>
      </pre>
    </div>
  );
}
