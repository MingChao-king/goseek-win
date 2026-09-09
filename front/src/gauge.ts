// 上下文仪表盘的纯计算部分。
//
// 和渲染分开是为了能直接测：几何和分档都是"给同样的输入必须得到同样的输出"的
// 东西，不该为了验证它们去搭一套 DOM。

/** 仪表的几何参数。半圆，从左到右扫过 180°。 */
export const gaugeRadius = 34;
export const gaugeStroke = 7;
export const gaugeSize = gaugeRadius * 2 + gaugeStroke;

/**
 * arcPath 生成一段从左端起、扫过 `fraction` 比例的半圆弧。
 *
 * SVG 的圆弧写法是 `A rx ry rotation large-arc sweep x y`。这里最多半圈，
 * 永远不会超过 180°，所以 large-arc 恒为 0；sweep=1 表示顺时针。
 *
 * fraction 会被夹到 [0,1]：占用比例可能超过 100%（估算偏高时确实会），
 * 弧长截到满是为了不撑破布局——**但百分比数字不截**，截数字是隐瞒。
 */
export function arcPath(fraction: number): string {
  const clamped = Math.max(0, Math.min(1, fraction));
  const angle = Math.PI * clamped; // 0 → 左端，π → 右端
  const center = gaugeSize / 2;
  // 起点固定在左端，终点按角度算。y 取负是因为 SVG 的 y 轴向下。
  const endX = center - gaugeRadius * Math.cos(angle);
  const endY = center - gaugeRadius * Math.sin(angle);
  return `M ${center - gaugeRadius} ${center} A ${gaugeRadius} ${gaugeRadius} 0 0 1 ${endX} ${endY}`;
}

/** 占用的三档。接近满的时候要显眼——那是该压缩的信号。 */
export type GaugeLevel = "ok" | "warn" | "danger";

/** gaugeLevel 按占用百分比分档。分界与终端那边保持一致。 */
export function gaugeLevel(percent: number): GaugeLevel {
  if (percent >= 90) return "danger";
  if (percent >= 70) return "warn";
  return "ok";
}

/** compactTokens 把 token 数缩写成 4.2k 这样，仪表盘里放得下。 */
export function compactTokens(tokens: number): string {
  if (tokens < 1000) return String(tokens);
  return `${(tokens / 1000).toFixed(1)}k`;
}
