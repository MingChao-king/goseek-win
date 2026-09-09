// 前端的入口：把 React 应用挂到 index.html 里那个空 div 上。

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { applyTheme, loadTheme } from "./theme";
import "./styles.css";

// 挂载前应用主题。index.html 的内联脚本已经处理过持久化值（首帧不闪），
// 这里再跑一次是为了接住 ?theme= 这类只在 JS 里可读的来源。
applyTheme(loadTheme());

const container = document.getElementById("root");
if (!container) {
  throw new Error("找不到 #root 挂载点");
}

createRoot(container).render(
  // StrictMode 只在开发时生效：它会故意把组件渲染两遍、把 effect 执行两遍，
  // 用来暴露"没写清理函数"和"reducer 有副作用"这类问题。生产构建里它什么都不做。
  <StrictMode>
    {/* BrowserRouter 提供基于 URL 路径的路由，让 /sessions/xxx 这样的地址
        直接可用（而不是 #/sessions/xxx）。 */}
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
