// 左侧常驻的会话列表。
//
// 从独立页面改成侧栏，是因为 Agent 界面的常态是"一边看历史一边继续聊"：切一次会话
// 就整页跳转，等于把浏览器当成了终端。侧栏还顺带解决了"当前在哪个会话"这个问题——
// 高亮那一项就是答案。
//
// 它自己负责拉列表并定时刷新：会话的"最后活动"会随着对话推进而变，而这个组件拿不到
// 事件流（那是详情页的事）。定时刷新是这里最省事且够用的办法——列表查询走只读路径，
// 一次几毫秒，不会打扰正在跑的一轮。

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Link, useNavigate, useParams } from "react-router-dom";
import { deleteSession, listModels, listSessions, updateSession } from "./api";
import { Icon } from "./Icon";
import { loadTheme, saveTheme, themeNames, themeOrder, type Theme } from "./theme";
import { NewSessionModal } from "./NewSessionModal";
import { SkillsPanel } from "./SkillsPanel";
import { PluginsPanel } from "./PluginsPanel";
import { MCPPanel } from "./MCPPanel";
import type { ModelInfo } from "./types";
import { matchSessions } from "./filter";
import type { SessionSummary } from "./types";

/** 列表自动刷新的间隔。取 10 秒：够跟上"最后活动"的变化，又不至于成为噪音。 */
const refreshInterval = 10_000;

