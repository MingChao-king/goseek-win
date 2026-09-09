// 活动侧栏：浏览器页面与打开的文件以 tab 形式叠放，当前 tab 占满内容区。
//
// # tab 语义
//
// 浏览器页面由 agent 的工具开出来（2s 轮询 /tabs 发现新页面），文件由用户
// 从对话流或改动列表点开。两者共享一个 tab 栏：当前 tab 白底融入内容区，
// 其余灰底；关闭按钮 hover 才出现。
//
// # 浏览器画面的轮询
//
// 截图每 2s 拉一次（img src + 时间戳破缓存）。不用 SSE 推帧：帧率和事件流
// 不在一个量级，而"看到画面在动"的反馈 2s 已经足够。点击/滚动坐标按
// naturalWidth 换算回视口坐标转发。
//
// # 改动文件抽屉
//
// 有改动时底部出现一条摘要条，点击展开/收起；点开文件成为一个 tab。
// 抽屉默认收起——它的作用是"知道改了什么"，不是常驻占画面。

import { useCallback, useEffect, useState } from "react";
import {
  browserActivateTab, browserClick, browserCloseTab, browserScroll, browserScreenshotURL,
  browserSetViewport, browserTabs, type BrowserTab,
} from "./api";
import { CopyButton } from "./CopyButton";
import { MarkdownView } from "./MarkdownView";
import { Icon } from "./Icon";

/** 侧栏里的一个 tab：浏览器页面或打开的文件。 */
interface PanelTab {
  key: string;
  title: string;
  kind: "browser" | "file";
  /** browser 是 tab id，file 是路径。 */
  detail: string;
  url?: string;
}

export function ActivityPanel({
  sessionID, changedFiles, openFiles, width, onOpenFile, onCloseFile, onClose,
}: {
  sessionID: string;
  changedFiles: string[];
  openFiles: Record<string, string>;
  /** 侧栏宽度（px），由父级的拖拽 resizer 控制。 */
  width: number;
  onOpenFile: (path: string) => void;
  onCloseFile: (path: string) => void;
  onClose: () => void;
}) {
  const [browserPages, setBrowserPages] = useState<BrowserTab[]>([]);
  const [activeKey, setActiveKey] = useState("");
  /** 改动文件抽屉是否展开。 */
  const [filesOpen, setFilesOpen] = useState(false);

  const tabs: PanelTab[] = [
    ...browserPages.map((t) => ({
      key: `tab:${t.id}`, title: t.title || t.url, kind: "browser" as const,
      detail: t.id, url: t.url,
    })),
    ...Object.keys(openFiles).map((path) => ({
      key: `file:${path}`, title: path.split("/").pop() || path,
      kind: "file" as const, detail: path,
    })),
  ];
  const current = tabs.find((t) => t.key === activeKey) ?? tabs[tabs.length - 1] ?? null;

  // 轮询浏览器页面列表。2s 一次：agent 开页面不是高频事件，轮询足够。
  //
  // **连续失败即降级**：会话的浏览器实例随后端进程生灭，后端重启后 /tabs
  // 会挂起或报错——继续每 2s 打一次没有意义，还会拖住事件流。连续失败
  // 3 次就停轮询，把页面清空；这个会话再开新页面时用户重开侧栏即可。
  useEffect(() => {
    let cancelled = false;
    let failures = 0;
    let timer: ReturnType<typeof setInterval> | null = null;
    const pull = () => {
      browserTabs(sessionID)
        .then((body) => {
          failures = 0;
          if (!cancelled) setBrowserPages(body.tabs ?? []);
        })
        .catch(() => {
          failures += 1;
          if (failures >= 3 && timer && !cancelled) {
            clearInterval(timer);
            timer = null;
            setBrowserPages([]);
          }
        });
    };
    pull();
    timer = setInterval(pull, 800);
    return () => { cancelled = true; if (timer) clearInterval(timer); };
  }, [sessionID]);

  const closeTab = useCallback(async (tab: PanelTab) => {
    if (tab.kind === "file") {
      onCloseFile(tab.detail);
    } else {
      try {
        const body = await browserCloseTab(sessionID, tab.detail);
        setBrowserPages(body.tabs ?? []);
      } catch {
        return;
      }
    }
    if (current?.key === tab.key) {
      const rest = tabs.filter((t) => t.key !== tab.key);
      setActiveKey(rest.length > 0 ? rest[rest.length - 1].key : "");
    }
  }, [sessionID, current, tabs, onCloseFile]);

  return (
    <aside className="activity-sidebar" style={{ width }}>
      {tabs.length > 0 && (
        <header className="activity-head">
          <div className="activity-tab-strip" role="tablist">
            {tabs.map((tab) => (
              <button
                key={tab.key}
                role="tab"
                aria-selected={current?.key === tab.key}
                className={`activity-tab${current?.key === tab.key ? " active" : ""}`}
                title={tab.url ?? tab.detail}
                onClick={() => {
                  setActiveKey(tab.key);
                  if (tab.kind === "browser") void browserActivateTab(sessionID, tab.detail);
                }}
              >
                <Icon name={tab.kind === "browser" ? "globe" : "file"} size={12} />
                <span className="activity-tab-label">{tab.title}</span>
                <span
                  className="activity-tab-close"
                  role="button"
                  aria-label={`关闭 ${tab.title}`}
                  onClick={(event) => {
                    event.stopPropagation();
                    void closeTab(tab);
                  }}
                >
                  <Icon name="close" size={10} />
                </span>
              </button>
            ))}
          </div>
          <button className="activity-close" onClick={onClose} title="收起侧栏">
            <Icon name="close" size={13} />
          </button>
        </header>
      )}

      <div className="activity-content">
        {current?.kind === "browser" && (
          <BrowserView sessionID={sessionID} url={current.url ?? ""} tabKey={current.key} />
        )}
        {current?.kind === "file" && <FileView path={current.detail} content={openFiles[current.detail]} />}
        {!current && (
          <div className="activity-empty">
            <Icon name="globe" size={26} />
            <p>这里会显示 agent 打开的浏览器页面和你点开的文件。</p>
            <p className="activity-empty-hint">对话里的文件链接、改动文件列表都会在这里打开。</p>
          </div>
        )}
      </div>

      {changedFiles.length > 0 && (
        <div className="changed-drawer">
          <button className="changed-drawer-head" onClick={() => setFilesOpen((open) => !open)}>
            <Icon name={filesOpen ? "chevronDown" : "chevronRight"} size={12} />
            改动文件
            <span className="changed-count">{changedFiles.length}</span>
          </button>
          {filesOpen && (
            <div className="changed-drawer-list">
              {changedFiles.map((path) => (
                <button
                  key={path}
                  className="changed-file-item"
                  title={path}
                  onClick={() => onOpenFile(path)}
                >
                  <Icon name="file" size={12} />
                  <span>{path}</span>
                </button>
              ))}
            </div>
          )}
        </div>
      )}
    </aside>
  );
}

