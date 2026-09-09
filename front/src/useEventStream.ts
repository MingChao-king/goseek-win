// 订阅一个会话的实时事件流。
//
// 这是整个前端唯一涉及"长连接"的地方，也是最容易写出资源泄漏的地方，
// 因此单独抽成一个 hook 并写清生命周期。

import { useEffect, useState } from "react";
import type { RunEvent } from "./types";

/**
 * 连接状态。
 *
 * 之所以要把它暴露出来：事件流断了的时候界面会**安静地停住**——没有新的 delta、
 * 没有状态变化，看起来和"模型正在思考"一模一样。用户会一直等下去。
 */
export type StreamStatus =
  /** 还没建立连接（会话尚未加载完，或刚发起）。 */
  | "idle"
  /** 连接已打开，正在收事件。 */
  | "open"
  /** 断开了，EventSource 正在自动重连。 */
  | "reconnecting";

/**
 * useEventStream 在组件挂载期间保持一条 SSE 连接，把收到的事件交给 onEvent。
 *
 * # EventSource 是什么
 *
 * 浏览器内置的 SSE 客户端。给它一个 URL，它就保持一条 HTTP 连接，服务端每推
 * 一帧就触发一次 onmessage。它还自带**断线自动重连**：连接掉了会等一会儿再连，
 * 并把最后收到的那条 id 放进 Last-Event-ID 请求头——后端据此从那个序号之后
 * 继续推，这就是断开期间的事件能补齐的原因。
 *
 * 也正因为它自动重连，不需要在这里写重试逻辑；要做的只是在组件卸载时关掉它。
 *
 * # 参数
 *
 * @param sessionID 要订阅的会话；为 null 时不建立连接。
 * @param from      从哪个序号之后开始要。快照的 last_sequence 正是这个值。
 * @param onEvent   收到事件时调用。
 * @returns         当前连接状态，供界面显示。
 *
 * # 依赖数组为什么是这三个
 *
 * useEffect 的第二个参数决定"什么变化时要重新执行这个副作用"。这里只要
 * sessionID 或 from 变了就必须重连——换会话了，或者续传起点变了。
 *
 * onEvent 也在依赖里，因为 effect 内部用到了它；调用方必须用 useCallback 把它
 * 包稳定，否则每次渲染都是一个新函数，effect 会跟着重跑，连接被反复重建。
 * 这是 React 里最常见的一类性能 bug，所以在这里明确写出来。
 */
export function useEventStream(
  sessionID: string | null,
  from: number,
  onEvent: (event: RunEvent) => void,
): StreamStatus {
  const [status, setStatus] = useState<StreamStatus>("idle");

  useEffect(() => {
    if (!sessionID) {
      setStatus("idle");
      return;
    }

    let source: EventSource | null = new EventSource(
      `/api/v1/sessions/${sessionID}/events?from=${from}`,
    );
    let frame: number | null = null;
    let queued: RunEvent[] = [];
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let retryDelay = 1500;

    /** reconnect 关掉当前连接并重建。事件续传靠 Last-Event-ID，不丢事件。 */
    const reconnect = () => {
      source?.close();
      source = new EventSource(
        `/api/v1/sessions/${sessionID}/events?from=${from}`,
      );
      attach(source);
    };

    /** attach 把回调挂到一个 EventSource 实例上（初次与重连共用）。 */
    const attach = (es: EventSource) => {
      es.onopen = () => {
        retryDelay = 1500;
        setStatus("open");
      };
      es.onmessage = (message: MessageEvent<string>) => {
        setStatus("open");
        try {
          queued.push(JSON.parse(message.data) as RunEvent);
          if (frame === null) frame = requestAnimationFrame(flush);
        } catch {
          // 单帧解析失败只丢这一帧。整条连接不该因为一帧坏掉而断开——
          // durable 事件在数据库里，重连时会重放回来。
        }
      };
      es.onerror = () => {
        setStatus("reconnecting");
        if (es.readyState === EventSource.CLOSED && !retryTimer) {
          retryDelay = Math.min(retryDelay * 2, 15000);
          retryTimer = setTimeout(() => {
            retryTimer = null;
            reconnect();
          }, retryDelay);
        }
      };
    };

    attach(source);

    // 模型可能在一秒内推来几十个文字片段。逐片 dispatch 会让长会话逐片重排；
    // 收到的顺序仍然保留，但统一在下一帧交给 React，一帧最多绘制一次。
    const flush = () => {
      frame = null;
      const batch = queued;
      queued = [];
      for (const event of batch) onEvent(event);
    };



    // useEffect 返回的函数是**清理函数**：组件卸载时、以及依赖变化导致这个
    // effect 重新执行之前，React 都会先调用它。
    //
    // 不关的话，每次切换会话都会多留一条连接，而每条连接在后端都对应一个订阅者
    // 和一个 goroutine——几次切换之后就是一堆没人读的流。
    return () => {
      if (retryTimer) clearTimeout(retryTimer);
      source?.close();
      if (frame !== null) cancelAnimationFrame(frame);
      queued = [];
      setStatus("idle");
    };
  }, [sessionID, from, onEvent]);

  return status;
}