export function SessionSidebar() {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [modalOpen, setModalOpen] = useState(false);
  const [skillsOpen, setSkillsOpen] = useState(false);
  const [pluginsOpen, setPluginsOpen] = useState(false);
  const [mcpOpen, setMcpOpen] = useState(false);
  const [models, setModels] = useState<ModelInfo[]>([]);
  /** 是否把已归档的也列出来。默认不列——归档的意思就是"从眼前拿走"。 */
  const [showArchived, setShowArchived] = useState(false);
  /** 本地过滤词。**纯前端**：几十到几百个会话全在内存里，过滤即时且零请求。 */
  const [filter, setFilter] = useState("");
  /** 正在就地重命名的会话 id。 */
  const [renaming, setRenaming] = useState<string | null>(null);
  /** 展开了"更多"菜单的会话 id。 */
  const [menuOpen, setMenuOpen] = useState<string | null>(null);
  /** 主题选择菜单是否展开。 */
  const [themeMenuOpen, setThemeMenuOpen] = useState(false);
  const [theme, setTheme] = useState<Theme>(() => loadTheme());
  // 当前打开的会话 id，从路由里取，用来高亮。
  const { id: currentID } = useParams();
  const navigate = useNavigate();
  /** filterBox 供 Cmd/Ctrl+K 聚焦。 */
  const filterBox = useRef<HTMLInputElement>(null);

  // useCallback 把函数缓存下来，依赖是空数组，所以 refresh 在整个组件生命周期里
  // 是同一个函数——下面的 useEffect 依赖它，不稳定的话 effect 会每次渲染都重跑。
  const refresh = useCallback(async () => {
    try {
      const body = await listSessions(showArchived);
      setSessions(body.sessions);
      setError(null);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "读取会话列表失败");
    }
  }, [showArchived]);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), refreshInterval);
    // 清理函数：组件卸载时停掉定时器。不清的话它会一直跑下去，
    // 对着一个已经不存在的组件调 setState。
    return () => clearInterval(timer);
  }, [refresh]);

  // 切换会话之后立刻刷新一次：刚离开的那个会话的消息数变了。
  useEffect(() => {
    void refresh();
  }, [currentID, refresh]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const body = await listModels();
        if (!cancelled) setModels(body.models);
      } catch {
        // 模型目录加载失败不影响会话创建，只是下拉框为空。
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // 菜单打开时，点菜单外任意位置或按 Esc 关闭——不能只靠再点一次 ⋯，
  // 那是所有系统的菜单都不用的反模式。
  useEffect(() => {
    if (!menuOpen && !themeMenuOpen) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target as HTMLElement;
      if (menuOpen && !target.closest(".session-menu") && !target.closest(".session-more")) {
        setMenuOpen(null);
      }
      if (themeMenuOpen && !target.closest(".theme-menu") && !target.closest(".theme-btn")) {
        setThemeMenuOpen(false);
      }
    }
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") {
        setMenuOpen(null);
        setThemeMenuOpen(false);
      }
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [menuOpen, themeMenuOpen]);

  // Cmd/Ctrl+K 聚焦过滤框——这是唯一一个"找会话"的入口，值得一个快捷键。
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        filterBox.current?.focus();
        filterBox.current?.select();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  /** visible 是过滤之后要显示的会话。过滤规则见 filter.ts。 */
  const visible = useMemo(() => matchSessions(sessions, filter), [sessions, filter]);

  /** archive 归档或取消归档。 */
  async function archive(session: SessionSummary) {
    setMenuOpen(null);
    try {
      await updateSession(session.id, { archived: !session.archived });
      await refresh();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "归档失败");
    }
  }

  /**
   * remove 删除一个会话。
   *
   * 确认框里**必须显示标题和消息条数**——只问"确定吗"等于没问，用户看不出自己
   * 正要删掉的是哪一个、里面有多少东西。
   */
  async function remove(session: SessionSummary) {
    setMenuOpen(null);
    const confirmed = window.confirm(
      `删除会话「${session.title}」？\n\n` +
        `它有 ${session.message_count} 条消息，工作目录 ${session.workspace}。\n` +
        `消息、事件、摘要会一起删掉，**无法恢复**。`,
    );
    if (!confirmed) return;
    try {
      await deleteSession(session.id);
      await refresh();
      // 删掉的正是当前打开的那个，就回到欢迎页——否则详情页会对着一个
      // 已经不存在的会话干等。
      if (session.id === currentID) navigate("/");
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "删除失败");
    }
  }

  /** rename 提交重命名。空串表示恢复成从首条消息派生的标题。 */
  async function rename(session: SessionSummary, title: string) {
    setRenaming(null);
    if (title === session.title) return;
    try {
      await updateSession(session.id, { title });
      await refresh();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "重命名失败");
    }
  }

  return (
    <nav className="sidebar">
      <div className="sidebar-head">
        <span className="brand">
          <Icon name="sparkles" size={18} />
          GoSeek
        </span>
        <button className="btn-primary" onClick={() => setModalOpen(true)}>
          <Icon name="plus" size={14} />
          新会话
        </button>
      </div>

      <div className="sidebar-scroll">
      <input
        ref={filterBox}
        className="session-filter"
        value={filter}
        placeholder="过滤（⌘K）"
        onChange={(event) => setFilter(event.target.value)}
      />

      <div className="sidebar-tools">
        <button className="sidebar-tool" onClick={() => setSkillsOpen(true)}>
          <Icon name="puzzle" size={13} />
          Skills
        </button>
        <button className="sidebar-tool" onClick={() => setPluginsOpen(true)}>
          <Icon name="layers" size={13} />
          Plugins
        </button>
        <button className="sidebar-tool" onClick={() => setMcpOpen(true)}>
          <Icon name="list" size={13} />
          MCP
        </button>
      </div>

      <label className="archived-toggle">
        <input
          type="checkbox"
          checked={showArchived}
          onChange={(event) => setShowArchived(event.target.checked)}
        />
        <span className="toggle-track" aria-hidden="true" />
        显示已归档
      </label>

      {error && <p className="error">{error}</p>}
      {visible.length === 0 && !error && (
        <p className="empty">
          {sessions.length === 0 ? '还没有会话。点"新会话"开始。' : "没有匹配的会话。"}
        </p>
      )}

      <ul className="session-list">
        {visible.map((session) => (
          // key 用会话 id 而不是数组下标：下标会随排序变化（列表按最后活动倒序），
          // 导致 React 把状态错配到别的项上。
          <li key={session.id} className="session-row">
            {renaming === session.id ? (
              <RenameBox
                initial={session.custom_title ? session.title : ""}
                onCancel={() => setRenaming(null)}
                onSubmit={(title) => void rename(session, title)}
              />
            ) : (
              <>
                <Link
                  to={`/sessions/${session.id}`}
                  className={`session-item${session.id === currentID ? " current" : ""}${
                    session.archived ? " archived" : ""
                  }`}
                >
                  <div className="session-title">
                    {session.archived && <span className="archived-tag">已归档</span>}
                    {session.title}
                  </div>
                  <div className="session-meta">
                    <span>{formatTime(session.updated_at)}</span>
                    <span>{session.message_count} 条</span>
                  </div>
                  <div className="session-workspace" title={session.workspace}>
                    {session.workspace}
                  </div>
                </Link>
                <button
                  className="session-more"
                  title="更多"
                  aria-expanded={menuOpen === session.id}
                  onClick={() => setMenuOpen(menuOpen === session.id ? null : session.id)}
                >
                  <Icon name="more" size={15} />
                </button>
                {menuOpen === session.id && (
                  <div className="session-menu">
                    <button onClick={() => { setMenuOpen(null); setRenaming(session.id); }}>
                      <Icon name="edit" size={13} />
                      重命名
                    </button>
                    <button onClick={() => void archive(session)}>
                      <Icon name="archive" size={13} />
                      {session.archived ? "取消归档" : "归档"}
                    </button>
                    <button className="danger" onClick={() => void remove(session)}>
                      <Icon name="trash" size={13} />
                      删除
                    </button>
                  </div>
                )}
              </>
            )}
          </li>
        ))}
      </ul>
      </div>

      <div className="sidebar-foot">
        <button
          className="theme-btn"
          aria-expanded={themeMenuOpen}
          onClick={() => setThemeMenuOpen((open) => !open)}
        >
          <Icon name="sparkles" size={14} />
          主题 · {themeNames[theme]}
        </button>
        {themeMenuOpen && (
          <div className="theme-menu">
            {themeOrder.map((option) => (
              <button
                key={option}
                className={option === theme ? "current" : ""}
                onClick={() => {
                  setTheme(option);
                  saveTheme(option);
                  setThemeMenuOpen(false);
                }}
              >
                <span className="check-slot">
                  {option === theme && <Icon name="check" size={13} />}
                </span>
                {themeNames[option]}
              </button>
            ))}
          </div>
        )}
      </div>

      {/* 两个弹层都走 createPortal 挂到 body：侧栏的 backdrop-filter 会
          形成包含块，直接渲染在 <nav> 里的 fixed 遮罩会被困在侧栏里
          （真实踩过：面板只剩侧栏宽、压在输入区下面）。 */}
      {modalOpen &&
        createPortal(
          <NewSessionModal
            sessions={sessions}
            models={models}
            onClose={() => setModalOpen(false)}
            onCreated={(id) => {
              setModalOpen(false);
              void refresh();
              navigate(`/sessions/${id}`);
            }}
          />,
          document.body,
        )}

      {skillsOpen &&
        createPortal(<SkillsPanel onClose={() => setSkillsOpen(false)} />, document.body)}
      {pluginsOpen &&
        createPortal(<PluginsPanel onClose={() => setPluginsOpen(false)} />, document.body)}
      {mcpOpen &&
        createPortal(<MCPPanel onClose={() => setMcpOpen(false)} />, document.body)}
    </nav>
  );
}

/** RenameBox 是就地重命名的输入框。 */
function RenameBox({
  initial, onSubmit, onCancel,
}: { initial: string; onSubmit: (title: string) => void; onCancel: () => void }) {
  const [text, setText] = useState(initial);
  return (
    <form
      className="rename-box"
      onSubmit={(event) => {
        event.preventDefault();
        onSubmit(text.trim());
      }}
    >
      <input
        autoFocus
        value={text}
        placeholder="留空恢复默认标题"
        onChange={(event) => setText(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") onCancel();
        }}
        onBlur={() => onSubmit(text.trim())}
      />
    </form>
  );
}

/** formatTime 把 RFC3339 时间渲染成"几分钟前"这类相对描述。 */
function formatTime(raw: string): string {
  const when = new Date(raw);
  const elapsed = Date.now() - when.getTime();

  const minute = 60 * 1000;
  const hour = 60 * minute;
  const day = 24 * hour;

  if (elapsed < minute) return "刚刚";
  if (elapsed < hour) return `${Math.floor(elapsed / minute)} 分钟前`;
  if (elapsed < day) return `${Math.floor(elapsed / hour)} 小时前`;
  return when.toLocaleDateString();
}
