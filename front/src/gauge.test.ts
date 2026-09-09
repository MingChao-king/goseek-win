import { expect, test } from "bun:test";
import { arcPath, compactTokens, gaugeLevel, gaugeRadius, gaugeSize } from "./gauge";

// 三档的分界要和终端那边一致：70% 转黄、90% 转红。
test("占用分档", () => {
  expect(gaugeLevel(0)).toBe("ok");
  expect(gaugeLevel(69.9)).toBe("ok");
  expect(gaugeLevel(70)).toBe("warn");
  expect(gaugeLevel(89.9)).toBe("warn");
  expect(gaugeLevel(90)).toBe("danger");
  // 估算偏高时比例真的会超过 100%，不能因此跌出分档。
  expect(gaugeLevel(130)).toBe("danger");
});

test("token 数缩写", () => {
  expect(compactTokens(0)).toBe("0");
  expect(compactTokens(999)).toBe("999");
  expect(compactTokens(4200)).toBe("4.2k");
  expect(compactTokens(128000)).toBe("128.0k");
});

// 空弧的终点就是起点（左端），满弧的终点在右端——半圆的两头。
test("弧的两个端点", () => {
  const center = gaugeSize / 2;
  const left = center - gaugeRadius;
  const right = center + gaugeRadius;

  expect(arcPath(0)).toContain(`M ${left} ${center}`);
  // 浮点数直接比字符串不可靠，取出终点坐标比较。
  const [x, y] = endpointOf(arcPath(1));
  expect(x).toBeCloseTo(right, 6);
  expect(y).toBeCloseTo(center, 6);
});

// 弧长截到满，但这只是不让布局被撑破——百分比数字仍然如实显示（见 ContextGauge）。
test("超过 100% 的占用不会画出超过半圈的弧", () => {
  expect(arcPath(1.4)).toBe(arcPath(1));
  expect(arcPath(-0.3)).toBe(arcPath(0));
});

// 弧随占用单调增长：终点的 x 从左端一路走到右端。
test("弧随占用单调增长", () => {
  const xs = [0, 0.25, 0.5, 0.75, 1].map((fraction) => endpointOf(arcPath(fraction))[0]);
  for (let index = 1; index < xs.length; index++) {
    expect(xs[index]).toBeGreaterThan(xs[index - 1]);
  }
});

/** endpointOf 从路径字符串里取出终点坐标。 */
function endpointOf(path: string): [number, number] {
  const parts = path.split(" ");
  return [Number(parts[parts.length - 2]), Number(parts[parts.length - 1])];
}

// 缓存命中率：0 必须能显示出来。
//
// 0 命中是一个有意义的值——压缩刚换掉整条前缀时正好是 0，而那恰恰是最值得看的
// 一次。用真值判断（`ratio && ...`）会把它和"供应商没报"一起藏掉，于是唯一需要
// 报警的那种情况反而不显示。
test("缓存命中率：缺席与 0 必须能区分", () => {
  const absent: number | undefined = undefined;
  const zero: number | undefined = 0;

  expect(absent !== undefined).toBe(false);
  expect(zero !== undefined).toBe(true);
  // 反面：真值判断会把 0 误判成缺席，这正是要避免的写法。
  expect(Boolean(zero)).toBe(false);
});
