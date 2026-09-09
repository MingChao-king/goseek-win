// 复制按钮。
//
// 复制的一律是**原始文本**，不是渲染后的富文本：用户复制一段回复，是要贴到别处去
// ——issue、聊天窗、编辑器——那些地方认的是 Markdown 源码。把渲染结果复制过去，
// 得到的是丢了结构的一坨纯文字。
//
// 抽成组件是因为它要出现在四个地方（代码块、助手正文、命令、命令输出），
// 而"复制成功之后给个反馈、两秒后收回"这段状态机在每处都一样。

import { useState } from "react";
import { Icon } from "./Icon";

export function CopyButton({
  text, label = "复制", className = "link",
}: {
  text: string;
  /** 按钮平时的文案。多个复制入口并排时用它区分（"复制命令" / "复制输出"）。 */
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      // 两秒后把提示收回去。这里不清理定时器：组件卸载后 setState 在 React 18+
      // 只是空操作，为它引入一个 ref 不值得。
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // 剪贴板在非安全上下文（http 且非 localhost）下不可用。面板只监听回环，
      // 因此正常情况下能用；真不能用时不报错——用户还可以自己选中复制。
    }
  }

  return (
    <button type="button" className={className} onClick={copy} title={label}>
      {copied ? <Icon name="check" size={12} /> : <Icon name="copy" size={12} />}
      {copied ? "已复制" : label}
    </button>
  );
}
