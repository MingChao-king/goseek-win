import { expect, test } from "bun:test";
import { initialState, reduce } from "./reducer";
import type { RunEvent, SessionSnapshot } from "./types";

/** delta 造一个正文增量事件。transient 事件的 sequence 恒为 0。 */
function delta(text: string): RunEvent {
  return { sequence: 0, turn_id: "trn_a", type: "assistant.delta", payload: { text }, at: "" };
}

/** durable 造一个带序号的事件。 */
function durable(sequence: number, type: RunEvent["type"], payload: unknown): RunEvent {
  return { sequence, turn_id: "trn_a", type, payload, at: "" };
}

const testModelInfo = {
  name: "glm-5.3",
  display_name: "GLM 5.3",
  context_window: 1048576,
  effective_context_window: 996147,
  compaction_trigger: 796917,
  window_source: "gateway-measured",
  measured_at: "2026-09-01",
};

/** allText 把当前状态里所有片段的文字拼起来，便于断言。 */
function allText(state: ReturnType<typeof reduce>): string {
  return state.turns.flatMap((turn) =>
    turn.items.map((item) => ("text" in item ? item.text : "")),
  ).join("");
}

// React 的 StrictMode 在开发模式下会**用同样的输入调用 reducer 两次**，
// 用来暴露不纯的实现。reducer 必须是纯函数：两次调用得到同样的结果，
// 并且不修改传进来的旧状态。
//
// 这条测试就是那个双调用的模拟。它是本次 bug 的复现用例——原先 appendText
// 直接改了旧状态里的对象（`last.text += text`），于是第二次调用把同一段文字
// 又追加了一遍，界面上就出现"让我让我理解理解"。
test("同一个事件重复归约两次，结果不变（reducer 必须是纯函数）", () => {
  const state = reduce(initialState, { type: "event", event: delta("你好") });

  const once = reduce(state, { type: "event", event: delta("世界") });
  const twice = reduce(state, { type: "event", event: delta("世界") });

  expect(allText(once)).toBe("你好世界");
  expect(allText(twice)).toBe("你好世界");
});

test("归约不修改传进来的旧状态", () => {
  const before = reduce(initialState, { type: "event", event: delta("原文") });
  const snapshotOfBefore = allText(before);

  reduce(before, { type: "event", event: delta("追加") });

  expect(allText(before)).toBe(snapshotOfBefore);
});

test("连续的正文增量拼成一段", () => {
  let state = initialState;
  for (const piece of ["核心", "设计", "很清晰"]) {
    state = reduce(state, { type: "event", event: delta(piece) });
  }
  expect(allText(state)).toBe("核心设计很清晰");
});

// 思考和正文是两种不同的东西，切换时要另起一段——否则思考的最后一句会和
// 回复的第一句黏在一起。后端 CLI 那边踩过同一个坑。
test("思考与正文分成两段，不会黏在一起", () => {
  let state = reduce(initialState, {
    type: "event",
    event: { sequence: 0, turn_id: "trn_a", type: "assistant.reasoning.delta", payload: { text: "先想想" }, at: "" },
  });
  state = reduce(state, { type: "event", event: delta("答案") });

  const kinds = state.turns[0].items.map((item) => item.kind);
  expect(kinds).toEqual(["reasoning", "assistant"]);
});

// SSE 断线重连时，服务端会从指定序号之后重放。重放窗口与实时窗口有重叠的话，
// 同一个 durable 事件可能被送来两次；界面不能因此显示两条一样的消息。
test("重复送达的 durable 事件被忽略", () => {
  let state = reduce(initialState, {
    type: "event",
    event: durable(1, "user.message", { content: "你好" }),
  });
  state = reduce(state, {
    type: "event",
    event: durable(1, "user.message", { content: "你好" }),
  });

  expect(state.turns[0].items.length).toBe(1);
});

// turn.started 和 state.changed 一样是"一轮开始"的权威信号。快照总是
// IDLE，如果只靠后者，快照加载后立刻发消息会被误判成运行中注入，界面上
// 既不显示真身也不显示排队替身。
test("turn.started 将状态切到等待模型", () => {
  const state = reduce(initialState, {
    type: "event",
    event: durable(1, "turn.started", null),
  });

  expect(state.runState).toBe("WAITING_MODEL");
});

