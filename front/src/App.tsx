// 应用骨架：左侧常驻会话列表，右侧是当前会话。
//
// 侧栏放在 Routes **外面**，因此切换会话时它不会被卸载重建——列表的滚动位置、
// 已加载的数据都保留着。这正是把它从独立页面改成侧栏的意义。

import { Route, Routes } from "react-router-dom";
import { Icon } from "./Icon";
import { SessionSidebar } from "./SessionSidebar";
import { SessionDetailPage } from "./SessionDetailPage";

export function App() {
  return (
    <div className="app">
      <SessionSidebar />
      <main className="workspace-pane">
        {/* Routes 只渲染第一个匹配上的 Route。:id 是路径参数，
            在 SessionDetailPage 里用 useParams() 取出来。 */}
        <Routes>
          <Route path="/" element={<Welcome />} />
          <Route path="/sessions/:id" element={<SessionDetailPage />} />
          <Route path="*" element={<p className="empty">页面不存在</p>} />
        </Routes>
      </main>
    </div>
  );
}

/** Welcome 是还没选中会话时右侧的首屏。 */
function Welcome() {
  return (
    <div className="welcome">
      <div className="welcome-mark">
        <Icon name="sparkles" size={30} />
      </div>
      <h1>GoSeek</h1>
      <p className="welcome-sub">
        运行在你本机的编码助手。从左侧选一个会话继续，或者新建一个开始。
      </p>
      <div className="welcome-features">
        <div className="feature-card">
          <Icon name="compress" size={18} />
          <h3>自动压缩</h3>
          <p>上下文越过触发线时把早期轮次压成分层摘要，对话不限轮次。</p>
        </div>
        <div className="feature-card">
          <Icon name="layers" size={18} />
          <h3>记忆树</h3>
          <p>摘要逐层可回查，原文一条不丢；输入 /memory 随时翻看。</p>
        </div>
        <div className="feature-card">
          <Icon name="send" size={18} />
          <h3>运行中注入</h3>
          <p>一轮还在跑也能接着说，消息会在当前步骤结束后送达模型。</p>
        </div>
      </div>
    </div>
  );
}
