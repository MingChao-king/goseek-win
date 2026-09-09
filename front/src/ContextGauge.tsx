// 上下文占用仪表盘。
//
// # 为什么是仪表盘，为什么在输入框下面
//
// 它回答的是"我还能聊多久"，属于**输入的约束**，不是某条消息的属性。所以：
//
//   - 位置紧贴输入框，而不是挤在页顶和工作目录、运行状态混在一起；
//   - **绝不出现在任何一条消息里**——上下文占用不是对话的一部分。
//
// 用弧形仪表而不是一行数字：数字要读完才知道多少，弧形一眼就能看出"快满了"。
// 数字仍然给出来，因为"还剩多少 token"有时确实需要精确值。

import { arcPath, compactTokens, gaugeLevel, gaugeSize, gaugeStroke } from "./gauge";
import type { ContextUsage } from "./types";

/**
 * ContextGauge 显示下一次请求会占多少上下文。
 *
 * 三种状态各自要说清楚，不能含糊过去：
 *   - 还没请求过模型：没有数字可显示，整个仪表不出现；
 *   - 有数字但没配置窗口：只报绝对值，**不编一个比例出来**；
 *   - 都有：弧形 + 百分比 + 绝对值 + 这个数字是估算还是实测。
 */
export function ContextGauge({ usage }: { usage: ContextUsage | null }) {
  if (!usage) return null;

  // 没有窗口大小就算不出比例。这里不猜一个默认值——一个假的窗口会让用户对着
  // 错误的百分比做判断。
  if (usage.context_window <= 0) {
    return (
      <div className="gauge-row">
        <span className="gauge-flat">上下文 {usage.input_tokens.toLocaleString()} tokens</span>
        <span className="gauge-note">未配置窗口大小，无法计算占用比例</span>
      </div>
    );
  }

  // 比例可能超过 100%（估算偏高时确实会）。弧长截到满，但**数字如实显示**——
  // 截断弧长是为了不撑破布局，截断数字则是隐瞒。
  const percent = usage.ratio * 100;
  const level = gaugeLevel(percent);
  const sourceLabel = usage.source === "provider" ? "供应商实测" : "本地估算";

  return (
    <div className="gauge-row">
      <svg className="gauge-dial" width={gaugeSize} height={gaugeSize / 2 + gaugeStroke} viewBox={`0 0 ${gaugeSize} ${gaugeSize / 2 + gaugeStroke}`}>
        {/* 底弧：整个半圆，表示窗口总量。 */}
        <path d={arcPath(1)} className="gauge-track" strokeWidth={gaugeStroke} fill="none" strokeLinecap="round" />
        {/* 占用弧。 */}
        <path
          d={arcPath(usage.ratio)}
          className={`gauge-arc gauge-${level}`}
          strokeWidth={gaugeStroke}
          fill="none"
          strokeLinecap="round"
        />
        <text x={gaugeSize / 2} y={gaugeSize / 2 - 4} className="gauge-percent" textAnchor="middle">
          {percent.toFixed(0)}%
        </text>
      </svg>

      <div className="gauge-detail">
        <span className={`gauge-figure gauge-text-${level}`}>
          {compactTokens(usage.input_tokens)} / {compactTokens(usage.context_window)}
        </span>
        <span className="gauge-note">
          剩余 {usage.remaining.toLocaleString()} · {sourceLabel}
          {/* 缓存命中率只在供应商报了的时候显示。判空用 !== undefined 而不是真值
              判断：**0 是有意义的值**——压缩刚换掉整条前缀时正好是 0，而那恰恰是
              最值得看的一次。用 `ratio &&` 会把它悄悄藏起来。 */}
          {usage.cache_hit_ratio !== undefined && (
            <> · 缓存 {Math.round(usage.cache_hit_ratio * 100)}%</>
          )}
        </span>
      </div>
    </div>
  );
}