test("工具调用的输出片段按顺序累积", () => {
  const call = { id: "call-1", name: "bash", arguments: { command: "ls" } };
  let state = reduce(initialState, {
    type: "event",
    event: durable(1, "assistant.message", { content: "", tool_calls: [call], final: false }),
  });
  for (const chunk of ["第一行\n", "第二行\n"]) {
    state = reduce(state, {
      type: "event",
      event: { sequence: 0, turn_id: "trn_a", type: "tool.output.delta", payload: { tool_call_id: "call-1", chunk }, at: "" },
    });
  }

  const item = state.turns[0].items[0];
  expect(item.kind).toBe("tool");
  if (item.kind === "tool") {
    expect(item.step.output).toBe("第一行\n第二行\n");
  }
});

// 快照是事件流的起点。它必须把 tool 消息里的观察挂回对应的调用上——
// 那条不变量（每个调用恰好一条同 id 的观察）由后端保证。
test("快照把观察挂回对应的工具调用", () => {
  const snapshot: SessionSnapshot = {
    id: "ses_a",
    workspace: "/tmp",
    created_at: "",
    updated_at: "",
    last_sequence: 5,
    memory: { raw_compaction_cursor: 1, active_batches: [], total_batches: 0 },
    model: "glm-5.3",
    model_info: testModelInfo,
    usage: {
      context_window: 128000, input_tokens: 1200, remaining: 126800,
      ratio: 0.009375, source: "estimated",
    },
    messages: [
      { role: "user", content: "看看", turn_id: "trn_a" },
      {
        role: "assistant",
        content: "",
        turn_id: "trn_a",
        tool_calls: [{ id: "call-1", name: "bash", arguments: {} }],
      },
      {
        role: "tool",
        content: JSON.stringify({
          tool_call_id: "call-1", name: "bash", status: "success",
          content: "输出", exit_code: 0,
        }),
        tool_call_id: "call-1",
        turn_id: "trn_a",
      },
    ],
  };

  const state = reduce(initialState, { type: "snapshot", snapshot });

  expect(state.lastSequence).toBe(5);
  const toolItem = state.turns[0].items.find((item) => item.kind === "tool");
  expect(toolItem?.kind).toBe("tool");
  if (toolItem?.kind === "tool") {
    expect(toolItem.step.result?.status).toBe("success");
    expect(toolItem.step.result?.exit_code).toBe(0);
  }
});

// —— M4.2：上下文压缩 ——

/** batch 造一个摘要节点。 */
function batch(id: string, level: number, start: number, end: number, title: string) {
  return { id, level, title, start_message: start, end_message: end };
}

// 压缩要在**发生的那一轮里**留下痕迹：先出现"正在整理"，
// 结果回来后补到同一个片段上，而不是再追加一个。
test("压缩的开始与结束合并成同一个片段", () => {
  const started = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.started", { input_tokens: 52000, threshold: 48000 }),
  });

  const items = started.turns[0].items;
  expect(items).toHaveLength(1);
  expect(items[0].kind).toBe("compaction");
  if (items[0].kind === "compaction") {
    expect(items[0].step.result).toBeNull();
    expect(items[0].step.inputTokens).toBe(52000);
  }

  const completed = reduce(started, {
    type: "event",
    event: durable(2, "context.compaction.completed", {
      before_tokens: 52000,
      after_tokens: 11000,
      batches: [batch("mem_a", 0, 1, 12, "Topic: 排查登录失败")],
      target_unreachable: false,
    }),
  });

  // 仍然只有一个片段——结果补上去了，不是又追加了一条。
  expect(completed.turns[0].items).toHaveLength(1);
  const item = completed.turns[0].items[0];
  if (item.kind === "compaction") {
    expect(item.step.result?.after_tokens).toBe(11000);
    expect(item.step.result?.batches[0].title).toBe("Topic: 排查登录失败");
    // 触发时的数字要保留：结果事件里没有它，但界面上"为什么压"要靠它。
    expect(item.step.inputTokens).toBe(52000);
  }
});

