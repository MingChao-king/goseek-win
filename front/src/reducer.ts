// 会话详情页的状态与它的 reducer。
//
// # 为什么用 reducer
//
// 事件流本来就是一串"发生了什么"，而 reducer 的形状正是 (旧状态, 一件事) → 新状态。
// 快照是初始状态，之后每个事件 reduce 一次——这与后端"重放 durable 事件能还原
// 发生过什么"是同一件事，只是发生在浏览器里。
//
// 换成一堆 useState 的话，"收到 tool.resolved 时要同时改工具状态、清掉输出缓冲、
// 把状态切回等待模型"这种一件事引起多处变化的逻辑，会散在好几个 setState 里，
// 很容易只改了一半。reducer 把它们收在一个 case 里。
//
// # reducer 必须是纯函数
//
// 给同样的输入永远得到同样的输出，不发请求，**也不修改传进来的旧状态**。
// React 的 StrictMode 在开发模式下会用同样的输入调它两次，专门用来暴露不纯的
// 实现——本文件就栽过一次：`last.text += text` 看起来只是改了新数组里的元素，
// 但浅拷贝出来的数组和旧数组共享同一批元素对象，于是那次赋值同时改到了旧状态，
// 两次调用把同一段文字追加了两遍，界面上出现"让我让我理解理解一下一下"。
//
// 因此下面所有的"修改"都是**构造新对象**：数组用展开语法重建，对象用 {...old}
// 加覆盖字段。reducer.test.ts 里有一条测试专门模拟 StrictMode 的双调用。

import {
  assistantMessageOf,
  compactionCompletedOf,
  compactionStartedOf,
  contextUsageOf,
  reasonOf,
  stateOf,
  imageIDsOf, textOf,
  toolImageIDsOf,
  toolOutputOf,
  toolResultOf,
  toolStartedOf,
} from "./payload";
import type {
  CompactionResult, ContextUsage, Message, MessageImage, ModelInfo, RunEvent, RunState, SessionMemory,
  SessionSnapshot, ToolCall, ToolResult,
} from "./types";

/** 一次工具调用及其观察，界面上是可展开的一步。 */
export interface ToolStep {
  call: ToolCall;
  /** title 是模型自述的意图；参数原文才是事实，两者都展示。 */
  title: string;
  /** output 是实时累积的命令输出。 */
  output: string;
  /** result 在调用结束后出现，之前为 null。 */
  result: ToolResult | null;
}

/**
 * 一次压缩在界面上的一步。
 *
 * 它出现在**发生压缩的那一轮里**，位置就在用户提问之后、模型回答之前——那正是
 * 它真实发生的时刻。放在顶部当一条全局提示的话，用户就看不出"是哪次提问触发的
 * 压缩"，而这恰恰是理解上下文为什么变短的关键。
 *
 * result 为 null 表示压缩仍在进行中：界面据此显示"正在整理上下文"。
 */
export interface CompactionStep {
  /** 触发时的占用与触发线，回答"为什么现在压"。 */
  inputTokens: number;
  threshold: number;
  /** 压缩结果。进行中为 null。 */
  result: CompactionResult | null;
}

/** 一轮交互里的一个片段。界面按顺序渲染它们。 */
export type TurnItem =
  | { kind: "user"; text: string; images?: MessageImage[] }
  | { kind: "reasoning"; text: string }
  | { kind: "assistant"; text: string; final: boolean }
  | { kind: "tool"; step: ToolStep }
  | { kind: "compaction"; step: CompactionStep }
  | { kind: "failed"; reason: string };

/** 一轮交互。 */
export interface Turn {
  id: string;
  items: TurnItem[];
  /**
   * 这一轮最后一条消息在完整历史里的序号（从 1 开始）。0 表示未知。
   *
   * 只有从快照重建的轮次有这个值——那时才知道每条消息在数组里的位置。事件流建出
   * 来的轮次是最新的，必然在压缩游标之后，用不到它。
   *
   * 它唯一的用途是在对话流里画出**折叠边界**：游标之前的消息，模型看到的是摘要
   * 而不是原文。用户不知道这条线在哪，就会把模型对早期细节的模糊回答当成模型
   * 变笨了，而实际上那些细节确实不在它眼前。
   */
  endMessage: number;
}

