// 会话列表的本地过滤。

import { describe, expect, test } from "bun:test";
import { matchSessions } from "./filter";
import type { SessionSummary } from "./types";

function session(id: string, title: string, workspace: string): SessionSummary {
  return {
    id, title, workspace,
    message_count: 0, updated_at: "",
    archived: false, custom_title: false, running_state: null,
    model: "",
  };
}

const all = [
  session("ses_1", "分析日志里的错误", "/Users/me/logs"),
  session("ses_2", "重构 parser", "/Users/me/goseek/backend"),
  session("ses_3", "压缩验收", "/tmp/probe"),
];

describe("会话过滤", () => {
  test("空关键词返回原数组本身", () => {
    // 是同一个引用，不是内容相等的拷贝——调用方的 useMemo 依赖这一点，
    // 否则每次渲染都产生新数组，下游白白重渲。
    expect(matchSessions(all, "")).toBe(all);
    expect(matchSessions(all, "   ")).toBe(all);
  });

  test("匹配标题", () => {
    expect(matchSessions(all, "重构").map((s) => s.id)).toEqual(["ses_2"]);
  });

  test("匹配工作目录", () => {
    expect(matchSessions(all, "/tmp").map((s) => s.id)).toEqual(["ses_3"]);
  });

  test("大小写不敏感", () => {
    expect(matchSessions(all, "PARSER").map((s) => s.id)).toEqual(["ses_2"]);
    expect(matchSessions(all, "parser").map((s) => s.id)).toEqual(["ses_2"]);
  });

  test("关键词两端的空白不参与匹配", () => {
    expect(matchSessions(all, "  压缩  ").map((s) => s.id)).toEqual(["ses_3"]);
  });

  test("会话 id 不参与匹配", () => {
    // 否则 "ses" 会命中全部——那串十六进制没人记得住，让它参与匹配只会制造噪音。
    expect(matchSessions(all, "ses_1")).toEqual([]);
  });

  test("没有匹配时返回空数组", () => {
    expect(matchSessions(all, "不存在的东西")).toEqual([]);
  });

  test("一个关键词同时命中两个字段的两条会话，都返回", () => {
    const mixed = [
      session("ses_a", "goseek 的压缩", "/tmp/x"),
      session("ses_b", "别的事", "/Users/me/goseek"),
    ];
    expect(matchSessions(mixed, "goseek").map((s) => s.id)).toEqual(["ses_a", "ses_b"]);
  });
});
