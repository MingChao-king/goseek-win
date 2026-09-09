// 会话详情：历史、实时事件、输入框、上下文仪表盘、摘要树。
//
// 数据流：
//
//   1. 挂载时取快照 → dispatch({type:"snapshot"}) 建立初始状态；
//   2. 拿快照里的 last_sequence 作为起点订阅 SSE；
//   3. 每收到一个事件 → dispatch({type:"event"}) 更新状态；
//   4. 状态一变，React 重新渲染。
//
// 第 1 步和第 2 步之间的衔接由 last_sequence 保证：从那个序号往后订阅，中间既不会
// 漏掉事件，也不会把快照里已有的再收一遍。
//
// 这个组件只负责**编排**：拉数据、管开关、分发命令。具体怎么画交给
// Transcript / MemoryPanel / ContextGauge 三个组件——它们各自的取舍写在各自文件里。

import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import {
  ApiFailure, browserActivateTab, browserClick, browserCloseTab, browserInsertText, browserNavigate,
  browserPressKey,
  browserScroll, browserSetViewport, browserStreamURL, browserTabs,
  cancelTurn, compactSession, editMemory, getMemory,
  getSession, listModels, listSkills, submitTurn, updateSession, type BrowserTab,
  type SidebarContextItem,
} from "./api";
import { Composer } from "./Composer";
import { Icon } from "./Icon";
import { isAtBottom, shouldFollow } from "./scroll";
import { helpText, parseInput } from "./commands";
import { ContextGauge } from "./ContextGauge";
import { MemoryPanel } from "./MemoryPanel";
import { Transcript } from "./Transcript";
import { initialState, reduce, type Turn } from "./reducer";
import { textOf } from "./payload";
import { useEventStream } from "./useEventStream";
import type { MemoryTree, Message, ModelInfo, RunEvent, RunState } from "./types";
import type { SkillSummary } from "./api";