/** 会话详情页的全部状态。 */
export interface SessionState {
  turns: Turn[];
  /** Agent 此刻在做什么。 */
  runState: RunState;
  /** 已经消费到的最大事件序号，重连时从它往后要。 */
  lastSequence: number;
  workspace: string;
  /**
   * 最近一次上下文占用。null 表示这个会话还没请求过模型。
   *
   * 它不属于任何一轮，因此放在顶层而不是 Turn 里：界面上它是一个持续可见的仪表，
   * 反映"下一次请求会占多少"，而不是某一轮的历史记录。
   */
  usage: ContextUsage | null;
  /**
   * 这个会话的压缩现状：哪几段历史被折叠了、折叠到第几条为止。
   *
   * 初始值来自快照（压缩事件是历史事件，不会被重放），之后每次压缩完成时更新。
   * 它和 turns 里的 CompactionStep 是两回事：那些是"发生过哪几次压缩"的流水，
   * 这个是"现在的上下文长什么样"的现状。
   */
  memory: SessionMemory;
  /** 当前模型的窗口信息。 */
  modelInfo: ModelInfo;
  /** 本轮会话中被 write_file 修改过的文件路径列表。 */
  changedFiles: string[];
}

/** emptyMemory 是"一条都没折叠"的记忆现状。 */
const emptyMemory: SessionMemory = {
  raw_compaction_cursor: 1,
  active_batches: [],
  total_batches: 0,
};

/** reducer 接受的动作：加载快照，或收到一个事件。 */
export type Action =
  | { type: "snapshot"; snapshot: SessionSnapshot }
  | { type: "event"; event: RunEvent };

/** initialState 是还没加载出快照时的空状态。 */
export const initialState: SessionState = {
  turns: [],
  runState: "IDLE",
  lastSequence: 0,
  workspace: "",
  usage: null,
  memory: emptyMemory,
  modelInfo: {
    name: "",
    display_name: "",
    context_window: 0,
    effective_context_window: 0,
    compaction_trigger: 0,
    window_source: "unknown",
    measured_at: "",
  },
  changedFiles: [],
};

/**
 * reduce 是这个页面唯一改状态的地方。
 *
 * 每个 case 都返回**新的对象**而不是就地修改：React 靠引用是否变化来判断要不要
 * 重新渲染，原地 push 一个数组不会改变它的引用，界面就不会更新。
 */
export function reduce(state: SessionState, action: Action): SessionState {
  switch (action.type) {
    case "snapshot":
      return fromSnapshot(action.snapshot);
    case "event":
      return applyEvent(state, action.event);
  }
}

/**
 * fromSnapshot 用快照重建状态，作为事件流的起点。
 *
 * 这里的 push 是安全的：turns 和它里面的对象都是本函数刚建出来的，没有任何
 * 外部引用，改它们不会影响别处。真正要避免的是修改**传进来的旧状态**。
 */
function fromSnapshot(snapshot: SessionSnapshot): SessionState {
  const turns: Turn[] = [];
  snapshot.messages.forEach((message, index) => {
    let turn = turns.find((candidate) => candidate.id === message.turn_id);
    if (!turn) {
      turn = { id: message.turn_id, items: [], endMessage: 0 };
      turns.push(turn);
    }
    // 记下这一轮覆盖到第几条消息。同一轮的消息是连续的，因此不断覆盖即可。
    turn.endMessage = index + 1;
    appendSnapshotMessage(turn, message, snapshot.messages);
  });
  return {
    turns,
    // 初始运行状态来自快照：思考阶段不产生 state.changed 事件，事件流重放救
    // 不了它——"思考中切走再切回显示空闲"就出在这里。?? 兜底对付旧后端。
    runState: snapshot.run_state ?? "IDLE",
    lastSequence: snapshot.last_sequence,
    workspace: snapshot.workspace,
    // 占用来自快照（后端现算）。这一条是补上的：原来这里写的是 null，理由是
    // "占用描述的是下一次请求，而快照是历史"——听起来成立，实际后果是**刷新
    // 页面之后仪表盘就消失**，直到用户再发一条消息。那个数字后端随时算得出，
    // 让它跟快照一起过来就好。
    //
    // ?? 兜底是为了对付比前端旧的后端（那时这个字段还不存在）。
    usage: snapshot.usage ?? null,
    // 压缩现状则相反，必须从快照来：压缩事件早已过期，事件流只从 last_sequence
    // 之后订阅，刷新页面后不会重放到它们。
    memory: snapshot.memory ?? emptyMemory,
    modelInfo: snapshot.model_info ?? initialState.modelInfo,
    changedFiles: collectChangedFiles(snapshot.messages),
  };
}

