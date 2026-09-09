// 输入区：多行、草稿、历史回溯、运行中注入、停止。
//
// 这里集中了 M5.2 里"输入永不丢失"和"边跑边说"两条判据的实现。
//
// # 运行中既能打字也能发送
//
// 原来一轮在跑时整个输入框禁用。现在完全放开——发出去的消息会被注入到正在进行
// 的那一轮里（后端 agent.Injector）。禁用输入是在惩罚用户的耐心；而只让打字不让
// 发送，是把话堵在半路。
//
// 按钮的文案随状态变：空闲时"发送"，运行中"插入"，并说明它什么时候到达模型。

import { useCallback, useEffect, useRef, useState } from "react";
import { uploadImage, type ImageUpload, type SkillSummary } from "./api";
import { commands, parseSkillInput } from "./commands";
import { clearDraft, loadDraft, saveDraft } from "./draft";
import { entryAt, notBrowsing, stepBack, stepForward } from "./history";
import { Icon } from "./Icon";
import type { RunState } from "./types";

export function Composer({
  sessionID, ready, busy, runState, sentHistory, skills, onSubmit, onCancel,
}: {
  sessionID: string;
  /** 快照还没加载完时不能提交——那时连 last_sequence 都不知道。 */
  ready: boolean;
  /** 会话里有事在跑。**不禁用输入**，只改文案。 */
  busy: boolean;
  runState: RunState;
  /** 用户在这个会话里发过的消息，按时间正序，供 ↑ 回溯。 */
  sentHistory: string[];
  /** 已导入的 skill 列表，供 / 联想菜单展示。 */
  skills?: SkillSummary[];
  onSubmit: (text: string, imageIds: string[]) => Promise<void>;
  onCancel: () => Promise<void>;
}) {
  const [text, setText] = useState(() => loadDraft(sessionID));
  const [sending, setSending] = useState(false);
  const [cursor, setCursor] = useState(notBrowsing);
  const [images, setImages] = useState<ImageUpload[]>([]);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState("");
  const [dragOver, setDragOver] = useState(false);
  /** / 联想菜单是否展开。输入以 / 开头（不是 //）时自动弹出。 */
  const [showAutocomplete, setShowAutocomplete] = useState(false);
  /** 联想菜单里当前高亮的条目下标（-1 = 无，用鼠标）。 */
  const [activeItem, setActiveItem] = useState(-1);
  const box = useRef<HTMLTextAreaElement>(null);

  // 换会话时把草稿换过来。依赖里带 sessionID：切走再切回来，看到的应当是
  // **这个会话**的草稿，而不是上一个会话的残留。
  useEffect(() => {
    setText(loadDraft(sessionID));
    setCursor(notBrowsing);
    setImages([]);
    setUploadError("");
    setDragOver(false);
  }, [sessionID]);

  // 每次改动都存。存的时机不重要（存多了没代价），清的时机才重要——
  // 只在发送成功之后清（见 send）。
  useEffect(() => {
    saveDraft(sessionID, text);
  }, [sessionID, text]);

  // 自适应高度：最少一行，最多约十二行，再多则内部滚动。
  // 直接改 style 而不是用 state：这是纯粹的布局，走 state 会多一轮渲染。
  useEffect(() => {
    const element = box.current;
    if (!element) return;
    element.style.height = "auto";
    element.style.height = `${Math.min(element.scrollHeight, 260)}px`;
  }, [text]);

  async function send() {
    const content = text.trim();
    if ((!content && images.length === 0) || sending || !ready) return;
    setSending(true);
    try {
      const skill = parseSkillInput(content);
      const payload = skill
        ? skill.text
          ? `请使用 skill "${skill.name}" 完成以下任务：${skill.text}`
          : `请使用 skill "${skill.name}"，按照 skill 里的指令行动。`
        : content;
      await onSubmit(payload, images.map((image) => image.id));
      // **只有成功了才清**。失败时用户还想要那段文字。
      setText("");
      clearDraft(sessionID);
      setCursor(notBrowsing);
      setImages([]);
    } finally {
      setSending(false);
    }
  }

  const addFiles = useCallback(async (files: FileList | File[]) => {
    if (!sessionID) {
      setUploadError("会话还没准备好，请稍后再试");
      return;
    }
    const valid = Array.from(files).filter((file) =>
      ["image/png", "image/jpeg", "image/webp"].includes(file.type),
    );
    if (valid.length === 0) {
      setUploadError("仅支持 PNG / JPEG / WebP 图片");
      return;
    }
    setUploading(true);
    setUploadError("");
    try {
      const results = await Promise.all(
        valid.slice(0, 4).map(async (file) => {
          // WKWebView 的网络进程拿不到拖入文件的磁盘沙箱扩展，直接传 File 会
          // 发出一个空请求体。先读进内存，再以同名同 MIME 的 Blob 发送。
          const content = await file.arrayBuffer();
          const memoryFile = new File([content], file.name, {
            type: file.type || "application/octet-stream",
          });
          return uploadImage(sessionID, memoryFile);
        }),
      );
      setImages((previous) => [...previous, ...results].slice(0, 4));
    } catch (failure) {
      setUploadError(failure instanceof Error && failure.message ? failure.message : "图片上传失败，请重试");
    } finally {
      setUploading(false);
    }
  }, [sessionID]);

  function onKeyDown(event: React.KeyboardEvent<HTMLTextAreaElement>) {
    // 联想菜单展开时，↑↓/Enter/Esc 先给菜单用——这是所有补全菜单的惯例；
    // 菜单关掉以后 ↑ 才回到历史回溯。
    if (showAutocomplete) {
      const items = autocompleteItems();
      if (event.key === "ArrowDown") {
        event.preventDefault();
        setActiveItem((current) => (current + 1) % items.length);
        return;
      }
      if (event.key === "ArrowUp") {
        event.preventDefault();
        setActiveItem((current) => (current - 1 + items.length) % items.length);
        return;
      }
      if (event.key === "Enter" && activeItem >= 0 && items[activeItem]) {
        event.preventDefault();
        pickAutocomplete(items[activeItem]);
        return;
      }
      if (event.key === "Escape") {
        event.preventDefault();
        setShowAutocomplete(false);
        setActiveItem(-1);
        return;
      }
    }
    // Enter 发送，Shift+Enter 换行。这是对话框的既有习惯。
    // Cmd/Ctrl + Enter 也发送：从别的工具（多数要求组合键）过来的人会习惯性地按它，
    // 不接住的话他会得到一个换行，然后疑惑为什么没发出去。
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      void send();
      return;
    }
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void send();
      return;
    }
    // ↑ / ↓ 回溯历史，**只在输入框为空或正在回溯时生效**——否则用户在多行文本
    // 里上下移动光标会被劫持。
    if (event.key === "ArrowUp" && (text === "" || cursor !== notBrowsing)) {
      event.preventDefault();
      const next = stepBack(sentHistory, cursor);
      setCursor(next);
      setText(entryAt(sentHistory, next));
      return;
    }
    if (event.key === "ArrowDown" && cursor !== notBrowsing) {
      event.preventDefault();
      const next = stepForward(sentHistory, cursor);
      setCursor(next);
      setText(entryAt(sentHistory, next));
    }
  }

  /** autocompleteItems 是过滤后的扁平列表（键盘导航用）。 */
  const autocompleteItems = () => filteredItems(text, skills);

  /** autocompleteGroups 按组聚合（渲染用）。 */
  const autocompleteGroups = () => {
    const items = filteredItems(text, skills);
    const groups: { label: string; items: AutocompleteItem[] }[] = [];
    for (const item of items) {
      const group = groups.find((each) => each.label === item.group);
      if (group) group.items.push(item);
      else groups.push({ label: item.group, items: [item] });
    }
    return groups;
  };

  /** pickAutocomplete 选中一条：插入文本、关菜单、焦点回到输入框。 */
  function pickAutocomplete(item: AutocompleteItem) {
    setText(item.insert);
    setShowAutocomplete(false);
    setActiveItem(-1);
    box.current?.focus();
  }

  return (
    <>
      <form
        className={`composer${dragOver ? " composer-drag-over" : ""}`}
        onSubmit={(event) => {
          event.preventDefault();
          void send();
        }}
      >
        {showAutocomplete && autocompleteGroups().length > 0 && (
          <div className="autocomplete">
            {autocompleteGroups().map((group) => (
              <div key={group.label} className="autocomplete-group">
                <div className="autocomplete-group-label">{group.label}</div>
                {group.items.map((item) => (
                  <button
                    key={item.insert}
                    type="button"
                    className={`autocomplete-item${
                      autocompleteItems()[activeItem] === item ? " active" : ""
                    }`}
                    // mousedown 而不是 click：click 前 input 先 blur，
                    // 菜单会被 blur 的延时关闭吞掉。
                    onMouseDown={(event) => {
                      event.preventDefault();
                      pickAutocomplete(item);
                    }}
                  >
                    <span className="autocomplete-name">{item.label}</span>
                    <span className="autocomplete-desc">{item.description}</span>
                  </button>
                ))}
              </div>
            ))}
          </div>
        )}
        {images.length > 0 && (
          <div className="composer-images">
            {images.map((image) => (
              <div key={image.id} className="composer-image-thumb">
                <img src={`/api/v1/images/${image.id}`} alt="" width={64} height={64} />
                <button
                  type="button"
                  className="composer-image-remove"
                  onClick={() => setImages((previous) => previous.filter((item) => item.id !== image.id))}
                  aria-label="移除图片"
                >
                  <Icon name="close" size={10} />
                </button>
              </div>
            ))}
          </div>
        )}
        {uploading && <div className="composer-uploading">正在上传图片…</div>}
        {!uploading && uploadError && (
          <div className="composer-upload-error" role="alert">{uploadError}</div>
        )}
        <div className="composer-input-row">
          <textarea
            ref={box}
            value={text}
            rows={1}
            onChange={(event) => {
              setText(event.target.value);
              // 一动手打字就退出回溯——否则再按 ↑ 会把他刚打的覆盖掉。
              setCursor(notBrowsing);
              setShowAutocomplete(
                event.target.value.startsWith("/") && !event.target.value.startsWith("//"),
              );
              setActiveItem(-1);
            }}
            onKeyDown={onKeyDown}
            onBlur={() =>
              setTimeout(() => {
                setShowAutocomplete(false);
                setActiveItem(-1);
              }, 150)
            }
            placeholder={ready ? "说点什么，或输入 /help 看命令" : "正在加载…"}
            disabled={!ready}
            onPaste={(event) => {
              const files = Array.from(event.clipboardData?.files ?? []);
              if (files.length > 0) {
                event.preventDefault();
                void addFiles(files);
              }
            }}
            onDragOver={(event) => {
              event.preventDefault();
              setDragOver(true);
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={(event) => {
              event.preventDefault();
              setDragOver(false);
              if (event.dataTransfer?.files) void addFiles(event.dataTransfer.files);
            }}
          />
          <div className="composer-buttons">
            {busy && (
              // 停止和"插入"是两件事，入口不能长得像：一个是"别做了"，
              // 一个是"接着做，但换个方向"。所以停止单独一个按钮、红色。
              <button type="button" className="stop" onClick={() => void onCancel()}>
                <Icon name="stop" size={13} />
                停止
              </button>
            )}
            <button type="submit" className="btn-primary" disabled={!ready || sending || (!text.trim() && images.length === 0)}>
              <Icon name="send" size={13} />
              {sending ? "…" : busy ? "插入" : "发送"}
            </button>
          </div>
        </div>
      </form>

      <p className="composer-hint">
        {busy ? (
          <>
            {describeBusy(runState)} · 现在发出的消息会在当前步骤结束后送达模型
          </>
        ) : (
          <>Enter 发送 · Shift+Enter 换行 · ↑ 回溯 · /help 看命令</>
        )}
      </p>
    </>
  );
}

/** 联想菜单的一条可选条目。 */
interface AutocompleteItem {
  /** 展示用，如 /compact 或 /skill:build。 */
  label: string;
  /** 选中后插入输入框的文本。 */
  insert: string;
  description: string;
  group: "命令" | "Skills";
}

/** filteredItems 按当前输入过滤。输入 /com 时只剩 /compact。 */
function filteredItems(text: string, skills: SkillSummary[] | undefined): AutocompleteItem[] {
  // text 以 / 开头；取 / 之后、空格之前的部分做包含过滤。
  const query = /^\/([^\s]*)/.exec(text)?.[1]?.toLowerCase() ?? "";
  const commandItems: AutocompleteItem[] = commands.map((command) => ({
    label: command.usage,
    insert: `/${command.name} `,
    description: command.description,
    group: "命令",
  }));
  const skillItems: AutocompleteItem[] = (skills ?? []).map((skill) => ({
    label: `/skill:${skill.name}`,
    insert: `/skill:${skill.name} `,
    description: skill.description,
    group: "Skills",
  }));
  return [...commandItems, ...skillItems].filter(
    (item) => !query || item.label.toLowerCase().includes(query),
  );
}

/** describeBusy 说明现在在等什么，而不是笼统一句"忙碌中"。 */
function describeBusy(state: RunState): string {
  switch (state) {
    case "WAITING_MODEL":
      return "正在等模型回复";
    case "RUNNING_TOOL":
      return "正在执行命令";
    case "COMPRESSING":
      return "正在整理上下文";
    default:
      return "忙碌中";
  }
}
