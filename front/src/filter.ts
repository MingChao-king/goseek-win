// 会话列表的本地过滤。
//
// 单独抽出来是为了能直接测：过滤的规则（匹配哪些字段、大小写怎么处理、空串怎么办）
// 是有取舍的，而取舍需要用例钉住。放在组件里就只能靠渲染整棵树来测。
//
// **过滤在前端做，不发请求。** 会话数量是几十到几百的量级，全在内存里；为一次
// 输入去请求一趟后端，代价是每敲一个字都要等一次往返，而收益是零。

import type { SessionSummary } from "./types";

/**
 * matchSessions 按关键词过滤会话。
 *
 * 匹配**标题和工作目录**两个字段：用户找一个会话时，记得住的要么是它叫什么，
 * 要么是它在哪个目录里干活。会话 id 不参与匹配——那串十六进制没人记得住，
 * 反而会让形如 "ses" 的输入命中全部。
 *
 * 空关键词返回原数组本身（而不是拷贝）：这样调用方的 useMemo 在没过滤时
 * 返回同一个引用，下游组件不会因为"数组换了个新的"而白白重渲。
 */
export function matchSessions(sessions: SessionSummary[], keyword: string): SessionSummary[] {
  const needle = keyword.trim().toLowerCase();
  if (!needle) return sessions;
  return sessions.filter(
    (session) =>
      session.title.toLowerCase().includes(needle) ||
      session.workspace.toLowerCase().includes(needle),
  );
}
