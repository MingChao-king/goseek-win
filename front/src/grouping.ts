// 把一轮里的片段归组，供对话流渲染。
//
// # 为什么要归组
//
// 一轮交互里模型可能连着跑十几条命令。每条都单独占一张卡片的话，用户要滚过一大片
// 命令输出才能看到真正的回复——而**对用户最重要的是正文**，命令是过程。
//
// 所以连续的工具调用合成一个盒子，默认折叠。展开与否的规则见 Transcript 里的
// ToolGroup：还在跑的时候自动展开（执行过程需要看得见），跑完自动收起。
//
// 只归**连续**的：中间夹了一段正文，就说明模型在那里停下来说了句话，那是一个
// 自然的分界，不该把两边的命令并进同一个盒子。

import type { ToolStep, TurnItem } from "./reducer";

/** RenderNode 是对话流真正渲染的单位。 */
export type RenderNode =
  /** 一个普通片段，原样渲染。 */
  | { kind: "item"; item: TurnItem; key: number }
  /** 一组连续的工具调用，渲染成一个可折叠的盒子。 */
  | { kind: "tools"; steps: ToolStep[]; key: number };

/**
 * groupTurnItems 把连续的工具调用合并成一组。
 *
 * key 用的是这一组第一个片段的下标：同一轮里的片段只会往后追加、不会重排，
 * 因此下标是稳定的，可以直接当 React 的 key。
 */
export function groupTurnItems(items: TurnItem[]): RenderNode[] {
  const nodes: RenderNode[] = [];

  for (let index = 0; index < items.length; index++) {
    const item = items[index];
    if (item.kind !== "tool") {
      nodes.push({ kind: "item", item, key: index });
      continue;
    }

    // 收拢从这里开始的一串连续工具调用。
    const steps: ToolStep[] = [];
    const start = index;
    while (index < items.length) {
      const candidate = items[index];
      if (candidate.kind !== "tool") break;
      steps.push(candidate.step);
      index++;
    }
    // 循环多走了一步，退回去让外层的 index++ 接上。
    index--;
    nodes.push({ kind: "tools", steps, key: start });
  }

  return nodes;
}

/** GroupState 概括一组工具调用的进展，用在折叠时的那行摘要上。 */
export interface GroupState {
  total: number;
  /** 还没拿到观察的调用数。大于 0 表示这一组还在跑。 */
  running: number;
  failed: number;
}

/** summarizeGroup 统计一组工具调用的进展。 */
export function summarizeGroup(steps: ToolStep[]): GroupState {
  let running = 0;
  let failed = 0;
  for (const step of steps) {
    if (!step.result) running++;
    else if (step.result.status === "error") failed++;
  }
  return { total: steps.length, running, failed };
}

/**
 * describeGroup 生成折叠时那一行字。
 *
 * 折叠起来的东西必须**自我说明**：只写"3 次命令"，用户就得展开才知道有没有出错。
 * 所以把结局也带上——有失败一定要在收起状态下就看得见。
 */
export function describeGroup(steps: ToolStep[]): string {
  const state = summarizeGroup(steps);
  if (state.total === 0) return "没有命令";

  const names = new Set(steps.map((step) => step.call.name));
  const what = names.size === 1 ? [...names][0] : "工具";

  if (state.running > 0) {
    // 正在跑：说清楚跑到第几条了，比只说"执行中"有用。
    const done = state.total - state.running;
    return `正在执行 ${what}（第 ${done + 1} / ${state.total} 条）`;
  }
  if (state.failed > 0) {
    return `${state.total} 次 ${what} 调用，其中 ${state.failed} 次失败`;
  }
  return `${state.total} 次 ${what} 调用`;
}