export function SessionDetailPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();

  // useReducer 是 useState 的兄弟：状态复杂、且"一件事引起多处变化"时用它。
  // 见 reducer.ts 开头的说明。
  const [state, dispatch] = useReducer(reduce, initialState);
  // activityOpen 控制右侧活动面板的展开/收起。
  const [activityOpen, setActivityOpen] = useState(false);
  // openFiles 是侧栏同时展开的文件（path → content），并列展示。
  const [openFiles, setOpenFiles] = useState<Record<string, string>>({});
  // activityOpen 控制右侧活动面板的展开/收起。
  // activityWidth 是活动侧栏宽度（px），可拖动左边框调整。
  const [activityWidth, setActivityWidth] = useState(480);
  // sidebarPagesRef 记录侧栏当前打开的页面 URL（ActivitySidebar 轮询后更新）。
  // 用户发消息时把这些 URL 附加到请求，让模型知道侧栏有什么。
  const sidebarPagesRef = useRef<string[]>([]);
  const updateSidebarPages = useCallback((urls: string[]) => {
    sidebarPagesRef.current = urls;
  }, []);
  const activitySidebarRef = useRef<HTMLElement | null>(null);
  const resizeStartRef = useRef<{
    pointerID: number;
    target: HTMLDivElement;
    x: number;
    width: number;
  } | null>(null);
  const pendingActivityWidthRef = useRef(activityWidth);
  const resizeFrameRef = useRef<number | null>(null);
  const resizeCleanupRef = useRef<(() => void) | null>(null);

  /**
   * error 是当前的错误提示。**每条错误都要回答"现在能做什么"**——只说发生了什么，
   * 用户只能刷新页面碰运气。所以它不是一个字符串，而是「一句话 + 一个可选动作」。
   *
   * 动作用一个枚举而不是回调：describeFailure 是纯函数、要能单独测，而回调需要
   * 拿到组件里的 id、navigate 这些东西。翻译成按钮的活儿留给渲染处（见 ErrorBar）。
   */
  const [error, setError] = useState<Failure | null>(null);
  const handleSidebarError = useCallback((failure: unknown) => {
    setError(describeFailure(failure, "侧边栏操作失败"));
  }, []);
  // ready 表示快照已经加载完。没加载完就订阅的话，起点序号还是 0，
  // 会把整个会话重放一遍。
  const [ready, setReady] = useState(false);
  const [startSequence, setStartSequence] = useState(0);
  /**
   * notices 是**界面自己**给出的提示（/help 的输出、"没有这个命令"）。
   *
   * 它刻意不进 state.turns：那些是会话里真实发生过的事，由快照和事件流决定，
   * 刷新页面后原样重现。本地提示不是会话事实，混进去会让 reducer 不再是
   * "快照 + 事件"的纯函数，而且刷新之后凭空消失，反而更让人困惑。
   */
  const [notices, setNotices] = useState<string[]>([]);
  /**
   * queued 是**已发出但还没被注入**的消息。
   *
   * 一轮在跑时提交，后端会把消息排进队列，等这一步结束、下一次请求模型之前才
   * 追加进对话——中间可能隔着一整条命令的执行时间。那段时间里界面上什么都没有，
   * 用户会以为自己没点到发送、于是再发一遍。所以这里先乐观地画出来。
   *
   * 它**不进 reducer**：reducer 是"快照 + 事件"的纯函数，塞进一条本地臆测的消息
   * 会破坏这个性质（刷新页面后它凭空消失，reducer 却认为它存在过）。真实的
   * user.message 事件一到，就把对应的那条从这里摘掉，见下面的 onEvent。
   */
  const [queued, setQueued] = useState<string[]>([]);
  const [memoryOpen, setMemoryOpen] = useState(false);
  const [turnIndexOpen, setTurnIndexOpen] = useState(false);
  const [tree, setTree] = useState<MemoryTree | null>(null);
  const [skills, setSkills] = useState<SkillSummary[]>([]);
  /** 快照里的完整历史，供摘要树面板显示某个节点覆盖的原文。 */
  const [messages, setMessages] = useState<Message[]>([]);
  const [models, setModels] = useState<ModelInfo[]>([]);
  const [snapshotModel, setSnapshotModel] = useState("");

  // 加载快照。依赖 [id]：切换到别的会话时要重新加载。
  useEffect(() => {
    let cancelled = false;
    // 换会话时先把上一个会话的界面状态清干净，否则新会话加载完之前，
    // 屏幕上还挂着上一个会话的消息。
    setReady(false);
    setMemoryOpen(false);
    setTurnIndexOpen(false);
    setTree(null);
    setNotices([]);
    setError(null);
    setQueued([]);
    sidebarPagesRef.current = [];

    void (async () => {
      try {
        const snapshot = await getSession(id);
        setSnapshotModel(snapshot.model ?? "");
        // 组件可能在请求返回之前就被卸载了（用户很快切走）。对已卸载的组件
        // dispatch 是无意义的，用一个标志位挡住。
        if (cancelled) return;
        dispatch({ type: "snapshot", snapshot });
        setMessages(snapshot.messages);
        setStartSequence(snapshot.last_sequence);
        setReady(true);
      } catch (failure) {
        if (cancelled) return;
        setError(describeFailure(failure, "读取会话失败"));
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [id]);

  // 模型目录是静态配置，加载一次即可；失败时下拉框留在“当前模型”状态。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const body = await listModels();
        if (!cancelled) setModels(body.models);
      } catch {
        // 目录加载失败不阻断会话；切换入口暂时只显示当前模型。
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // onEvent 必须用 useCallback 包稳定，否则每次渲染都是新函数，
  // useEventStream 里的 effect 会跟着重跑，SSE 连接被反复重建。
  // dispatch 由 React 保证稳定，因此依赖数组可以是空的。
  const onEvent = useCallback((event: RunEvent) => {
    dispatch({ type: "event", event });
    // 真身到了，撤掉替身。按内容匹配而不是按顺序：注入是 FIFO，但队列里可能
    // 有重复文本，匹配第一条相同的即可（画出来的两条本来就长得一样）。
    if (event.type === "user.message") {
      const arrived = textOf(event.payload);
      setQueued((previous) => {
        const at = previous.indexOf(arrived);
        if (at < 0) return previous;
        return [...previous.slice(0, at), ...previous.slice(at + 1)];
      });
    }
  }, []);

  const streamStatus = useEventStream(ready ? id : null, startSequence, onEvent);

  // 压缩会改变摘要树。收到 completed 事件之后，已经取来的那棵树就过期了——丢掉它，
  // 下次展开时重新取。这比在前端自己拼一棵树可靠：树的形状由后端决定，前端照着
  // 事件推演迟早会推错。
  const treeVersion = state.memory.total_batches;
  useEffect(() => {
    setTree(null);
  }, [treeVersion]);

  // Skill 列表供 / 联想菜单使用。挂载时拉一次，导入面板关闭后靠 side effect 刷新。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const body = await listSkills();
        if (!cancelled) setSkills(body.skills);
      } catch {
        // 读取失败不影响主流程，联想菜单只是少了 skill 部分。
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // 展开面板时才取树：节点正文可能上千字，不该塞进每次快照。
  useEffect(() => {
    if (!memoryOpen || tree !== null || !ready) return;
    let cancelled = false;
    void (async () => {
      try {
        const fetched = await getMemory(id);
        if (!cancelled) setTree(fetched);
      } catch (failure) {
        if (!cancelled) setError(describeFailure(failure, "读取摘要树失败"));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [memoryOpen, tree, ready, id]);

  // Esc 关闭摘要树面板。
  //
  // 挂在 window 上而不是某个元素上：按 Esc 的时候焦点可能在任何地方（输入框、
  // 某个按钮、什么都没有），只有全局监听才每次都收得到。
  //
  // **面板内部的"取消编辑"优先**：编辑器自己也监听 Esc 并 stopPropagation
  // （见 MemoryPanel），所以正在编辑时按 Esc 只退出编辑，再按一次才关面板。
  // 反过来的话，改了一半按 Esc，整个面板连同改动一起消失。
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") setMemoryOpen(false);
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  /**
   * busy 表示会话里有事在跑。
   *
   * 它**不再阻止提交**（消息会被注入到正在进行的那一轮），只影响两件事：
   * 摘要不能改（后端会拒），以及输入区的文案与"停止"按钮的显隐。
   */
  const busy = state.runState !== "IDLE" && state.runState !== "FAILED";

  /** sentHistory 是用户在这个会话里发过的消息，按时间正序，供 ↑ 回溯。 */
  const sentHistory = useMemo(() => {
    const sent: string[] = [];
    for (const turn of state.turns) {
      for (const item of turn.items) {
        if (item.kind === "user") sent.push(item.text);
      }
    }
    return sent;
  }, [state.turns]);

  const note = useCallback((text: string) => {
    setNotices((previous) => [...previous, text]);
  }, []);

  /**
   * runCommand 执行一个已知命令。
   *
   * 三个命令里只有 /compact 要往后端发请求，另外两个纯本地——这正是命令解析放在
   * 前端的好处之一：本地反馈立刻可见，不必往返一趟。
   */
  const runCommand = useCallback(
    async (name: "compact" | "memory" | "help") => {
      switch (name) {
        case "help":
          note(helpText());
          return;
        case "memory":
          setMemoryOpen((open) => !open);
          return;
        case "compact":
          try {
            await compactSession(id);
            // 不在这里报"压缩完成"：请求只是被**接受**了，真正的过程和结果由
            // 事件流带回来，对话流里会出现一个完整的压缩片段。
            note("已请求压缩，过程见对话流。");
          } catch (failure) {
            setError(describeFailure(failure, "压缩失败"));
          }
      }
    },
    [id, note],
  );

  /**
   * handleEdit 保存一段摘要的人工修订。
   *
   * 后端把修订之后的整棵树直接带回来，因此这里不需要再发一次 GET——修订会改变
   * 节点标题（标题取正文首行），界面上不止一处要跟着变。
   */
  const handleEdit = useCallback(
    async (batchID: string, content: string) => {
      try {
        setTree(await editMemory(id, batchID, content));
        setError(null);
      } catch (failure) {
        setError(describeFailure(failure, "保存摘要修订失败"));
        // 往上抛，让编辑框留在编辑态——保存失败了还把框关掉，用户刚写的东西就没了。
        throw failure;
      }
    },
    [id],
  );

  /**
   * handleSubmit 处理一次输入。
   *
   * 先分流：斜杠命令走本地或专用端点，普通文字才发给模型。不认识的命令**就地
   * 报错、不发任何请求**——否则用户敲错一个命令，代价是一次真实的模型调用，
   * 而且模型会一本正经地回答一个不存在的命令。
   *
   * 抛出异常表示"没发成功"，Composer 据此保留草稿。
   */
  const handleSubmit = useCallback(
    async (raw: string, imageIds: string[] = []) => {
      const parsed = parseInput(raw);
      if (parsed.kind === "unknown") {
        setError({ text: `没有 /${parsed.name} 这个命令。输入 /help 看可用命令。` });
        throw new Error("unknown command");
      }
      setError(null);
      if (parsed.kind === "command") {
        await runCommand(parsed.name);
        return;
      }
      if (!parsed.text) throw new Error("empty");
      try {
        const sidebarItems: SidebarContextItem[] = [
          ...sidebarPagesRef.current.map((url) => ({ kind: "page" as const, url })),
          ...Object.keys(openFiles).map((path) => ({ kind: "file" as const, path })),
        ];
        await submitTurn(id, parsed.text, imageIds, sidebarItems);
        // 空闲时提交会立刻产生 turn.started + user.message，替身多余；
        // 只有排队的情况才需要它。
        if (busy) setQueued((previous) => [...previous, parsed.text]);
      } catch (failure) {
        setError(describeFailure(failure, "提交失败"));
        throw failure;
      }
    },
    [id, busy, runCommand, openFiles],
  );

  /** handleCancel 中止当前这一轮。会话继续存在，之后还能接着聊。 */
  const handleCancel = useCallback(async () => {
    try {
      await cancelTurn(id);
    } catch (failure) {
      setError(describeFailure(failure, "中止失败"));
    }
  }, [id]);

  /** handleModelChange 切换会话模型。失败时保留当前选择，并把可操作原因显示出来。 */
  const handleModelChange = useCallback(async (model: string) => {
    try {
      await updateSession(id, { model });
      setError(null);
      const snapshot = await getSession(id);
      dispatch({ type: "snapshot", snapshot });
    } catch (failure) {
      setError(describeFailure(failure, "切换模型失败"));
    }
  }, [id]);

  /** 头部标题：取第一条用户消息，和侧栏派生标题同一条规则。 */
  const title = useMemo(() => {
    for (const turn of state.turns) {
      for (const item of turn.items) {
        if (item.kind === "user") {
          const compact = item.text.replace(/\s+/g, " ").trim();
          if (compact) return compact.length > 60 ? `${compact.slice(0, 60)}…` : compact;
        }
      }
    }
    return "新会话";
  }, [state.turns]);

  // 折叠边界：游标之前的消息模型看到的是摘要。游标为 1 表示一条都没折叠。
  const foldedUpTo = state.memory.raw_compaction_cursor - 1;

  // 拖动期间直接在动画帧里改侧栏宽度，松手后才提交 React state。
  // 长会话和真实 iframe 因此不会在每个 pointermove 上整棵重渲染。
  const clampActivityWidth = useCallback((width: number) => {
    return Math.max(320, Math.min(Math.max(320, window.innerWidth - 400), width));
  }, []);

  const finishResize = useCallback(() => {
    const start = resizeStartRef.current;
    if (!start) return;
    resizeCleanupRef.current?.();
    resizeCleanupRef.current = null;
    if (resizeFrameRef.current !== null) {
      cancelAnimationFrame(resizeFrameRef.current);
      resizeFrameRef.current = null;
    }
    const width = pendingActivityWidthRef.current;
    if (activitySidebarRef.current) activitySidebarRef.current.style.width = `${width}px`;
    resizeStartRef.current = null;
    document.documentElement.classList.remove("activity-is-resizing");
    if (start.target.hasPointerCapture(start.pointerID)) {
      start.target.releasePointerCapture(start.pointerID);
    }
    setActivityWidth(width);
  }, []);

  const startResize = useCallback((event: React.PointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || !activitySidebarRef.current) return;
    event.preventDefault();
    resizeCleanupRef.current?.();

    const width = activitySidebarRef.current.getBoundingClientRect().width;
    resizeStartRef.current = {
      pointerID: event.pointerId,
      target: event.currentTarget,
      x: event.clientX,
      width,
    };
    pendingActivityWidthRef.current = width;
    event.currentTarget.setPointerCapture(event.pointerId);
    document.documentElement.classList.add("activity-is-resizing");

    const move = (clientX: number) => {
      const start = resizeStartRef.current;
      if (!start) return;
      pendingActivityWidthRef.current = clampActivityWidth(start.width + start.x - clientX);
      if (resizeFrameRef.current !== null) return;
      resizeFrameRef.current = requestAnimationFrame(() => {
        resizeFrameRef.current = null;
        if (activitySidebarRef.current) {
          activitySidebarRef.current.style.width = `${pendingActivityWidthRef.current}px`;
        }
      });
    };
    const onPointerMove = (moveEvent: PointerEvent) => move(moveEvent.clientX);
    const onMouseMove = (moveEvent: MouseEvent) => move(moveEvent.clientX);
    const onEnd = () => finishResize();

    window.addEventListener("pointermove", onPointerMove);
    window.addEventListener("pointerup", onEnd, { once: true });
    window.addEventListener("pointercancel", onEnd, { once: true });
    // WKWebView 和部分自动化输入只补发 mouse 事件，保留这层轻量兼容。
    window.addEventListener("mousemove", onMouseMove);
    window.addEventListener("mouseup", onEnd, { once: true });
    resizeCleanupRef.current = () => {
      window.removeEventListener("pointermove", onPointerMove);
      window.removeEventListener("pointerup", onEnd);
      window.removeEventListener("pointercancel", onEnd);
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", onEnd);
    };
  }, [clampActivityWidth, finishResize]);

  useEffect(() => {
    return () => {
      resizeCleanupRef.current?.();
      if (resizeFrameRef.current !== null) cancelAnimationFrame(resizeFrameRef.current);
      document.documentElement.classList.remove("activity-is-resizing");
    };
  }, []);

  // openFileInSidebar 从后端取文件内容，在侧栏并列展开（再次点击收起）。
  const openFileInSidebar = useCallback(async (rawPath: string) => {
    // file:// 链接可能是绝对路径（如 /Users/x/.../backend/src/x.go），
    // 也可能是 workspace 相对路径。统一转成 workspace 相对路径。
    let relPath = rawPath;
    const workspace = state.workspace;
    if (workspace && rawPath.startsWith(workspace)) {
      relPath = rawPath.slice(workspace.length);
    }
    // file:///xxx 截断后是 /xxx（带前导斜杠），write_file 的 path 是 xxx——统一去掉。
    relPath = relPath.replace(/^\/+/, "");
    setActivityOpen(true);
    // 已展开就收起（toggle）。
    if (openFiles[relPath] !== undefined) {
      setOpenFiles((files) => {
        const next = { ...files };
        delete next[relPath];
        return next;
      });
      return;
    }
    try {
      const res = await fetch(`/api/v1/sessions/${id}/file?path=${encodeURIComponent(relPath)}`);
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        setOpenFiles((files) => ({ ...files, [relPath]: `无法读取文件：${body.message ?? res.statusText}` }));
        return;
      }
      const body = await res.json();
      setOpenFiles((files) => ({ ...files, [body.path]: body.content }));
    } catch (e) {
      setOpenFiles((files) => ({ ...files, [relPath]: `加载失败：${e}` }));
    }
  }, [id, state.workspace, openFiles, setActivityOpen]);

  // 活动面板数据：浏览器操作（goseek-browser 命令）和它产出的截图。
  const activeIDs = new Set(state.memory.active_batches.map((batch) => batch.id));

  return (
    <div className="session">
      <header className="session-head">
        <span className="head-title" title={title || undefined}>
          {title}
        </span>
        <button
          type="button"
          className="workspace"
          title={`点击复制：${state.workspace}`}
          onClick={() => {
            // 工作目录是最常需要拿去终端用的东西。复制失败不打扰——
            // 非安全上下文下剪贴板不可用，用户还能手动选中。
            void navigator.clipboard?.writeText(state.workspace).catch(() => {});
          }}
        >
          {state.workspace || "…"}
        </button>
        <span className="head-spacer" />
        {streamStatus === "reconnecting" && (
          // 连接正常时不占版面——一个长期显示的"正常"标记只会让人忽略它，
          // 等它真的变红时反而注意不到。
          <span className="badge badge-reconnecting">连接断开，重连中…</span>
        )}
        <StateBadge state={state.runState} />
        <button
          className={`head-btn${turnIndexOpen ? " active" : ""}`}
          onClick={() => setTurnIndexOpen((open) => !open)}
          title="轮次目录"
          aria-label="轮次目录"
        >
          <Icon name="list" size={14} />
          <span className="head-btn-label">
            轮次
            {state.turns.length > 0 && ` · ${state.turns.length}`}
          </span>
        </button>
        <button
          className={`head-btn${memoryOpen ? " active" : ""}`}
          onClick={() => setMemoryOpen((open) => !open)}
          // 一个只能靠敲命令才能发现的功能等于不存在，所以这里有个常驻按钮；
          // /memory 命令切换的是同一个开关。
          title="摘要树（也可以在输入框敲 /memory）"
          aria-label="摘要树"
        >
          <Icon name="layers" size={14} />
          <span className="head-btn-label">
            摘要树
            {state.memory.active_batches.length > 0 &&
              ` · ${state.memory.active_batches.length}`}
          </span>
        </button>
        <button
          className={`head-btn${activityOpen ? " active" : ""}`}
          onClick={() => setActivityOpen((open) => !open)}
          title={activityOpen ? "收起侧栏" : "展开侧栏：浏览器页面、文件改动"}
          aria-label={activityOpen ? "收起活动侧栏" : "展开活动侧栏"}
        >
          <Icon name="pulse" size={14} />
          <span className="head-btn-label">
            活动
            {state.changedFiles.length > 0 && ` · ${state.changedFiles.length}`}
          </span>
        </button>
      </header>

      <div className="session-body">
        {turnIndexOpen && (
          <TurnIndex
            turns={state.turns}
            current={state.runState === "IDLE" || state.runState === "FAILED" ? undefined : state.turns.at(-1)?.id}
            onClose={() => setTurnIndexOpen(false)}
          />
        )}
        <Conversation
          turns={state.turns}
          foldedUpTo={foldedUpTo}
          notices={notices}
          queued={queued}
          ready={ready}
          onFileLinkClick={openFileInSidebar}
        />

        {memoryOpen && (
          <MemoryPanel
            tree={tree}
            messages={messages}
            activeIDs={activeIDs}
            busy={busy}
            onEdit={handleEdit}
            onClose={() => setMemoryOpen(false)}
          />
        )}

        {activityOpen && (
          <>
            <div
              className="activity-resizer"
              role="separator"
              aria-label="调整侧栏宽度"
              aria-orientation="vertical"
              onPointerDown={startResize}
            />
            <ActivitySidebar
              key={id}
              sidebarRef={activitySidebarRef}
              sessionID={id}
              onTabsUpdate={updateSidebarPages}
              openFiles={openFiles}
              width={activityWidth}
              onActivityOpen={() => setActivityOpen(true)}
              onCloseFile={(path) => {
                setOpenFiles((files) => {
                  const next = { ...files };
                  delete next[path];
                  return next;
                });
              }}
              onError={handleSidebarError}
              onClose={() => setActivityOpen(false)}
            />
          </>
        )}
      </div>

      <footer className="composer-area">
        {error && (
          <ErrorBar
            failure={error}
            onDismiss={() => setError(null)}
            // 重试就是重新加载这一页。刷新比"把每个失败的请求都记下来再重放"
            // 可靠得多：出错之后本地状态可能已经半新半旧。
            onRetry={() => window.location.reload()}
            onList={() => navigate("/")}
            onStop={() => void handleCancel()}
          />
        )}

        <Composer
          sessionID={id}
          ready={ready}
          busy={busy}
          runState={state.runState}
          sentHistory={sentHistory}
          onSubmit={handleSubmit}
          onCancel={handleCancel}
          skills={skills}
        />

        <div className="composer-meta">
          {/* 模型决定窗口，窗口决定占用；两个控件放在一起。 */}
          <ModelSelect
            models={models}
            current={state.modelInfo?.name || snapshotModel}
            busy={busy}
            onChange={handleModelChange}
          />
          <ContextGauge usage={state.usage} />
        </div>
        {state.changedFiles.length > 0 && (
          <div className="changed-files">
            <div className="changed-files-header">本轮改动</div>
            {state.changedFiles.map((path) => (
              <button
                key={path}
                type="button"
                className="changed-file"
                title={`在侧栏打开 ${path}`}
                onClick={() => void openFileInSidebar(path)}
              >
                {path}
              </button>
            ))}
          </div>
        )}
      </footer>
    </div>
  );
}

/** TurnIndex 是会话内轮次目录。 */
function TurnIndex({
  turns, current, onClose,
}: {
  turns: Turn[];
  current?: string;
  onClose: () => void;
}) {
  return (
    <aside className="turn-index">
      <header>
        <span>
          <Icon name="list" size={14} /> 轮次 · {turns.length}
        </span>
        <button onClick={onClose} title="关闭">
          <Icon name="close" size={14} />
        </button>
      </header>
      <ol>
        {turns.map((turn, index) => (
          <li key={turn.id} className={turn.id === current ? "current" : ""}>
            <button
              onClick={() => {
                document
                  .querySelector(`[data-turn-id="${CSS.escape(turn.id)}"]`)
                  ?.scrollIntoView({ block: "start" });
              }}
            >
              <span className="turn-index-number">{index + 1}</span>
              <span>{titleOfTurn(turn)}</span>
            </button>
          </li>
        ))}
      </ol>
    </aside>
  );
}

/** titleOfTurn 取一轮里第一条用户消息，找不到时给出占位。 */
function titleOfTurn(turn: Turn): string {
  const text = turn.items.find((item) => item.kind === "user");
  if (text?.kind !== "user") return "（没有用户消息）";
  const compact = text.text.replace(/\s+/g, " ").trim();
  if (!compact) return "（空消息）";
  return compact.length > 48 ? `${compact.slice(0, 48)}…` : compact;
}

/** ModelSelect 是会话级模型选择器。 */
function ModelSelect({
  models, current, busy, onChange,
}: {
  models: ModelInfo[];
  current: string;
  busy: boolean;
  onChange: (model: string) => Promise<void>;
}) {
  const currentInfo = models.find((model) => model.name === current);
  return (
    <select
      className="model-select"
      value={current}
      disabled={busy || models.length === 0}
      onChange={(event) => void onChange(event.target.value)}
      title={currentInfo ? describeModel(currentInfo) : current}
    >
      {models.map((model) => (
        <option key={model.name} value={model.name}>
          {model.display_name} · {compactWindow(model.effective_context_window)}
        </option>
      ))}
    </select>
  );
}

/** describeModel 给出完整窗口信息，select 的 tooltip 原样显示。 */
function describeModel(model: ModelInfo): string {
  return [
    `目录窗口 ${model.context_window.toLocaleString()}`,
    `有效窗口 ${model.effective_context_window.toLocaleString()}（95%）`,
    `压缩触发 ${model.compaction_trigger.toLocaleString()}（80%）`,
    `实测日期 ${model.measured_at}`,
  ].join("\n");
}

/** compactWindow 把 token 数压成 996K 这种短标签。 */
function compactWindow(tokens: number): string {
  if (tokens >= 1000) return `${Math.round(tokens / 1000)}K`;
  return tokens.toLocaleString();
}

/**
 * Conversation 是对话区：可滚动的容器 + 跟随策略 + 「回到底部」。
 *
 * 跟随策略见 scroll.ts——**只在用户已经在底部时才跟**。原来无条件滚到底，
 * 导致长对话里根本没法回头看。
 */
function Conversation({
  turns, foldedUpTo, notices, queued, ready, onFileLinkClick,
}: {
  turns: Turn[];
  foldedUpTo: number;
  notices: string[];
  queued: string[];
  ready: boolean;
  onFileLinkClick?: (path: string) => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const [atBottom, setAtBottom] = useState(true);
  // 用对象而不是 boolean：并发渲染下 ref.current 的写入时机不受我们控制，
  // 布尔在重渲染后可能被丢弃导致重做；done 里记录的是"已为哪个会话定位过"，
  // 切会话自然失效，不需要显式复位。
  const initialScrollDone = useRef<{ done: boolean }>({ done: false });
  /** 用户主动滚过之后，补滚必须让位——不打扰是底线。 */
  const userScrolled = useRef(false);

  // 打开一个已有历史的会话时先到最新回复。
  //
  // 双帧 rAF + 一次延迟补滚：快照渲染那一帧，代码块和表格常常还没撑开布局，
  // 单帧 rAF 拿到的 scrollHeight 是旧值，于是"定位到底"差一截。补滚只在用户
  // 还没主动滚动时执行；之后的新事件遵循 shouldFollow，不再强行拉回。
  useEffect(() => {
    if (!ready) {
      initialScrollDone.current.done = false;
      userScrolled.current = false;
      return;
    }
    const element = box.current;
    if (!element || initialScrollDone.current.done) return;
    initialScrollDone.current.done = true;
    const toBottom = () => {
      if (!userScrolled.current) {
        element.scrollTop = element.scrollHeight;
        setAtBottom(true);
      }
    };
    const first = requestAnimationFrame(() => {
      requestAnimationFrame(toBottom);
    });
    const late = setTimeout(toBottom, 120);
    return () => {
      cancelAnimationFrame(first);
      clearTimeout(late);
    };
  }, [ready]);

  // 内容变化时按策略决定要不要跟随。读的是 DOM 的实时度量，不是 state——
  // state 会滞后一帧，而这里判断的正是"此刻用户在哪"。
  useEffect(() => {
    const element = box.current;
    if (!element) return;
    if (shouldFollow(element)) {
      element.scrollTop = element.scrollHeight;
    }
  }, [turns, notices, queued]);

  if (!ready) {
    // 加载骨架，不白屏。切会话时快照要往返一次，这期间给个轮廓比给一片空白强。
    return (
      <div className="conversation">
        <div className="skeleton">
          <div className="skeleton-line short" />
          <div className="skeleton-line" />
          <div className="skeleton-line" />
          <div className="skeleton-line short" />
        </div>
      </div>
    );
  }

  return (
    <div
      className="conversation"
      ref={box}
      onScroll={(event) => {
        setAtBottom(isAtBottom(event.currentTarget));
        // 初始定位期间程序自己也在滚。区分不了来源，就用高度判断：
        // 只有"离开底部"才算用户意图——定位滚到底那一下不算。
        if (!isAtBottom(event.currentTarget)) userScrolled.current = true;
      }}
    >
      <Transcript turns={turns} foldedUpTo={foldedUpTo} onFileLinkClick={onFileLinkClick} />

      {queued.map((text, index) => (
        // 排队中的消息：和真实用户气泡同一位置、同一形状，只是淡一点并挂一个
        // 「已排队」角标——形状一变，用户会以为这条没发出去。
        <div key={`queued-${index}`} className="row row-user">
          <div className="bubble-user queued">
            {text}
            <span className="queued-tag">已排队</span>
          </div>
        </div>
      ))}

      {notices.map((text, index) => (
        // key 用下标：提示只会往后追加，不重排。
        <pre key={index} className="notice">{text}</pre>
      ))}

      {!atBottom && (
        <button
          className="to-bottom"
          title="回到底部"
          onClick={() => {
            const element = box.current;
            if (element) element.scrollTop = element.scrollHeight;
          }}
        >
          <Icon name="arrowDown" size={16} />
        </button>
      )}
    </div>
  );
}

/**
 * describeFailure 把一次失败翻译成一句人能照着做的话。
 *
 * 按后端给的 `code` 分支而不是匹配 message 的文案：文案随时会被改写，code 是契约。
 */
function describeFailure(failure: unknown, fallback: string): Failure {
  if (failure instanceof ApiFailure) {
    switch (failure.code) {
      case "turn_in_progress":
        // 有出路：那一轮现在可以被停掉（M5.2 加的 CancelTurn）。
        return { text: "这个会话正有一轮在跑。", action: "stop" };
      case "batch_not_active":
        // 无出路，但这条本来就不是故障——说清楚"改它没用"就够了。
        return { text: "这段摘要已经被合并进上层，改它不会影响模型看到的内容。" };
      case "session_busy":
        return {
          text: "这个会话正被别的进程打开（比如终端里的 goseek），先关掉那边再刷新。",
          action: "retry",
        };
      case "session_not_found":
        return { text: "会话不存在，可能已经被删掉了。", action: "list" };
      // http_404 是 request() 在**解析不出 JSON 错误体**时兜底造出来的 code。
      // 后端所有的业务 404 都带 {"error":{"code":"session_not_found"}}，所以走到
      // 这里只有一种可能：这条路由在后端根本不存在——也就是后端比前端旧。
      //
      // 这一条是真实踩过的：前端由 Vite 热更新、随代码即时生效，后端却要手动
      // 重新 go install，两边很容易错位。不给提示的话，界面只会说"请求失败
      // （HTTP 404）"，人会去查会话是不是没了。
      case "http_404":
        return {
          text: "这个接口在后端不存在，多半是后端版本比面板旧。到 backend 目录下重新 go install ./cmd/goseek 再重启服务。",
          action: "retry",
        };
      default:
        return { text: failure.message };
    }
  }
  // 走到这里通常是 fetch 本身失败——后端没起、端口不通。这类错误重试有意义：
  // 用户去把服务起起来，然后点一下。
  return {
    text: failure instanceof Error ? `${fallback}：${failure.message}` : fallback,
    action: "retry",
  };
}

/** 一条错误提示：一句话，外加一个"现在能做什么"。 */
interface Failure {
  text: string;
  /**
   * 可选动作。用枚举而不是回调，好让 describeFailure 保持是纯函数。
   *
   *   retry  重新加载这一页
   *   list   回到会话列表（会话已经不存在了，留在这一页没有意义）
   *   stop   停止正在跑的那一轮
   */
  action?: "retry" | "list" | "stop";
}

/** ErrorBar 把一条 Failure 画成"一句话 + 一个按钮"。 */
function ErrorBar({
  failure, onDismiss, onRetry, onList, onStop,
}: {
  failure: Failure;
  onDismiss: () => void;
  onRetry: () => void;
  onList: () => void;
  onStop: () => void;
}) {
  const actions = {
    retry: { label: "重试", run: onRetry },
    list: { label: "回到列表", run: onList },
    stop: { label: "停止那一轮", run: onStop },
  };
  const action = failure.action ? actions[failure.action] : null;

  return (
    <div className="error-bar" role="alert">
      <Icon name="alert" size={15} />
      <span className="error-text">{failure.text}</span>
      {action && (
        <button className="link" onClick={action.run}>
          {action.label}
        </button>
      )}
      <button className="link" onClick={onDismiss}>
        知道了
      </button>
    </div>
  );
}

/** StateBadge 显示 Agent 此刻在做什么；等待模型时带实时耗时。 */
function StateBadge({ state }: { state: RunState }) {
  const labels: Record<RunState, string> = {
    IDLE: "空闲",
    WAITING_MODEL: "等待模型",
    RUNNING_TOOL: "执行命令",
    // 压缩要单独成一档：它可能持续几十秒（每个摘要节点都是一次模型调用），
    // 混在"等待模型"里会让用户以为卡住了。
    COMPRESSING: "整理上下文",
    FAILED: "上一轮失败",
  };
  const [elapsed, setElapsed] = useState(0);

  // 进入非空闲状态的瞬间开始计时；离开后自动停止，下一次重新归零。
  // 放在这里而不是顶层 state：耗时只服务这个徽标，不参与对话状态。
  useEffect(() => {
    if (state === "IDLE" || state === "FAILED") return;
    const startedAt = Date.now();
    setElapsed(0);
    const timer = setInterval(() => {
      setElapsed(Math.floor((Date.now() - startedAt) / 1000));
    }, 1000);
    return () => clearInterval(timer);
  }, [state]);

  const suffix = state === "WAITING_MODEL" || state === "COMPRESSING"
    ? ` · ${formatElapsed(elapsed)}`
    : "";
  return (
    <span className={`badge badge-${state.toLowerCase()}`}>
      {labels[state]}{suffix}
    </span>
  );
}

/** formatElapsed 把秒数压成 1m 32s 这种好扫读的形态。 */
function formatElapsed(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}


/** ActivitySidebar：右侧活动面板——页面栈 + tab 栏，覆盖整个侧栏。 */
function ActivitySidebar({
  sidebarRef, sessionID, openFiles, width, onCloseFile, onClose, onActivityOpen,
  onTabsUpdate, onError,
}: {
  sidebarRef: React.RefObject<HTMLElement | null>;
  sessionID: string;
  openFiles: Record<string, string>;
  width: number;
  onCloseFile: (path: string) => void;
  onClose: () => void;
  onActivityOpen: () => void;
  onTabsUpdate: (urls: string[]) => void;
  onError: (failure: unknown) => void;
}) {
  // pages: 浏览器页面（从 /tabs 轮询）+ 打开的文件，统一为一个列表。
  const [browserPages, setBrowserPages] = useState<BrowserTab[]>([]);
  const [tabPreview, setTabPreview] = useState("");
  
  // 打开的文件列表（作为 tab 叠在页面栏）。
  const fileTabs = Object.keys(openFiles).map((path) => ({
    key: `file:${path}`,
    title: path.split("/").pop() || path,
    kind: "file" as const,
    detail: path,
  }));
  const allTabs = [
    ...browserPages.map((t) => ({ key: `tab:${t.id}`, title: t.url, kind: "browser" as const, detail: t.id, url: t.url })),
    ...fileTabs,
  ];
  // 当前激活的 tab（默认最后一个）。
  const [activeKey, setActiveKey] = useState<string>("");
  const current = allTabs.find((t) => t.key === activeKey) ?? allTabs[allTabs.length - 1] ?? null;

  const applyBrowserTabs = useCallback((tabs: BrowserTab[]) => {
    setBrowserPages(tabs);
    onTabsUpdate(tabs.map((tab) => tab.url));
  }, [onTabsUpdate]);

  // 首次展开立即拉取，随后轮询 agent 新开的页面。
  useEffect(() => {
    let cancelled = false;
    const pull = async () => {
      try {
        const body = await browserTabs(sessionID);
        if (!cancelled) applyBrowserTabs(body.tabs ?? []);
      } catch {
        // 短暂失败保留上一帧；下一轮继续尝试。
      }
    };
    void pull();
    const interval = setInterval(() => { void pull(); }, 2000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [sessionID, applyBrowserTabs]);

  // 激活某个浏览器 tab。
  const activateTab = useCallback(async (targetId: string) => {
    await browserActivateTab(sessionID, targetId);
  }, [sessionID]);

  // 地址栏输入后新开页面，并显式选中后端返回的 target。
  const [newTabURL, setNewTabURL] = useState("");
  const openNewPage = useCallback(async () => {
    let url = newTabURL.trim();
    if (!url) return;
    // 没有协议就默认 https://
    if (!url.startsWith("http://") && !url.startsWith("https://")) {
      url = "https://" + url;
    }
    onActivityOpen();
    try {
      const opened = await browserNavigate(sessionID, url);
      const optimisticTab: BrowserTab = {
        id: opened.targetId,
        url,
        title: url,
      };
      applyBrowserTabs([
        ...browserPages.filter((tab) => tab.id !== opened.targetId),
        optimisticTab,
      ]);
      setActiveKey(`tab:${opened.targetId}`);
      setNewTabURL("");
      await activateTab(opened.targetId);

      // Chrome 可能稍后才把新 target 的标题与最终 URL 暴露给 /tabs。
      // targetId 已经证明页面创建成功；此处只在元数据就绪时补齐显示。
      const body = await browserTabs(sessionID);
      if ((body.tabs ?? []).some((tab) => tab.id === opened.targetId)) {
        applyBrowserTabs(body.tabs ?? []);
      }
    } catch (failure) {
      onError(failure);
    }
  }, [
    newTabURL,
    sessionID,
    browserPages,
    applyBrowserTabs,
    activateTab,
    onActivityOpen,
    onError,
  ]);

  // 文件 tab 只关闭本地视图；浏览器 tab 必须关闭真实 CDP target。
  const closeTab = useCallback(async (t: { key: string; kind: string; detail: string }) => {
    if (t.kind === "file") {
      onCloseFile(t.detail);
      if (current?.key === t.key) {
        const others = allTabs.filter((x) => x.key !== t.key);
        setActiveKey(others.length > 0 ? others[others.length - 1].key : "");
      }
      return;
    }
    try {
      const body = await browserCloseTab(sessionID, t.detail);
      const tabs = body.tabs ?? [];
      applyBrowserTabs(tabs);
      if (current?.key === t.key) {
        const remainingKeys = [
          ...tabs.map((tab) => `tab:${tab.id}`),
          ...fileTabs.map((tab) => tab.key),
        ];
        setActiveKey(remainingKeys.length > 0 ? remainingKeys[remainingKeys.length - 1] : "");
      }
    } catch (failure) {
      onError(failure);
    }
  }, [sessionID, current, allTabs, fileTabs, onCloseFile, applyBrowserTabs, onError]);

  const revealTab = useCallback((tab: HTMLElement) => {
    const reveal = () => tab.scrollIntoView({ block: "nearest", inline: "nearest" });
    requestAnimationFrame(reveal);
    window.setTimeout(reveal, 190);
  }, []);

  const scrollTabs = useCallback((event: React.WheelEvent<HTMLDivElement>) => {
    const strip = event.currentTarget;
    if (strip.scrollWidth <= strip.clientWidth) return;
    const delta = Math.abs(event.deltaX) > Math.abs(event.deltaY) ? event.deltaX : event.deltaY;
    if (delta === 0) return;
    event.preventDefault();
    strip.scrollLeft += delta;
  }, []);

  return (
    <aside ref={sidebarRef} className="activity-sidebar" style={{ width }}>
      <div className="activity-address-bar">
        <div className="activity-address-field">
          <Icon name="globe" size={14} />
          <input
            className="activity-address-input"
            aria-label="页面地址"
            placeholder="输入 URL 打开页面…"
            value={newTabURL}
            onChange={(e) => setNewTabURL(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") void openNewPage(); }}
          />
        </div>
        <button className="activity-add-btn" onClick={() => void openNewPage()} title="跳转到此页面">
          <Icon name="send" size={15} />
        </button>
      </div>
      <header className="activity-head">
        <div className="activity-tab-strip" onWheel={scrollTabs}>
          {allTabs.length === 0 && <span className="activity-tab-empty">侧边栏</span>}
          {allTabs.map((t) => (
            <div
              key={t.key}
              className={`activity-tab${current?.key === t.key ? " active" : ""}`}
              data-target-id={t.kind === "browser" ? t.detail : undefined}
              data-page-url={t.kind === "browser" ? t.url : undefined}
              data-file-path={t.kind === "file" ? t.detail : undefined}
              onPointerEnter={(event) => {
                setTabPreview(t.kind === "browser" ? t.url ?? t.title : t.detail);
                revealTab(event.currentTarget);
              }}
              onPointerLeave={() => setTabPreview("")}
              onFocusCapture={(event) => {
                setTabPreview(t.kind === "browser" ? t.url ?? t.title : t.detail);
                revealTab(event.currentTarget);
              }}
              onBlurCapture={(event) => {
                if (!event.currentTarget.contains(event.relatedTarget as Node | null)) {
                  setTabPreview("");
                }
              }}
              title={t.title}
            >
              <button
                type="button"
                className="activity-tab-target"
                onClick={() => {
                  setActiveKey(t.key);
                  if (t.kind === "browser") void activateTab(t.detail);
                }}
              >
                <Icon name={t.kind === "browser" ? "globe" : "file"} size={13} />
                <span className="activity-tab-label">{t.title}</span>
              </button>
              <button
                type="button"
                className="activity-tab-close"
                onClick={(e) => {
                  e.stopPropagation();
                  void closeTab(t);
                }}
                title="关闭此页"
                aria-label={
                  t.kind === "browser"
                    ? `关闭 ${t.title} (${t.url})`
                    : `关闭文件 ${t.detail}`
                }
              >
                <Icon name="close" size={11} />
              </button>
            </div>
          ))}
        </div>
        <button className="activity-close" onClick={onClose} title="收起侧栏">
          <Icon name="close" size={12} />
        </button>
        {tabPreview && <div className="activity-tab-preview">{tabPreview}</div>}
      </header>

      {/* 内容区域：覆盖整个侧栏（tab 栏下方全部空间） */}
      <div className="activity-content">
        {current?.kind === "browser" && (
          <BrowserSurface
            key={current.key}
            sessionID={sessionID}
            targetID={current.detail}
            title={current.title}
          />
        )}
        {current?.kind === "file" && (
          <pre className="activity-file-content">{openFiles[current.detail]}</pre>
        )}
        {!current && (
          <div className="activity-empty">打开页面或文件后，这里会显示内容。</div>
        )}
      </div>
    </aside>
  );
}

const browserSpecialKeys = new Set([
  "Backspace", "Delete", "Enter", "Tab", "Escape",
  "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
  "Home", "End", "PageUp", "PageDown",
]);

function BrowserSurface({
  sessionID, targetID, title,
}: {
  sessionID: string;
  targetID: string;
  title: string;
}) {
  const stageRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const viewportRef = useRef({ width: 960, height: 640 });
  const surfaceSizeRef = useRef({ width: 960, height: 640 });
  const viewportRequestRef = useRef(0);
  const pendingScrollRef = useRef<{ x: number; y: number; deltaY: number } | null>(null);
  const scrollFrameRef = useRef<number | null>(null);
  const [streamNonce, setStreamNonce] = useState(0);
  const [loaded, setLoaded] = useState(false);
  const [zoomMode, setZoomMode] = useState<"fit" | number>("fit");
  const [appliedScale, setAppliedScale] = useState(1);

  useEffect(() => {
    const stage = stageRef.current;
    if (!stage) return;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let cancelled = false;
    const applyViewport = async (width: number, height: number) => {
      const requestID = ++viewportRequestRef.current;
      try {
        const viewport = await browserSetViewport(
          sessionID,
          targetID,
          width,
          height,
          zoomMode === "fit" ? 1 : zoomMode,
          zoomMode === "fit",
        );
        if (cancelled || requestID !== viewportRequestRef.current) return;
        viewportRef.current = { width: viewport.width, height: viewport.height };
        setAppliedScale(viewport.scale);
      } catch {
        // 页面切换或关闭时可能取消请求；下一次 resize/轮询会重新同步。
      }
    };
    const schedule = (width: number, height: number) => {
      surfaceSizeRef.current = { width, height };
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => {
        void applyViewport(width, height);
      }, 90);
    };
    const observer = new ResizeObserver(([entry]) => {
      const width = Math.max(1, Math.round(entry.contentRect.width));
      const height = Math.max(1, Math.round(entry.contentRect.height));
      schedule(width, height);
    });
    observer.observe(stage);
    const interval = zoomMode === "fit"
      ? window.setInterval(() => {
          const { width, height } = surfaceSizeRef.current;
          void applyViewport(width, height);
        }, 2000)
      : null;
    return () => {
      cancelled = true;
      observer.disconnect();
      if (timer) clearTimeout(timer);
      if (interval !== null) clearInterval(interval);
    };
  }, [sessionID, targetID, zoomMode]);

  useEffect(() => {
    return () => {
      if (scrollFrameRef.current !== null) cancelAnimationFrame(scrollFrameRef.current);
      scrollFrameRef.current = null;
      pendingScrollRef.current = null;
    };
  }, [sessionID, targetID]);

  const pointInViewport = (event: React.MouseEvent | React.WheelEvent) => {
    const stage = stageRef.current;
    if (!stage) return null;
    const rect = stage.getBoundingClientRect();
    return {
      x: ((event.clientX - rect.left) / rect.width) * viewportRef.current.width,
      y: ((event.clientY - rect.top) / rect.height) * viewportRef.current.height,
    };
  };

  const zoomSteps = [0.25, 0.33, 0.5, 0.67, 0.8, 1];
  const changeZoom = (direction: -1 | 1) => {
    const current = zoomMode === "fit" ? appliedScale : zoomMode;
    if (direction < 0) {
      const next = [...zoomSteps].reverse().find((step) => step < current - 0.01);
      if (next) setZoomMode(next);
      return;
    }
    const next = zoomSteps.find((step) => step > current + 0.01);
    if (next) setZoomMode(next);
  };

  return (
    <div
      ref={stageRef}
      className={`browser-surface${loaded ? " loaded" : ""}`}
      aria-label={`页面 ${title}`}
      onClick={(event) => {
        const point = pointInViewport(event);
        if (point) void browserClick(sessionID, targetID, point.x, point.y);
        inputRef.current?.focus({ preventScroll: true });
      }}
      onWheel={(event) => {
        const point = pointInViewport(event);
        if (!point) return;
        event.preventDefault();
        const pending = pendingScrollRef.current;
        pendingScrollRef.current = pending
          ? { x: point.x, y: point.y, deltaY: pending.deltaY + event.deltaY }
          : { x: point.x, y: point.y, deltaY: event.deltaY };
        if (scrollFrameRef.current !== null) return;
        scrollFrameRef.current = requestAnimationFrame(() => {
          scrollFrameRef.current = null;
          const scroll = pendingScrollRef.current;
          pendingScrollRef.current = null;
          if (scroll) {
            void browserScroll(
              sessionID,
              targetID,
              scroll.x,
              scroll.y,
              scroll.deltaY,
            );
          }
        });
      }}
    >
      <div className="browser-loading">正在连接页面…</div>
      <img
        className="browser-live-frame"
        src={browserStreamURL(sessionID, targetID, streamNonce)}
        alt=""
        draggable={false}
        onLoad={() => setLoaded(true)}
        onError={() => {
          setLoaded(false);
          window.setTimeout(() => setStreamNonce((value) => value + 1), 800);
        }}
      />
      <div
        className="browser-zoom-controls"
        onClick={(event) => event.stopPropagation()}
        onWheel={(event) => event.stopPropagation()}
      >
        <button
          type="button"
          className="browser-zoom-button"
          onClick={() => changeZoom(-1)}
          title="缩小页面"
          aria-label="缩小页面"
        >
          <Icon name="minus" size={12} />
        </button>
        <span className="browser-zoom-value">{Math.round(appliedScale * 100)}%</span>
        <button
          type="button"
          className={`browser-zoom-button${zoomMode === "fit" ? " active" : ""}`}
          onClick={() => setZoomMode("fit")}
          title="适应侧栏宽度"
          aria-label="适应侧栏宽度"
        >
          <Icon name="expand" size={12} />
        </button>
        <button
          type="button"
          className="browser-zoom-button"
          onClick={() => changeZoom(1)}
          title="放大页面"
          aria-label="放大页面"
        >
          <Icon name="plus" size={12} />
        </button>
      </div>
      <textarea
        ref={inputRef}
        className="browser-keyboard-capture"
        aria-label="向当前页面输入"
        value=""
        onChange={(event) => {
          if (event.target.value) {
            void browserInsertText(sessionID, targetID, event.target.value);
          }
        }}
        onKeyDown={(event) => {
          if (!browserSpecialKeys.has(event.key)) return;
          event.preventDefault();
          void browserPressKey(sessionID, targetID, event.key);
        }}
      />
    </div>
  );
}
