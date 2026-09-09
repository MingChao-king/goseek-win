import { expect, test } from "bun:test";
import { describeGroup, groupTurnItems, summarizeGroup } from "./grouping";
import type { RenderNode } from "./grouping";
import type { ToolStep, TurnItem } from "./reducer";

/** step 造一个工具步骤。result 为 null 表示还在跑。 */
function step(name: string, status?: "success" | "error"): ToolStep {
  return {
    call: { id: `call-${name}-${status ?? "running"}`, name, arguments: {} },
    title: "",
    output: "",
    result: status
      ? { tool_call_id: "x", name, status, content: "" }
      : null,
  };
}

function toolItem(name: string, status?: "success" | "error"): TurnItem {
  return { kind: "tool", step: step(name, status) };
}

const userItem: TurnItem = { kind: "user", text: "问题" };
const textItem: TurnItem = { kind: "assistant", text: "回答", final: true };

function shape(nodes: RenderNode[]): string[] {
  return nodes.map((node) =>
    node.kind === "tools" ? `tools×${node.steps.length}` : node.item.kind,
  );
}

test("连续的工具调用合成一组", () => {
  const nodes = groupTurnItems([
    userItem,
    toolItem("bash", "success"),
    toolItem("bash", "success"),
    toolItem("bash", "error"),
    textItem,
  ]);

  expect(shape(nodes)).toEqual(["user", "tools×3", "assistant"]);
});

// 中间夹了一段正文，说明模型在那里停下来说了句话——那是一个自然的分界，
// 不该把两边的命令并进同一个盒子。
test("被正文隔开的工具调用分成两组", () => {
  const nodes = groupTurnItems([
    toolItem("bash", "success"),
    textItem,
    toolItem("bash", "success"),
    toolItem("bash", "success"),
  ]);

  expect(shape(nodes)).toEqual(["tools×1", "assistant", "tools×2"]);
});

test("没有工具调用时原样透传", () => {
  const nodes = groupTurnItems([userItem, textItem]);
  expect(shape(nodes)).toEqual(["user", "assistant"]);
});

test("空片段列表得到空结果", () => {
  expect(groupTurnItems([])).toEqual([]);
});

// key 用组内第一个片段的下标：片段只会往后追加、不会重排，下标是稳定的。
test("key 是组内第一个片段的下标", () => {
  const nodes = groupTurnItems([userItem, toolItem("bash"), toolItem("bash"), textItem]);
  expect(nodes.map((node) => node.key)).toEqual([0, 1, 3]);
});

test("统计进展", () => {
  const steps = [step("bash", "success"), step("bash", "error"), step("bash")];
  expect(summarizeGroup(steps)).toEqual({ total: 3, running: 1, failed: 1 });
});

// 折叠起来的东西必须自我说明：只写"3 次命令"，用户就得展开才知道有没有出错。
test("折叠时那行字带上结局", () => {
  expect(describeGroup([step("bash", "success"), step("bash", "success")]))
    .toBe("2 次 bash 调用");
  expect(describeGroup([step("bash", "success"), step("bash", "error")]))
    .toContain("1 次失败");
});

test("正在跑时说清楚跑到第几条", () => {
  const steps = [step("bash", "success"), step("bash"), step("bash")];
  expect(describeGroup(steps)).toBe("正在执行 bash（第 2 / 3 条）");
});

test("混了不同工具时不假装只有一种", () => {
  const steps = [step("bash", "success"), step("conversation_history", "success")];
  expect(describeGroup(steps)).toBe("2 次 工具 调用");
});
