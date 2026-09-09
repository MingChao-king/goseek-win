package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"goseek/internal/domain"
)

// sseHeartbeatInterval 是 SSE 心跳间隔。会话空闲时事件流里没有任何字节，
// 中间的 NAT / 代理 / macOS 内核会在几分钟到十几分钟后把"看起来死掉"的
// TCP 连接掐掉——前端只看到一个安静的断开，EventSource 重连时如果碰上
// 后端正忙就会得到 "Load failed"。定期发一条 SSE 注释行（冒号开头，
// EventSource 会忽略）保住连接活性，同时让断线能被快速探测到。
const sseHeartbeatInterval = 25 * time.Second

// streamEvents 用 Server-Sent Events 把一个会话的运行过程推给客户端。
//
// # SSE 是什么
//
// 一个建立在普通 HTTP 响应之上的文本协议：响应不关闭，服务端持续往里写"一行行的
// 字段"，浏览器的 EventSource 负责解析并触发回调。一帧长这样（末尾必须有空行）：
//
//	id: 42
//	data: {"sequence":42,"type":"tool.started",...}
//	<空行>
//
// 这正是 M3.1 里读模型响应用的同一个协议，只是这次我们是发送方。
//
// # 重放与实时的衔接
//
// 断线重连要能补上断开期间的事件，且不重不漏。顺序必须是**先订阅、后重放**：
//
//  1. 先向 Hub 订阅，这期间到达的实时事件进缓冲；
//  2. 再从数据库读 sequence > from 的历史事件发出去；
//  3. 然后把缓冲里序号大于已发最大值的补上；
//  4. 之后转入实时。
//
// 反过来做（先重放再订阅）会在两步之间漏掉事件——而那正是重连要解决的问题。
//
// # 为什么只有 durable 事件写 id
//
// 浏览器断线重连时会自动带上最后收到的 id 作为 Last-Event-ID。transient 事件
// （各种 delta）没有序号，给它们写 id 会把客户端的续传位置带偏——它可能记住一个
// 0，下次重连就要求"从 0 之后"，等于要求重放整个会话。
func (server *Server) streamEvents(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	// SSE 要求响应能被逐帧刷出去。拿不到 Flusher 说明中间有一层缓冲的包装，
	// 此时继续下去会让所有事件堆到连接关闭时才一起到达，不如直接说清楚。
	flusher, ok := writer.(http.Flusher)
	if !ok {
		server.writeInternal(writer, "当前响应不支持流式输出", nil)
		return
	}

	// 续传位置有两个来源：查询参数 from（前端拿快照后主动指定），以及浏览器在
	// 自动重连时带的 Last-Event-ID。后者优先——它是浏览器实际收到的最后一条，
	// 比前端记着的那个更准。
	from := parseSequence(request.URL.Query().Get("from"))
	if resumeID := request.Header.Get("Last-Event-ID"); resumeID != "" {
		from = parseSequence(resumeID)
	}

	// 打开会话（必要时创建 Runner）。订阅事件流意味着用户正在看这个会话，
	// 接下来多半就要提交消息，所以这里就把它打开。
	runner, err := server.runnerFor(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}

	// 第一步：先订阅。必须在读历史之前，否则两步之间产生的事件会丢。
	live, dropped, unsubscribe := runner.Hub().Subscribe()
	defer unsubscribe()

	writer.Header().Set("Content-Type", "text/event-stream")
	// 关掉缓存和代理缓冲，否则中间层可能把整条流攒起来再发。
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 第二步：重放历史。
	// 重放的价值在于，sse断线时，可能已经有durable，落库事件发生
	// 如果没有重放，sse不知道中断之后发生了什么，因此需要重放中断点之后的事件，由于是断线重连，也不需要从头开始重放
	// 只需要从中断点开始即可
	// 即从数据库中重新拉取这些数据
	history, err := server.reader.EventsAfter(id, from)
	if err != nil {
		server.logger.Error("重放事件失败", "session_id", string(id), "error", err)
		return
	}
	lastSent := from
	// 根据拿出的历史，写出，发送给浏览器，进行实际的回放操作
	for _, event := range history {
		if !writeEvent(writer, flusher, event) {
			return
		}
		lastSent = event.Sequence
	}

	// 第三步与第四步：把缓冲里的补上，然后持续转发。两步是同一个循环——
	// 缓冲里的事件和之后到达的实时事件走的是同一个通道，唯一的区别是前者可能
	// 与刚才重放过的重叠，靠序号跳过。
	//
	// request.Context() 在客户端断开时被取消，这是 SSE 唯一可靠的结束信号：
	// HTTP 层不会告诉我们"对方关掉了页面"，只能靠 context。
	// 心跳计时器：空闲会话每 25 秒发一条 SSE 注释，防止中间层掐断连接。
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-heartbeat.C:
			// SSE 注释行（冒号开头）对 EventSource 不可见，只是让字节流动。
			if _, err := fmt.Fprint(writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()

		// 此处由emit给event传入
		case event := <-live:
			// 重放过的 durable 事件跳过。transient 事件没有序号，不参与去重——
			// 它们只可能是刚刚产生的，不会与历史重叠。
			if event.Durable() && event.Sequence <= lastSent {
				continue
			}
			if !writeEvent(writer, flusher, event) {
				return
			}
			if event.Durable() {
				lastSent = event.Sequence
			}
			// 缓冲满
		case <-dropped:
			// 这个订阅者跟不上，已经被 Hub 摘掉。直接结束连接，浏览器会自动
			// 重连并带上 Last-Event-ID，durable 事件从数据库补齐，一条不少。
			server.logger.Warn("订阅者被丢弃，断开连接", "session_id", string(id))
			return

		case <-request.Context().Done():
			return
		}
	}
}

// writeEvent 写出一帧 SSE，返回是否写成功。
//
// 写失败通常意味着客户端已经断开。此时没有任何补救动作可做——连接都没了，
// 报错也发不出去——因此只是结束循环，让 defer 收尾。
func writeEvent(writer http.ResponseWriter, flusher http.Flusher, event domain.RunEvent) bool {
	payload, err := json.Marshal(newEventView(event))
	if err != nil {
		// payload 由本进程序列化过一次，这里不可能失败；真失败了也只能跳过这一帧，
		// 不该让整条连接断掉。
		return true
	}

	// 只有 durable 事件写 id：它是浏览器重连时的续传位置。
	if event.Durable() {
		if _, err := fmt.Fprintf(writer, "id: %d\n", event.Sequence); err != nil {
			return false
		}
	}
	// data 的值必须是单行。事件 payload 是 JSON，本身不含裸换行，因此直接写；
	// 若将来有多行内容，SSE 的规矩是拆成多个 data 行。
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", payload); err != nil {
		return false
	}

	// 每帧都刷。不刷的话内容会停在 Go 的写缓冲里，直到攒满或连接关闭——
	// 那就等于把流式退化成了一次性响应。
	flusher.Flush()
	return true
}
