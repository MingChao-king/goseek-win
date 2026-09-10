// 侧栏文件查看器：按文件类型分流渲染。
//
// 之前所有文件一律塞进一个 <pre>——zip 显示成乱码、读不到显示"无法读取文件：
// Not Found"、markdown 和代码都没有格式。这里把"文件"分成三类，各自给对应的
// 呈现：
//
//   文本（md）      → MarkdownView 渲染（标题/列表/代码块/表格，与对话一致）
//   文本（代码/其它）→ <code> + 轻量语法着色（注释/字符串/关键字三类，见 highlight）
//   无法预览        → 文件卡片：图标 + 大小 + "在文件管理器中显示" + "复制路径"
//
// 文件卡片同时是"读不到"时的兜底：文件可能已被删除、还没生成、或超过 1MB——
// 此时卡片给出绝对路径和定位按钮，用户至少能自己去目录看一眼。这比一句
// Not Found 有用得多，也是这个组件存在的理由。

import { useCallback, useMemo, useState } from "react";
import { revealFile } from "./api";
import { Icon } from "./Icon";
import { CopyButton } from "./CopyButton";
import { MarkdownView } from "./MarkdownView";

/** 服务器返回的文件查看载荷（fileViewPayload 的前端镜像）。 */
export interface FilePayload {
  path: string;
  abs_path: string;
  binary: boolean;
  size: number;
  modified_at?: string;
  not_found?: boolean;
  content?: string;
}

/** markdown 扩展名：渲染成排版视图而不是代码。 */
const markdownExtensions = new Set(["md", "markdown", "mdx"]);

/** extension 取文件名的小写扩展名；无扩展名返回空串。 */
function extension(path: string): string {
  const name = path.split("/").pop() ?? path;
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
}

