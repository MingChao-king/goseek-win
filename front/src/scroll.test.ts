import { expect, test } from "bun:test";
import { bottomThreshold, distanceFromBottom, isAtBottom, shouldFollow } from "./scroll";

/** metrics 造一组度量：内容 total 高，可视 view 高，已滚到距底 fromBottom。 */
function metrics(total: number, view: number, fromBottom: number) {
  return { scrollHeight: total, clientHeight: view, scrollTop: total - view - fromBottom };
}

test("距底距离", () => {
  expect(distanceFromBottom(metrics(1000, 400, 0))).toBe(0);
  expect(distanceFromBottom(metrics(1000, 400, 250))).toBe(250);
});

test("贴着底部时跟随", () => {
  expect(shouldFollow(metrics(5000, 600, 0))).toBe(true);
});

// 容差不能是 0：流式输出把内容顶高的那一瞬间会出现几十像素的瞬时偏差，
// 容差取 0 会把这一下误判成"用户上翻了"，于是跟随在流式输出的第一个字就停了。
test("底部附近的小偏差仍然算在底部", () => {
  expect(shouldFollow(metrics(5000, 600, bottomThreshold - 1))).toBe(true);
  expect(shouldFollow(metrics(5000, 600, bottomThreshold))).toBe(true);
});

// 这是这一整块存在的理由：用户往上翻之后，新内容不能把他拽回去。
test("用户往上翻之后不跟随", () => {
  expect(shouldFollow(metrics(5000, 600, bottomThreshold + 1))).toBe(false);
  expect(shouldFollow(metrics(5000, 600, 2000))).toBe(false);
});

test("内容还没撑满一屏时恒跟随", () => {
  expect(shouldFollow({ scrollHeight: 300, clientHeight: 600, scrollTop: 0 })).toBe(true);
});

// isAtBottom 和 shouldFollow 在内容超过一屏时应当一致——两个判定分叉会让
// "回到底部"按钮的显隐和实际跟随行为对不上。
test("超过一屏时两个判定一致", () => {
  for (const fromBottom of [0, 40, 79, 80, 81, 500]) {
    const m = metrics(5000, 600, fromBottom);
    expect(shouldFollow(m)).toBe(isAtBottom(m));
  }
});