// 压缩成功后，顶部的记忆现状要跟着更新——用户得知道模型眼前不再是全部原文。
test("压缩成功后更新记忆现状", () => {
  const state = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.completed", {
      before_tokens: 52000,
      after_tokens: 11000,
      batches: [batch("mem_a", 0, 1, 12, "前半段"), batch("mem_b", 0, 13, 20, "后半段")],
      target_unreachable: false,
    }),
  });

  expect(state.memory.active_batches).toHaveLength(2);
  // 覆盖到第 20 条为止，那么第 21 条起仍是原文。
  expect(state.memory.raw_compaction_cursor).toBe(21);
  expect(state.memory.total_batches).toBe(2);
});

// 压缩失败不改变上下文的构成，因此记忆现状保持原样。
// 显示一个"压了但没压成"的中间状态只会让人困惑。
test("压缩失败不改变记忆现状", () => {
  const state = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.completed", {
      before_tokens: 52000,
      after_tokens: 52000,
      batches: [],
      target_unreachable: false,
      failed: "供应商 500",
    }),
  });

  expect(state.memory.active_batches).toHaveLength(0);
  expect(state.memory.raw_compaction_cursor).toBe(1);
  const item = state.turns[0].items[0];
  expect(item.kind).toBe("compaction");
  if (item.kind === "compaction") {
    expect(item.step.result?.failed).toBe("供应商 500");
  }
});

// 只收到结果没收到开始（从中间重连）时也要能显示，不能把这个事件丢掉。
test("只收到压缩结果时补建片段", () => {
  const state = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.completed", {
      before_tokens: 52000,
      after_tokens: 11000,
      batches: [batch("mem_a", 1, 1, 12, "合并摘要")],
      target_unreachable: true,
    }),
  });

  const item = state.turns[0].items[0];
  expect(item.kind).toBe("compaction");
  if (item.kind === "compaction") {
    expect(item.step.result?.target_unreachable).toBe(true);
  }
});

// COMPRESSING 是一个独立的状态，不能被当成未知值丢掉——
// 丢掉的话界面会停在"正在请求模型"，而压缩可能要几十秒。
test("COMPRESSING 状态被识别", () => {
  const state = reduce(initialState, {
    type: "event",
    event: durable(1, "state.changed", { state: "COMPRESSING" }),
  });

  expect(state.runState).toBe("COMPRESSING");
});

// 压缩现状必须来自快照：压缩事件是历史事件，刷新页面后不会被重放。
test("快照带来压缩现状", () => {
  const snapshot: SessionSnapshot = {
    id: "ses_a",
    workspace: "/tmp",
    created_at: "",
    updated_at: "",
    last_sequence: 9,
    memory: {
      raw_compaction_cursor: 13,
      active_batches: [batch("mem_a", 1, 1, 12, "合并摘要")],
      total_batches: 3,
    },
    model: "glm-5.3",
    model_info: testModelInfo,
    usage: {
      context_window: 128000, input_tokens: 1200, remaining: 126800,
      ratio: 0.009375, source: "estimated",
    },
    messages: [],
  };

  const state = reduce(initialState, { type: "snapshot", snapshot });

  expect(state.memory.raw_compaction_cursor).toBe(13);
  expect(state.memory.active_batches[0].title).toBe("合并摘要");
  // 总数大于前沿长度：差值就是已经被合并进上层的那些节点。
  expect(state.memory.total_batches).toBe(3);
});

// 压缩片段同样要经得起 StrictMode 的双调用。
test("重复归约压缩事件不产生两个片段", () => {
  const started = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.started", { input_tokens: 52000, threshold: 48000 }),
  });
  const event = durable(2, "context.compaction.completed", {
    before_tokens: 52000, after_tokens: 11000,
    batches: [batch("mem_a", 0, 1, 12, "摘要")], target_unreachable: false,
  });

  const once = reduce(started, { type: "event", event });
  const twice = reduce(started, { type: "event", event });

  expect(once.turns[0].items).toHaveLength(1);
  expect(twice.turns[0].items).toHaveLength(1);
  expect(once.memory.total_batches).toBe(twice.memory.total_batches);
  // 旧状态不能被改动。
  const stillRunning = started.turns[0].items[0];
  if (stillRunning.kind === "compaction") {
    expect(stillRunning.step.result).toBeNull();
  }
});

