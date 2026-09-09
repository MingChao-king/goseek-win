// 事件的 payload 形状由事件类型决定，而 TypeScript 无法凭 type 字段自动推断出
// 它——那需要后端和前端共享一份可辨识联合类型的定义。这里用一组小函数手工收窄：
// 每个函数接收 unknown，检查它长得对不对，返回一个具体类型或 null。
//
// 为什么要检查而不是直接断言：payload 来自网络，是外部输入。`as` 断言只是骗过
// 编译器，运行时该 undefined 还是 undefined，最后变成界面上一句 "Cannot read
// properties of undefined"，而且离出错的地方很远。

import type {
  CompactionResult, ContextUsage, MemoryBatch, RunState, ToolCall, ToolResult,
} from "./types";

/** isObject 判断一个值是不是非 null 的对象。 */
function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/** stringField 取出一个字符串字段，缺失或类型不对时返回空串。 */
function stringField(payload: unknown, key: string): string {
  if (!isObject(payload)) return "";
  const value = payload[key];
  return typeof value === "string" ? value : "";
}

/** textOf 取出 delta 或用户消息里的文字。 */
export function textOf(payload: unknown): string {
  return stringField(payload, "text") || stringField(payload, "content");
}

/** imageIDsOf 取出用户消息事件里的图片 ID 列表。 */
export function imageIDsOf(payload: unknown): string[] {
  if (!isObject(payload)) return [];
  const value = payload["image_ids"];
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is string => typeof item === "string");
}

/** toolImageIDsOf 取出 tool.resolved 事件里的图片 ID 列表。 */
export function toolImageIDsOf(payload: unknown): string[] {
  if (!isObject(payload) || !isObject(payload.result)) return [];
  const direct = payload["image_ids"];
  if (Array.isArray(direct)) {
    return direct.filter((item): item is string => typeof item === "string");
  }
  // 兼容旧事件：result.images 里的字段可能是大写 ID。
  const images = payload.result["images"];
  if (!Array.isArray(images)) return [];
  return images
    .map((item: unknown) => {
      if (!isObject(item)) return "";
      const v = item["id"] ?? item["ID"];
      return typeof v === "string" ? v : "";
    })
    .filter(Boolean);
}

/** numberField 取出一个数字字段，缺失或类型不对时返回 0。 */
function numberField(payload: unknown, key: string): number {
  if (!isObject(payload)) return 0;
  const value = payload[key];
  return typeof value === "number" ? value : 0;
}

/** stateOf 取出状态变化事件里的新状态。 */
export function stateOf(payload: unknown): RunState | null {
  const state = stringField(payload, "state");
  const known: RunState[] = [
    "IDLE", "WAITING_MODEL", "RUNNING_TOOL", "COMPRESSING", "FAILED",
  ];
  return known.includes(state as RunState) ? (state as RunState) : null;
}

/** assistantMessageOf 取出一条完整的 assistant 消息。 */
export function assistantMessageOf(
  payload: unknown,
): { content: string; toolCalls: ToolCall[]; final: boolean } | null {
  if (!isObject(payload)) return null;
  const rawCalls = payload.tool_calls;
  return {
    content: stringField(payload, "content"),
    toolCalls: Array.isArray(rawCalls) ? (rawCalls as ToolCall[]) : [],
    final: payload.final === true,
  };
}

/** toolStartedOf 取出即将执行的调用及其标题。 */
export function toolStartedOf(
  payload: unknown,
): { call: ToolCall; title: string } | null {
  if (!isObject(payload) || !isObject(payload.call)) return null;
  return {
    call: payload.call as unknown as ToolCall,
    title: stringField(payload, "title"),
  };
}

/** toolOutputOf 取出一段工具输出以及它属于哪次调用。 */
export function toolOutputOf(
  payload: unknown,
): { toolCallID: string; chunk: string } | null {
  if (!isObject(payload)) return null;
  return {
    toolCallID: stringField(payload, "tool_call_id"),
    chunk: stringField(payload, "chunk"),
  };
}

/** toolResultOf 取出一次调用的观察结果。 */
export function toolResultOf(payload: unknown): ToolResult | null {
  if (!isObject(payload) || !isObject(payload.result)) return null;
  return payload.result as unknown as ToolResult;
}

/** contextUsageOf 取出上下文占用。 */
export function contextUsageOf(payload: unknown): ContextUsage | null {
  if (!isObject(payload)) return null;
  const source = stringField(payload, "source");
  const known = ["unknown", "estimated", "provider"];
  return {
    context_window: numberField(payload, "context_window"),
    input_tokens: numberField(payload, "input_tokens"),
    remaining: numberField(payload, "remaining"),
    ratio: numberField(payload, "ratio"),
    source: (known.includes(source) ? source : "unknown") as ContextUsage["source"],
    // 用 optionalNumber 而不是 numberField：缺席必须保持 undefined。
    // numberField 把缺席折成 0，那会让"这次全部未命中"和"供应商没报"分不开——
    // 前者是需要注意的信号，后者只是一个不适用的字段。
    cache_hit_tokens: optionalNumber(payload, "cache_hit_tokens"),
    cache_miss_tokens: optionalNumber(payload, "cache_miss_tokens"),
    cache_hit_ratio: optionalNumber(payload, "cache_hit_ratio"),
  };
}

/** optionalNumber 取出一个可选的数字字段。缺席或类型不对时返回 undefined。 */
function optionalNumber(payload: unknown, key: string): number | undefined {
  if (!isObject(payload)) return undefined;
  const value = payload[key];
  return typeof value === "number" ? value : undefined;
}

/**
 * memoryBatchesOf 从一个数组字段里取出摘要节点。
 *
 * 逐字段挑出来而不是整个数组 `as MemoryBatch[]`：这些数据来自网络，断言只是
 * 骗过编译器。真缺了字段的话，`as` 会让 `batch.title` 在渲染时变成 undefined，
 * 报错点离真正的原因很远；这里补默认值，最坏情况是显示一行空标题。
 */
function memoryBatchesOf(payload: unknown, key: string): MemoryBatch[] {
  if (!isObject(payload) || !Array.isArray(payload[key])) return [];
  return (payload[key] as unknown[]).filter(isObject).map((raw) => ({
    id: stringField(raw, "id"),
    level: numberField(raw, "level"),
    title: stringField(raw, "title"),
    start_message: numberField(raw, "start_message"),
    end_message: numberField(raw, "end_message"),
  }));
}

/** compactionStartedOf 取出"为什么现在要压缩"：当前占用和触发线。 */
export function compactionStartedOf(
  payload: unknown,
): { inputTokens: number; threshold: number } | null {
  if (!isObject(payload)) return null;
  return {
    inputTokens: numberField(payload, "input_tokens"),
    threshold: numberField(payload, "threshold"),
  };
}

/** compactionCompletedOf 取出一次压缩的结果。 */
export function compactionCompletedOf(payload: unknown): CompactionResult | null {
  if (!isObject(payload)) return null;
  return {
    before_tokens: numberField(payload, "before_tokens"),
    after_tokens: numberField(payload, "after_tokens"),
    batches: memoryBatchesOf(payload, "batches"),
    target_unreachable: payload.target_unreachable === true,
    // reason 和 failed 一样是 omitempty，正常情况下这个键不存在。
    reason: stringField(payload, "reason"),
    // failed 在后端是 omitempty：成功时这个键根本不存在，取出来就是空串。
    failed: stringField(payload, "failed"),
  };
}

/** reasonOf 取出失败原因。 */
export function reasonOf(payload: unknown): string {
  return stringField(payload, "reason");
}
