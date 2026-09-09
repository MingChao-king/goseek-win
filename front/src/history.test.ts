import { expect, test } from "bun:test";
import { entryAt, notBrowsing, stepBack, stepForward } from "./history";

const entries = ["第一条", "第二条", "第三条"];

test("第一次按 ↑ 取最新一条", () => {
  const cursor = stepBack(entries, notBrowsing);
  expect(entryAt(entries, cursor)).toBe("第三条");
});

test("继续按 ↑ 往更早走", () => {
  let cursor = stepBack(entries, notBrowsing);
  cursor = stepBack(entries, cursor);
  expect(entryAt(entries, cursor)).toBe("第二条");
});

// 走到最早那条就停住。循环回到最新会让人以为按错了键。
test("走到头就停在最早那条", () => {
  let cursor = notBrowsing;
  for (let i = 0; i < 10; i++) cursor = stepBack(entries, cursor);
  expect(entryAt(entries, cursor)).toBe("第一条");
});

test("↓ 往回走，走过最新就退出回溯", () => {
  let cursor = stepBack(entries, notBrowsing);   // 第三条
  cursor = stepBack(entries, cursor);            // 第二条
  cursor = stepForward(entries, cursor);         // 第三条
  expect(entryAt(entries, cursor)).toBe("第三条");
  cursor = stepForward(entries, cursor);         // 退出
  expect(cursor).toBe(notBrowsing);
  expect(entryAt(entries, cursor)).toBe("");
});

test("没有历史时按 ↑ 不进入回溯", () => {
  expect(stepBack([], notBrowsing)).toBe(notBrowsing);
});

test("不在回溯中时按 ↓ 无事发生", () => {
  expect(stepForward(entries, notBrowsing)).toBe(notBrowsing);
});