// 一个节点都没产生时不更新记忆现状——什么都没压，现状没变。
test("空压缩不改变记忆现状", () => {
  const before = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.completed", {
      before_tokens: 4997, after_tokens: 4997, batches: [], target_unreachable: true,
    }),
  });

  expect(before.memory.total_batches).toBe(0);
  expect(before.memory.raw_compaction_cursor).toBe(1);
});

// 后端在 payload 里给的是空数组，但历史数据可能是 null——两种都要能处理，
// 不能让一帧坏数据把整个页面搞崩。
test("batches 为 null 时按空处理", () => {
  const state = reduce(initialState, {
    type: "event",
    event: durable(1, "context.compaction.completed", {
      before_tokens: 100, after_tokens: 100, batches: null, target_unreachable: false,
    }),
  });

  const item = state.turns[0].items[0];
  expect(item.kind).toBe("compaction");
  if (item.kind === "compaction") {
    expect(item.step.result?.batches).toEqual([]);
  }
});

// 刷新页面之后仪表盘不该消失：占用由快照带过来。
test("快照带来上下文占用", () => {
  const snapshot: SessionSnapshot = {
    id: "ses_a", workspace: "/tmp", created_at: "", updated_at: "", last_sequence: 9,
    memory: { raw_compaction_cursor: 1, active_batches: [], total_batches: 0 },
    model: "glm-5.3",
    model_info: testModelInfo,
    usage: {
      context_window: 128000, input_tokens: 28666, remaining: 99334,
      ratio: 0.2239, source: "provider",
    },
    messages: [],
  };

  const state = reduce(initialState, { type: "snapshot", snapshot });

  expect(state.usage?.input_tokens).toBe(28666);
  expect(state.usage?.source).toBe("provider");
});

// 后端比前端旧时这个字段还不存在，不能因此让整个页面崩掉。
test("旧后端没有 usage 字段时按空处理", () => {
  const snapshot = {
    id: "ses_a", workspace: "/tmp", created_at: "", updated_at: "", last_sequence: 0,
    memory: { raw_compaction_cursor: 1, active_batches: [], total_batches: 0 },
    messages: [],
  } as unknown as SessionSnapshot;

  expect(reduce(initialState, { type: "snapshot", snapshot }).usage).toBeNull();
});


test("快照 run_state：思考中切走再切回不再是空闲", () => {
    const snapshot: SessionSnapshot = {
      id: "ses_x", workspace: "/tmp", created_at: "", updated_at: "",
      last_sequence: 1, messages: [], memory: { raw_compaction_cursor: 1, active_batches: [], total_batches: 0 },
      usage: {
        context_window: 128000, input_tokens: 1200, remaining: 126800,
        ratio: 0.009375, source: "estimated",
      },
      model: "", model_info: testModelInfo, run_state: "WAITING_MODEL",
    };
    const state = reduce(initialState, { type: "snapshot", snapshot });
    expect(state.runState).toBe("WAITING_MODEL");
});

test("快照不带 run_state 时保持 IDLE（旧后端兼容）", () => {
    const snapshot: SessionSnapshot = {
      id: "ses_x", workspace: "/tmp", created_at: "", updated_at: "",
      last_sequence: 1, messages: [], memory: { raw_compaction_cursor: 1, active_batches: [], total_batches: 0 },
      usage: {
        context_window: 128000, input_tokens: 1200, remaining: 126800,
        ratio: 0.009375, source: "estimated",
      },
      model: "", model_info: testModelInfo,
    };
    const state = reduce(initialState, { type: "snapshot", snapshot });
    expect(state.runState).toBe("IDLE");
});

