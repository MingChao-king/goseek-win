// 草稿的存取与**清除时机**。
//
// 后者才是重点：存多存少没有代价，清早了就是数据丢失。所以这里除了往返，
// 还钉住"哪些情况不清"。

import { beforeEach, describe, expect, test } from "bun:test";
import { clearDraft, loadDraft, saveDraft } from "./draft";

/** 一个够用的 localStorage 替身。bun 的测试环境里没有 window。 */
function installStorage(): Map<string, string> {
  const store = new Map<string, string>();
  (globalThis as { window?: unknown }).window = {
    localStorage: {
      getItem: (key: string) => store.get(key) ?? null,
      setItem: (key: string, value: string) => void store.set(key, value),
      removeItem: (key: string) => void store.delete(key),
    },
  };
  return store;
}

let store: Map<string, string>;
beforeEach(() => {
  store = installStorage();
});

describe("草稿", () => {
  test("存进去能取出来", () => {
    saveDraft("ses_a", "写了一半");
    expect(loadDraft("ses_a")).toBe("写了一半");
  });

  test("按会话隔离：一个会话的草稿不会串到另一个", () => {
    saveDraft("ses_a", "甲的草稿");
    saveDraft("ses_b", "乙的草稿");
    expect(loadDraft("ses_a")).toBe("甲的草稿");
    expect(loadDraft("ses_b")).toBe("乙的草稿");
  });

  test("没存过的会话取出空串，而不是 null", () => {
    // 返回 null 的话，调用方要么多写一次判空，要么把 "null" 显示到输入框里。
    expect(loadDraft("ses_never")).toBe("");
  });

  test("空串等同于清除，不在存储里留空项", () => {
    saveDraft("ses_a", "有内容");
    saveDraft("ses_a", "");
    expect(store.has("goseek.draft.v1.ses_a")).toBe(false);
  });

  test("clearDraft 之后取到空串", () => {
    saveDraft("ses_a", "发出去了");
    clearDraft("ses_a");
    expect(loadDraft("ses_a")).toBe("");
  });

  test("覆盖写：后写的赢", () => {
    saveDraft("ses_a", "第一版");
    saveDraft("ses_a", "第二版");
    expect(loadDraft("ses_a")).toBe("第二版");
  });

  test("会话 id 为空时不读不写——没有归属的草稿不该被存下来", () => {
    saveDraft("", "无主草稿");
    expect(store.size).toBe(0);
    expect(loadDraft("")).toBe("");
  });

  test("localStorage 抛异常时不炸，只是丢草稿", () => {
    // 隐私模式、配额满都会抛。草稿丢了是小事，页面挂掉不是。
    (globalThis as { window?: unknown }).window = {
      localStorage: {
        getItem: () => { throw new Error("SecurityError"); },
        setItem: () => { throw new Error("QuotaExceededError"); },
        removeItem: () => { throw new Error("SecurityError"); },
      },
    };
    expect(() => saveDraft("ses_a", "x")).not.toThrow();
    expect(loadDraft("ses_a")).toBe("");
  });
});