/** collectChangedFiles 从快照消息中提取 write_file 调用过的路径（去重、按时间顺序）。 */
function collectChangedFiles(messages: Message[]): string[] {
  const paths: string[] = [];
  for (const message of messages) {
    if (message.role !== "assistant") continue;
    for (const call of message.tool_calls ?? []) {
      if (call.name !== "write_file") continue;
      try {
        const args = JSON.parse(String(call.arguments)) as { path?: string };
        if (args.path && !paths.includes(args.path)) paths.push(args.path);
      } catch { /* 参数不是合法 JSON 就跳过 */ }
    }
  }
  return paths;
}

/** appendSnapshotMessage 把一条历史消息还原成界面片段。 */
function appendSnapshotMessage(turn: Turn, message: Message, all: Message[]): void {
  switch (message.role) {
    case "user":
      turn.items.push({ kind: "user", text: message.content, images: message.images });
      break;

    case "assistant": {
      const calls = message.tool_calls ?? [];
      if (message.content) {
        turn.items.push({ kind: "assistant", text: message.content, final: calls.length === 0 });
      }
      // 历史里的工具调用要连同它的观察一起还原。观察存在另一条 tool 消息里，
      // 靠 tool_call_id 找回来——这正是后端那条"每个调用恰好一条同 id 的观察"
      // 的不变量在前端的用处。
      for (const call of calls) {
        turn.items.push({
          kind: "tool",
          step: { call, title: "", output: "", result: findResult(all, call.id) },
        });
      }
      break;
    }

    case "tool":
      // 观察已经在上面挂到对应的调用上了，这里不再单独渲染。
      break;
  }
}

/** findResult 在历史里找出某次调用的观察。 */
function findResult(all: Message[], toolCallID: string): ToolResult | null {
  for (const message of all) {
    if (message.role !== "tool" || message.tool_call_id !== toolCallID) continue;
    try {
      // tool 消息的正文是整个 ToolResult 的 JSON——后端刻意这么存，
      // 只放命令输出会丢掉"成功但无输出""非零退出""根本没执行"的区别。
      return JSON.parse(message.content) as ToolResult;
    } catch {
      return null;
    }
  }
  return null;
}

/** applyEvent 把一个事件应用到状态上，返回新的状态。 */
function applyEvent(state: SessionState, event: RunEvent): SessionState {
  // durable 事件带序号，重复送达时跳过。
  //
  // SSE 断线重连会从指定序号之后重放，重放窗口与实时窗口可能有重叠；没有这道
  // 判断的话，同一条消息会在界面上出现两次。transient 事件（各种 delta）序号
  // 恒为 0，不参与去重——它们不会被重放，只可能是刚产生的。
  if (event.sequence > 0 && event.sequence <= state.lastSequence) {
    return state;
  }

  return {
    ...state,
    turns: updateTurn(state.turns, event.turn_id, (items) => nextItems(items, event)),
    runState: nextRunState(state.runState, event),
    usage: nextUsage(state.usage, event),
    memory: nextMemory(state.memory, event),
    changedFiles: nextChangedFiles(state.changedFiles, event),
    // transient 事件序号为 0，不能让它把已消费位置退回去。
    lastSequence: Math.max(state.lastSequence, event.sequence),
  };
}

/** nextChangedFiles 记录 write_file 工具修改过的文件路径。 */
function nextChangedFiles(current: string[], event: RunEvent): string[] {
  if (event.type !== "file.changed") return current;
  const payload = event.payload;
  if (typeof payload !== "object" || payload === null || !("path" in payload)) return current;
  const path = (payload as Record<string, unknown>)["path"];
  if (typeof path !== "string" || path === "") return current;
  if (current.includes(path)) return current;
  return [...current, path];
}