/** formatSize 把字节数压成 1.2 MB / 340 KB 这样的可扫读形态。 */
function formatSize(bytes: number): string {
  if (bytes < 0) return "未知大小";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * highlight 把一行代码拆成着色片段。
 *
 * 刻意用"每行三种 token"而不是真正的词法分析：侧栏是阅读视图不是编辑器，
 * 注释/字符串/关键字三色已经覆盖了"扫一眼分清结构"的需求，而一个完整的
 * 高亮库（几十 KB + 引入注入面）不值得。返回 [{text, kind}]，kind 决定样式类。
 */
function highlightLine(line: string, language: string): Array<{ text: string; kind: string }> {
  // 纯注释行：整行一种色。
  const trimmed = line.trimStart();
  if (
    (language !== "python" && (trimmed.startsWith("//") || trimmed.startsWith("/*") || trimmed.startsWith("*"))) ||
    trimmed.startsWith("#") ||
    trimmed.startsWith("--")
  ) {
    return [{ text: line, kind: "tok-comment" }];
  }

  const tokens: Array<{ text: string; kind: string }> = [];
  // 扫描指针：逐段找出字符串与行内注释，剩下的部分再按关键字切。
  let rest = line;
  while (rest.length > 0) {
    const stringMatch = rest.match(/^("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`(?:[^`\\]|\\.)*`)/);
    if (stringMatch) {
      tokens.push({ text: stringMatch[0], kind: "tok-string" });
      rest = rest.slice(stringMatch[0].length);
      continue;
    }
    const commentMatch = rest.match(/(\/\/.*$|#.*$)/);
    if (commentMatch && commentMatch.index !== undefined && commentMatch.index >= 0) {
      if (commentMatch.index > 0) tokens.push({ text: rest.slice(0, commentMatch.index), kind: "" });
      tokens.push({ text: rest.slice(commentMatch.index), kind: "tok-comment" });
      rest = "";
      continue;
    }
    const wordMatch = rest.match(/^[A-Za-z_][A-Za-z0-9_]*/);
    if (wordMatch) {
      const word = wordMatch[0];
      tokens.push({ text: word, kind: keywords.has(word) ? "tok-keyword" : "" });
      rest = rest.slice(word.length);
      continue;
    }
    const otherMatch = rest.match(/^[^A-Za-z_"'`#]+/);
    if (otherMatch) {
      tokens.push({ text: otherMatch[0], kind: "" });
      rest = rest.slice(otherMatch[0].length);
      continue;
    }
    // 单个字符（引号开头但没闭合等），防止死循环。
    tokens.push({ text: rest[0], kind: "" });
    rest = rest.slice(1);
  }
  return tokens;
}

/** keywords 是常见语言的公共关键字集。跨语言混用没有关系：着色是提示不是编译。 */
const keywords = new Set([
  "func", "package", "import", "return", "if", "else", "for", "range", "go", "defer", "var", "const", "type",
  "struct", "interface", "switch", "case", "default", "break", "continue", "select", "chan", "map", "nil",
  "export", "function", "class", "extends", "async", "await", "let", "from", "new", "this", "try", "catch",
  "finally", "throw", "typeof", "interface", "enum", "public", "private", "readonly", "static", "void",
  "def", "elif", "while", "in", "is", "not", "and", "or", "pass", "with", "lambda", "yield", "True", "False",
  "None", "true", "false", "null", "end", "do", "then", "fn", "pub", "use", "mut", "match", "impl",
]);

/** missingFilePayload 构造“服务不可达时的最小文件卡片”，仍保留定位入口。 */
export function missingFilePayload(path: string, absPath: string): FilePayload {
  return { path, abs_path: absPath, binary: true, size: -1, not_found: true };
}

/** FileView 渲染一个侧栏文件：文本给排版，二进制/缺失给文件卡片。 */
export function FileView({ payload, sessionID }: { payload: FilePayload; sessionID: string }) {
  const ext = extension(payload.path);
  const isMarkdown = !payload.binary && !payload.not_found && markdownExtensions.has(ext);

  if (payload.binary || payload.not_found) {
    return <FileCard payload={missingSize(payload)} sessionID={sessionID} />;
  }
  // 本地兜底/旧后端可能只带路径。没有 content 时不能渲染空代码视图，
  // 按"当前不存在"给卡片，保留定位与复制入口。
  if (payload.content === undefined) {
    return <FileCard payload={missingSize({ ...payload, not_found: true })} sessionID={sessionID} />;
  }
  if (isMarkdown) {
    return (
      <div className="file-view">
        <FileHead path={payload.path} absPath={payload.abs_path} sessionID={sessionID} />
        <div className="activity-file-content markdown">
          <MarkdownView text={payload.content ?? ""} />
        </div>
      </div>
    );
  }
  return (
    <div className="file-view">
      <FileHead path={payload.path} absPath={payload.abs_path} sessionID={sessionID} />
      <CodeFile code={payload.content ?? ""} language={ext} />
    </div>
  );
}

/** missingSize 补齐缺失卡片的大小字段，兼容旧载荷。 */
function missingSize(payload: FilePayload): FilePayload {
  return { ...payload, size: payload.size ?? -1 };
}

/** FileHead 是文件查看顶部的路径条：相对路径 + 定位/复制按钮。 */
function FileHead({ path, absPath, sessionID }: { path: string; absPath: string; sessionID: string }) {
  return (
    <div className="file-view-head">
      <span className="file-view-path" title={absPath}>{path}</span>
      <RevealButton path={payloadAbs(path, absPath)} sessionID={sessionID} />
      <CopyButton text={absPath} className="link icon-btn" />
    </div>
  );
}

function payloadAbs(_path: string, absPath: string): string {
  return absPath;
}

/**
 * RevealButton 调用后端 reveal 端点，在系统文件管理器中定位文件。
 * 失败时按钮短暂显示"无法打开"——不弹窗，这种小事不值得打断用户。
 */
export function RevealButton({ path, sessionID }: { path: string; sessionID: string }) {
  const [failed, setFailed] = useState(false);
  const reveal = useCallback(async () => {
    try {
      await revealFile(sessionID, path);
      setFailed(false);
    } catch {
      setFailed(true);
      window.setTimeout(() => setFailed(false), 2000);
    }
  }, [sessionID, path]);
  return (
    <button className="link icon-btn" title="在文件管理器中显示" onClick={() => void reveal()}>
      {failed
        ? <Icon name="alert" size={13} />
        : <Icon name="folder" size={13} />}
    </button>
  );
}

/**
 * RevealParentButton 只打开文件所在的目录（不要求文件存在）。
 *
 * not_found 场景的主力动作：文件本身可能还没生成/已被删，但它该在的目录
 * 大概率在——去目录里看一眼是用户此刻真正想要的。后端 reveal 对不存在的
 * 文件也能打开其父目录（见该端点注释），这里只是把入口显式给出来。
 */
export function RevealParentButton({ path, sessionID }: { path: string; sessionID: string }) {
  const [failed, setFailed] = useState(false);
  const reveal = useCallback(async () => {
    try {
      // 复用 reveal 端点：macOS open -R / Windows explorer /select 对"目录部分
      // 存在、文件不存在"的路径会打开父目录，这正是想要的行为。
      await revealFile(sessionID, path);
      setFailed(false);
    } catch {
      setFailed(true);
      window.setTimeout(() => setFailed(false), 2000);
    }
  }, [sessionID, path]);
  return (
    <button className="link icon-btn" title="打开所在目录" onClick={() => void reveal()}>
      {failed ? <Icon name="alert" size={13} /> : <Icon name="folder" size={13} />}
      <span className="reveal-label">打开所在目录</span>
    </button>
  );
}

/** CodeFile 是带行号与轻量着色的代码视图。 */
function CodeFile({ code, language }: { code: string; language: string }) {
  const lines = useMemo(() => code.split("\n"), [code]);
  return (
    <div className="file-view-code">
      <ol className="file-code-lines">
        {lines.map((line, index) => (
          <li key={index} className="file-code-line">
            <code>
              {highlightLine(line, language).map((token, tokenIndex) =>
                token.kind
                  ? <span key={tokenIndex} className={token.kind}>{token.text}</span>
                  : <span key={tokenIndex}>{token.text}</span>,
              )}
            </code>
          </li>
        ))}
      </ol>
    </div>
  );
}

/** FileCard 是无法预览的文件的卡片：图标、大小、定位与复制。 */
function FileCard({ payload, sessionID }: { payload: FilePayload; sessionID: string }) {
  const isMissing = !!payload.not_found;
  const name = payload.path.split("/").pop() || payload.path;
  // 完整路径展示：文件名可能重复（多个 release/x.zip），用户需要知道它该在哪。
  const dir = payload.abs_path.slice(0, Math.max(0, payload.abs_path.length - name.length));
  return (
    <div className="file-view">
      <div className="file-card">
        <div className="file-card-icon">
          <Icon name="file" size={22} />
        </div>
        <div className="file-card-name" title={payload.abs_path}>{name}</div>
        <div className="file-card-dir" title={payload.abs_path}>{dir}</div>
        <div className="file-card-meta">
          {isMissing
            ? "文件当前不存在——可能还没生成，或已被移动/删除"
            : payload.binary
              ? `${formatSize(payload.size)} · 无法在侧栏预览`
              : formatSize(payload.size)}
        </div>
        <div className="file-card-actions">
          {isMissing ? (
            // 缺失文件的主动作是"去它该在的目录看看"——比在文件管理器里
            // 选中一个不存在的文件更符合用户此刻的意图。
            <RevealParentButton path={payload.abs_path} sessionID={sessionID} />
          ) : (
            <RevealButton path={payload.abs_path} sessionID={sessionID} />
          )}
          <CopyButton text={payload.abs_path} className="link icon-btn" />
        </div>
        <div className="file-card-hint">
          {isMissing ? "到它该在的位置看一眼 · 复制完整路径" : "在文件管理器中显示 · 复制路径"}
        </div>
      </div>
    </div>
  );
}