/** BrowserView 是一个浏览器 tab 的内容：工具条 + 实时画面。 */
function BrowserView({ sessionID, url, tabKey }: { sessionID: string; url: string; tabKey: string }) {
  const [frameSize, setFrameSize] = useState({ w: 960, h: 640 });
  const [screenshotURL, setScreenshotURL] = useState("");

  useEffect(() => {
    setScreenshotURL(browserScreenshotURL(sessionID));
    // 800ms 一帧，接近浏览器原生渲染的流畅度；配合后端 q85 JPEG，画面不糊。
    const timer = setInterval(() => setScreenshotURL(browserScreenshotURL(sessionID)), 800);
    return () => clearInterval(timer);
  }, [sessionID, tabKey]);

  // 挂载时把 viewport 对齐到 1280×800 fit（真实桌面浏览器的常见视口），
  // 保证模型看到的页面布局和用户在侧栏看到的一致；宽高只影响显示，不改变
  // 模型的坐标体系（后端按 fit 返回等比缩放的视口信息）。
  useEffect(() => {
    const targetId = tabKey.replace(/^tab:/, "");
    if (!targetId) return;
    void browserSetViewport(sessionID, targetId, 1280, 800, 1, true).catch(() => {});
  }, [sessionID, tabKey]);

  /** toViewport 把图片上的点击/滚动位置换算成浏览器视口坐标。 */
  const toViewport = (element: HTMLImageElement, clientX: number, clientY: number) => {
    const rect = element.getBoundingClientRect();
    return {
      x: ((clientX - rect.left) / rect.width) * frameSize.w,
      y: ((clientY - rect.top) / rect.height) * frameSize.h,
    };
  };

  return (
    <div className="browser-view">
      <div className="browser-toolbar">
        <span className="browser-url" title={url}>{url}</span>
        <span className="copy-slot">
          <CopyButton text={url} label="复制链接" />
        </span>
        <button
          className="icon-btn"
          title="立即刷新画面"
          onClick={() => setScreenshotURL(browserScreenshotURL(sessionID))}
        >
          <Icon name="refresh" size={13} />
        </button>
      </div>
      <div className="browser-stage">
        <img
          key={tabKey}
          src={screenshotURL}
          alt=""
          className="activity-live"
          onClick={(event) => {
            const point = toViewport(event.currentTarget, event.clientX, event.clientY);
            void browserClick(sessionID, tabKey.replace(/^tab:/, ""), point.x, point.y);
          }}
          onWheel={(event) => {
            const point = toViewport(event.currentTarget, event.clientX, event.clientY);
            void browserScroll(sessionID, tabKey.replace(/^tab:/, ""), point.x, point.y, event.deltaY);
          }}
          onLoad={(event) => {
            const img = event.currentTarget;
            if (img.naturalWidth > 0) setFrameSize({ w: img.naturalWidth, h: img.naturalHeight });
          }}
        />
      </div>
    </div>
  );
}

/** FileView 是文件 tab 的内容：完整路径 + 正文。 */
function FileView({ path, content }: { path: string; content: string }) {
  // Markdown 文件渲染成格式化视图（与对话流同一套解析器），
  // 其余文件保持等宽原文——代码、日志、JSON 的缩进和符号是内容本身。
  const isMarkdown = /\.(md|markdown)$/i.test(path);
  return (
    <div className="file-view">
      <div className="browser-toolbar">
        <span className="browser-url" title={path}>{path}</span>
        <span className="copy-slot">
          <CopyButton text={path} label="复制路径" />
        </span>
      </div>
      {isMarkdown ? (
        <div className="activity-file-content markdown">
          <MarkdownView text={content} />
        </div>
      ) : (
        <pre className="activity-file-content">{content}</pre>
      )}
    </div>
  );
}
