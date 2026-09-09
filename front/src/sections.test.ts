import { describe, expect, test } from "bun:test";
import { parseSections, serializeSections } from "./sections";

const wellFormed = `主题：M5.1 压缩算法重做
目标与约束：至多三次模型调用；上下文永远低于硬边界。
用户要求注释写详细。
关键概念与决定：用户原话渲染时派生，不进存储。
涉及的文件与代码位置：internal/contextmgr/quotes.go
做成了什么：七节散文落地，测试通过。
失败与不该重走的路：成对合并是 O(K)，K≥20 必撞上限。
未完成 / 阻塞：真机验证还没做。`;

describe("七节解析", () => {
  test("正常格式拆成七节", () => {
    const sections = parseSections(wellFormed);
    expect(sections.map((s) => s.name)).toEqual([
      "主题", "目标与约束", "关键概念与决定", "涉及的文件与代码位置",
      "做成了什么", "失败与不该重走的路", "未完成 / 阻塞",
    ]);
  });

  test("一节里的多行都归给这一节", () => {
    const sections = parseSections(wellFormed);
    const goal = sections.find((s) => s.name === "目标与约束");
    expect(goal?.body).toContain("至多三次模型调用");
    expect(goal?.body).toContain("用户要求注释写详细");
  });

  test("缺节就少几节，不补空的", () => {
    // 不做机械校验：补一个空节是在给模型的能力打补丁，还会掩盖"这个模型不够用"。
    const sections = parseSections("主题：只写了一节\n做成了什么：别的都没写");
    expect(sections.map((s) => s.name)).toEqual(["主题", "做成了什么"]);
  });

  test("模型自己起的节名照样展示", () => {
    const sections = parseSections("主题：X\n额外的一节：模型多写的");
    expect(sections.map((s) => s.name)).toEqual(["主题", "额外的一节"]);
  });

  test("带序号前缀的节名也认", () => {
    // 提示词里节名带编号，模型两种都会写，而这个差别对用户没有意义。
    const sections = parseSections("1. 主题：带编号\n2. 目标与约束：也带编号");
    expect(sections.map((s) => s.name)).toEqual(["主题", "目标与约束"]);
  });

  test("半角冒号也认", () => {
    expect(parseSections("主题: 半角").map((s) => s.name)).toEqual(["主题"]);
  });

  test("完全不符合格式时返回空数组，让调用方整段降级", () => {
    // 返回"名字为空的单节"看起来更统一，但那会让调用方分不清
    // "模型没分节"和"模型写了一节叫空字符串"。
    expect(parseSections("这就是一段没有任何分节的普通文字。")).toEqual([]);
  });

  test("开头有闲话时整段降级，不猜从哪一行开始算", () => {
    expect(parseSections("好的，下面是摘要：\n主题：X")).toEqual([]);
  });

  test("正文里的冒号不会被误认成节名", () => {
    // 中文正文里冒号很常见。真正的节名都很短，而正文句子往往更长。
    const sections = parseSections(
      "主题：X\n做成了什么：查明了原因，结论是这样的：那个常量在写下来的时候是对的，后来悄悄过期了。",
    );
    expect(sections.map((s) => s.name)).toEqual(["主题", "做成了什么"]);
  });
});

describe("七节序列化", () => {
  test("与解析互为逆运算", () => {
    // 后端的 PATCH 收的是整段正文，界面上改一节实际发的是重新拼出来的全文。
    // 往返不一致就意味着"改 A 节顺手改坏了 B 节"。
    const once = parseSections(wellFormed);
    const twice = parseSections(serializeSections(once));
    expect(twice).toEqual(once);
  });

  test("改一节不影响别的节", () => {
    const sections = parseSections(wellFormed);
    const edited = sections.map((s) =>
      s.name === "未完成 / 阻塞" ? { ...s, body: "真机验证已经做完" } : s);
    const reparsed = parseSections(serializeSections(edited));

    expect(reparsed.find((s) => s.name === "未完成 / 阻塞")?.body).toBe("真机验证已经做完");
    expect(reparsed.find((s) => s.name === "主题")?.body).toBe("M5.1 压缩算法重做");
    expect(reparsed.length).toBe(sections.length);
  });

  test("空节被丢掉，不留空标题", () => {
    const text = serializeSections([
      { name: "主题", body: "X" },
      { name: "未完成 / 阻塞", body: "   " },
    ]);
    expect(text).toBe("主题：X");
  });

  test("空数组序列化成空串", () => {
    expect(serializeSections([])).toBe("");
  });
});
