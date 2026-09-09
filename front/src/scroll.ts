// 对话流的滚动跟随策略。
//
// # 这里修的是一个功能性缺陷，不是体验问题
//
// 原来的 ScrollAnchor 每次 turns 变化就无条件 scrollIntoView。后果是**长对话里
// 根本没法回头看**：你往上翻，模型吐出下一个 delta，界面立刻把你拽回底部——
// 流式输出时这一秒能发生几十次。
//
// 主流做法是"只在用户已经在底部时才跟随"：他上翻走了就停下，给一个「回到底部」
// 的入口，让他自己决定什么时候回去。
//
// 纯函数放在这里，是为了能不搭 DOM 就测它——判定规则是"同样输入必须得到同样输出"
// 的东西，不该为了验证它去起一个浏览器。

/** 滚动容器的度量，取自 DOM 但本身只是三个数。 */
export interface ScrollMetrics {
  /** 内容总高度。 */
  scrollHeight: number;
  /** 已经滚过的距离。 */
  scrollTop: number;
  /** 可视区域高度。 */
  clientHeight: number;
}

/**
 * bottomThreshold 是"算作在底部"的容差，单位像素。
 *
 * **不能是 0。** 流式输出把内容顶高的那一瞬间，scrollTop 还没跟上，距底会出现
 * 几十像素的瞬时偏差；容差取 0 会把这一下误判成"用户上翻了"，于是跟随在流式
 * 输出的第一个字就停了——正好是最需要跟随的时候。
 *
 * 取 80：比一行文字高，比一屏小得多。用户真的往上翻至少会翻过一行。
 */
export const bottomThreshold = 80;

/** distanceFromBottom 返回距离底部还有多少像素。 */
export function distanceFromBottom(metrics: ScrollMetrics): number {
  return metrics.scrollHeight - metrics.scrollTop - metrics.clientHeight;
}

/** isAtBottom 判断用户此刻是不是贴着底部。 */
export function isAtBottom(metrics: ScrollMetrics): boolean {
  return distanceFromBottom(metrics) <= bottomThreshold;
}

/**
 * shouldFollow 决定内容增长时要不要跟着滚到底。
 *
 * 规则只有一条：**用户在底部就跟，不在就不跟**。没有"上次跟过所以这次也跟"之类的
 * 记忆——那种状态机会在用户手动滚动和程序滚动之间纠缠不清。
 *
 * 内容还没撑满一屏时（scrollHeight <= clientHeight）恒为真：此时无所谓滚不滚，
 * 但返回真能让第一屏内容出现时行为一致。
 */
export function shouldFollow(metrics: ScrollMetrics): boolean {
  if (metrics.scrollHeight <= metrics.clientHeight) return true;
  return isAtBottom(metrics);
}
