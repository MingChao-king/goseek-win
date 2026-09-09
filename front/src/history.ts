// 输入框的历史回溯（↑ 键）。
//
// 和终端一致：输入框为空时按 ↑ 填入上一条自己发过的消息，再按继续往前。改一个字
// 重发是很常用的动作，而现在只能重新打一遍。
//
// 历史本身不用另外存——对话流里就有用户发过的每一条。这里只负责"游标走到第几条"。

/** 回溯游标。-1 表示没有在回溯（输入框里是用户自己打的东西）。 */
export const notBrowsing = -1;

/**
 * stepBack 把游标往回走一格，返回新的游标位置。
 *
 * `entries` 是按时间正序的历史（最早在前）。游标从末尾开始往前走，走到头就停在
 * 最早那条——**不循环回到最新**。循环会让人以为按错了键，而且没有任何场景需要它。
 */
export function stepBack(entries: string[], cursor: number): number {
  if (entries.length === 0) return notBrowsing;
  if (cursor === notBrowsing) return entries.length - 1;
  return Math.max(0, cursor - 1);
}

/**
 * stepForward 把游标往前走一格。
 *
 * 走过最新一条就退出回溯（返回 notBrowsing），此时调用方应当把输入框清空——
 * 那对应"我不想用历史了"。
 */
export function stepForward(entries: string[], cursor: number): number {
  if (cursor === notBrowsing) return notBrowsing;
  if (cursor + 1 >= entries.length) return notBrowsing;
  return cursor + 1;
}

/** entryAt 取游标指向的那条。游标不在回溯中时返回空串。 */
export function entryAt(entries: string[], cursor: number): string {
  if (cursor === notBrowsing || cursor < 0 || cursor >= entries.length) return "";
  return entries[cursor];
}