/** nextRunState 返回事件之后的运行状态。 */
function nextRunState(current: RunState, event: RunEvent): RunState {
  if (event.type === "turn.started") return "WAITING_MODEL";
  if (event.type !== "state.changed") return current;
  return stateOf(event.payload) ?? current;
}

/**
 * nextUsage 返回事件之后的上下文占用。
 *
 * 一轮里会收到两次：请求发出前是本地估算，响应回来后是供应商实测。两次都更新——
 * 界面上有一个持续可见的仪表，先看到估算再被校正成实测是有意义的反馈。终端那边
 * 只显示实测那次，因为两行数字一前一后跳动会让人以为出了问题。
 */
function nextUsage(current: ContextUsage | null, event: RunEvent): ContextUsage | null {
  if (event.type !== "context.usage.updated") return current;
  return contextUsageOf(event.payload) ?? current;
}

/**
 * nextMemory 返回事件之后的压缩现状。
 *
 * 只在压缩成功完成时更新。失败时保持原样——那次压缩没有改变上下文的构成，
 * 把一个"压了但没压成"的中间状态显示出来只会让人困惑。
 */
function nextMemory(current: SessionMemory, event: RunEvent): SessionMemory {
  if (event.type !== "context.compaction.completed") return current;
  const result = compactionCompletedOf(event.payload);
  if (!result || result.failed || result.batches.length === 0) return current;

  // 新生成的节点里，覆盖到最靠后的那个决定了游标：它之后的消息仍是原文。
  const furthest = result.batches.reduce(
    (sofar, batch) => Math.max(sofar, batch.end_message),
    current.raw_compaction_cursor - 1,
  );
  return {
    // 后端的游标是"第一条未折叠消息"的从 1 计序号，而 end_message 是闭区间的
    // 末条序号，所以要加一。
    raw_compaction_cursor: furthest + 1,
    // 前沿无法从单个事件推出来（合并会换掉好几个节点），这里退而求其次：
    // 显示本次新生成的那批。精确的前沿在下次刷新时由快照给出。
    active_batches: result.batches,
    total_batches: current.total_batches + result.batches.length,
  };
}

/** nextItems 返回这一轮在事件之后的片段列表。 */
function nextItems(items: TurnItem[], event: RunEvent): TurnItem[] {
  switch (event.type) {
    case "user.message":
      return [...items, {
        kind: "user",
        text: textOf(event.payload),
        images: imageIDsOf(event.payload).map((id) => ({ id, media_type: "", width: 0, height: 0 })),
      }];

    // 两种 delta 都是"往最后一段同类文字上追加"。分开处理是因为思考和正文要
    // 分别成段：模型可能想一段、说一段、再想一段。
    case "assistant.reasoning.delta":
      return appendText(items, "reasoning", textOf(event.payload));
    case "assistant.delta":
      return appendText(items, "assistant", textOf(event.payload));

    case "assistant.message": {
      const message = assistantMessageOf(event.payload);
      if (!message) return items;
      // 正文已经随 delta 逐字显示过了，这里只补上工具调用——它们没有 delta。
      return message.toolCalls.reduce<TurnItem[]>(
        (accumulated, call) => [
          ...accumulated,
          { kind: "tool", step: { call, title: "", output: "", result: null } },
        ],
        items,
      );
    }

    case "tool.started": {
      const started = toolStartedOf(event.payload);
      if (!started) return items;
      // assistant.message 已经为这次调用建过一个 step，这里补上标题；
      // 万一没有（比如从中间重连），就新建一个。
      if (!findStep(items, started.call.id)) {
        return [
          ...items,
          { kind: "tool", step: { call: started.call, title: started.title, output: "", result: null } },
        ];
      }
      return updateStep(items, started.call.id, (step) => ({ ...step, title: started.title }));
    }

    case "tool.output.delta": {
      const output = toolOutputOf(event.payload);
      if (!output) return items;
      return updateStep(items, output.toolCallID, (step) => ({
        ...step,
        output: step.output + output.chunk,
      }));
    }

    case "tool.resolved": {
      const result = toolResultOf(event.payload);
      if (!result) return items;
      const imageIDs = toolImageIDsOf(event.payload);
      const images = imageIDs.map((id) => ({ id, media_type: "", width: 0, height: 0 }));
      return updateStep(items, result.tool_call_id, (step) => ({
        ...step,
        result: { ...result, images: images.length > 0 ? images : result.images },
      }));
    }

    case "context.compaction.started": {
      const started = compactionStartedOf(event.payload);
      if (!started) return items;
      return [
        ...items,
        {
          kind: "compaction",
          step: { inputTokens: started.inputTokens, threshold: started.threshold, result: null },
        },
      ];
    }

    case "context.compaction.completed": {
      const result = compactionCompletedOf(event.payload);
      if (!result) return items;
      // 补到最后一个还没有结果的压缩步骤上。一轮里最多压缩一次，但从中间重连
      // 时可能只收到 completed 而没收到 started——那就补建一个。
      const index = items.findLastIndex(
        (item) => item.kind === "compaction" && item.step.result === null,
      );
      if (index < 0) {
        return [...items, { kind: "compaction", step: { inputTokens: 0, threshold: 0, result } }];
      }
      const existing = items[index] as { kind: "compaction"; step: CompactionStep };
      const updated: TurnItem = { kind: "compaction", step: { ...existing.step, result } };
      return [...items.slice(0, index), updated, ...items.slice(index + 1)];
    }

    case "turn.failed":
      return [...items, { kind: "failed", reason: reasonOf(event.payload) }];

    default:
      // 其余事件不产生界面片段：一轮的开始由用户消息表达，结束由最终回复表达，
      // 状态和上下文占用分别由顶部的标记和仪表表达。
      return items;
  }
}

