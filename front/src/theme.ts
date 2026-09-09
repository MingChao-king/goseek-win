// 主题系统：四档——跟随系统（默认）/ 亮色 / 暗色 / Claude 暖纸。
//
// 选择持久化在 localStorage，纯前端：它只影响这块屏幕，不进后端配置。
//
// 工作机制：给 <html> 写 data-theme 属性，CSS 用 :root[data-theme="…"] 覆写
// token。"system" 是缺省值——不写属性，由 prefers-color-scheme 媒体查询接管，
// 并监听系统变化实时跟随。
//
// 首帧闪烁由 index.html 里的内联脚本解决：它在 React 挂载前同步执行 apply。

export type Theme = "system" | "light" | "dark" | "claude";

export const themeNames: Record<Theme, string> = {
  system: "跟随系统",
  light: "亮色",
  dark: "暗色",
  claude: "Claude 暖纸",
};

export const themeOrder: Theme[] = ["system", "light", "dark", "claude"];

const storageKey = "goseek.theme";

/** loadTheme 读持久化的选择；没存过或值非法都是 system。 */
export function loadTheme(): Theme {
  // ?theme= 只影响当前这一次加载，不持久化。用途有二：分享一个带主题的链接；
  // 以及自动化验证——那是唯一能在外部指定主题的方式（localStorage 写不进去）。
  try {
    const params = new URLSearchParams(window.location.search);
    const query = params.get("theme");
    if (query === "light" || query === "dark" || query === "claude") {
      // 读完即清：?theme= 只影响这一次加载。留在地址栏的话，用户把这个链接
      // 分享出去，对方也被迫套上这个主题——那不是"对方的选择"。
      params.delete("theme");
      const rest = params.toString();
      window.history.replaceState(null, "", window.location.pathname + (rest ? `?${rest}` : "") + window.location.hash);
      return query;
    }
    const raw = localStorage.getItem(storageKey);
    if (raw === "light" || raw === "dark" || raw === "claude") return raw;
  } catch {
    // localStorage 在极少数隐私模式下不可用，此时每次进页面都是 system。
  }
  return "system";
}

/** applyTheme 把选择写到 <html>。system 不写属性，交给媒体查询。 */
export function applyTheme(theme: Theme): void {
  if (theme === "system") {
    delete document.documentElement.dataset.theme;
  } else {
    document.documentElement.dataset.theme = theme;
  }
}

/** saveTheme 持久化并立即应用。 */
export function saveTheme(theme: Theme): void {
  try {
    localStorage.setItem(storageKey, theme);
  } catch {
    // 存不上就只是不跨会话记忆，界面照常工作。
  }
  applyTheme(theme);
}

