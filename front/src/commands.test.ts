import { expect, test } from "bun:test";
import { helpText, parseInput } from "./commands";

test("普通文字是消息", () => {
  expect(parseInput("你好")).toEqual({ kind: "message", text: "你好" });
});

test("首尾空白被去掉", () => {
  expect(parseInput("  你好  ")).toEqual({ kind: "message", text: "你好" });
});

test("已知命令被识别", () => {
  expect(parseInput("/compact")).toEqual({ kind: "command", name: "compact" });
  expect(parseInput("/memory")).toEqual({ kind: "command", name: "memory" });
  expect(parseInput("/help")).toEqual({ kind: "command", name: "help" });
});

// 敲错的命令必须就地报错。发给模型的代价是一次真实调用，而且模型会
// 一本正经地回答一个不存在的命令。
test("不认识的斜杠开头是 unknown，不当成消息", () => {
  expect(parseInput("/compct")).toEqual({ kind: "unknown", name: "compct" });
});

// 命令不接受参数：安静忽略参数会让用户以为参数起了作用。
test("带参数的命令落进 unknown 而不是被忽略参数执行", () => {
  expect(parseInput("/compact 全部")).toEqual({ kind: "unknown", name: "compact 全部" });
});

// 转义：用户要发一段真的以斜杠开头的文字（比如问"/compact 是干嘛的"）。
test("两个斜杠转义成一个字面斜杠", () => {
  expect(parseInput("//compact 这个命令是干嘛的")).toEqual({
    kind: "message",
    text: "/compact 这个命令是干嘛的",
  });
});

// 转义要先于命令判断，否则 // 会被当成一个名字为空的命令。
test("光一个双斜杠是字面消息", () => {
  expect(parseInput("//")).toEqual({ kind: "message", text: "/" });
});

test("光一个斜杠是 unknown", () => {
  expect(parseInput("/")).toEqual({ kind: "unknown", name: "" });
});

test("帮助列出全部命令和转义写法", () => {
  const text = helpText();
  for (const fragment of ["/compact", "/memory", "/help", "//"]) {
    expect(text).toContain(fragment);
  }
});