/**
 * updateTurn 返回一个新的轮次数组，其中 id 对应的那一轮的片段被 fn 替换。
 *
 * 找不到就在末尾新建一轮。整个过程不修改传进来的任何对象——这是 reducer 保持
 * 纯净的关键，见文件开头的说明。
 */
function updateTurn(turns: Turn[], id: string, fn: (items: TurnItem[]) => TurnItem[]): Turn[] {
  const index = turns.findIndex((turn) => turn.id === id);
  if (index < 0) {
    // 事件流建出来的轮次是最新的，必然在压缩游标之后，因此 endMessage 用不到。
    return [...turns, { id, items: fn([]), endMessage: 0 }];
  }
  const updated = { ...turns[index], items: fn(turns[index].items) };
  return [...turns.slice(0, index), updated, ...turns.slice(index + 1)];
}

/**
 * appendText 把一段文字追加到末尾的同类片段上；末尾不是同类就新建一段。
 *
 * 注意是**替换**末尾那一项而不是就地改它的 text。就地改会同时改到旧状态里的
 * 那个对象（数组是浅拷贝的，元素还是同一批引用），于是 reducer 不再是纯函数：
 * StrictMode 用同样的输入调它两次，同一段文字就被追加了两遍——界面上就是
 * "让我让我理解理解一下一下"。
 */
function appendText(items: TurnItem[], kind: "reasoning" | "assistant", text: string): TurnItem[] {
  const last = items[items.length - 1];
  if (last && last.kind === kind) {
    const merged: TurnItem =
      kind === "reasoning"
        ? { kind: "reasoning", text: last.text + text }
        : { kind: "assistant", text: last.text + text, final: false };
    return [...items.slice(0, -1), merged];
  }
  return [
    ...items,
    kind === "reasoning"
      ? { kind: "reasoning", text }
      : { kind: "assistant", text, final: false },
  ];
}

/** updateStep 返回一个新的片段列表，其中匹配的工具步骤被 fn 的结果替换。 */
function updateStep(items: TurnItem[], toolCallID: string, fn: (step: ToolStep) => ToolStep): TurnItem[] {
  return items.map((item) =>
    item.kind === "tool" && item.step.call.id === toolCallID
      ? { kind: "tool", step: fn(item.step) }
      : item,
  );
}

/** findStep 在片段列表里找出某次工具调用对应的步骤。 */
function findStep(items: TurnItem[], toolCallID: string): ToolStep | null {
  for (const item of items) {
    if (item.kind === "tool" && item.step.call.id === toolCallID) return item.step;
  }
  return null;
}
