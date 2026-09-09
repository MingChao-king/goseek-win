// Package httpapi 提供 GoSeek 的 HTTP 接口：会话的增查、提交消息，以及用 SSE
// 推送运行过程。
//
// 这一层不含任何业务判断。它把 HTTP 请求翻译成对 Runner 和 store.Reader 的调用，
// 再把结果翻译回 JSON 或 SSE 帧；Agent 怎么跑、事件怎么产生，它一概不知道。
package httpapi

import (
	"log/slog"
	"sync"

	"goseek/internal/domain"
)

// subscriberBuffer 是每个订阅者的事件缓冲深度。
//
// 缓冲的意义是吸收瞬时突发：流式输出一秒钟可能产生上百个 delta，而浏览器那边的
// 写入偶尔会慢一拍。缓冲满了就说明这个订阅者是真的跟不上，此时断开它——见 publish。
const subscriberBuffer = 256

// Hub 把一个会话的运行事件广播给所有订阅者。
//
// 它独立于 Runner 存在，有自己的锁。原因是事件的产生方不止一个 goroutine：
// 模型文字来自跑 Agent 的那个 goroutine，工具输出来自 os/exec 的拷贝 goroutine
// （见 agent.EventSink 的接口注释）。把广播塞进 Runner 的 select 循环，就等于要求
// 所有产生方都先排队进那个循环，而它们本来是并行的。
type Hub struct {
	mutex sync.Mutex
	// subscribers 是当前的订阅者集合。用 map 以指针为键，退订时 O(1) 删除。
	subscribers map[*subscriber]struct{}
	// closed 表示这个 Hub 已经关闭，不再接受新订阅。
	closed bool
	logger *slog.Logger
}

// subscriber 是一个订阅者。
type subscriber struct {
	// events 是投递通道。它有缓冲，满了表示订阅者跟不上。
	events chan domain.RunEvent
	// dropped 由 publish 在丢弃这个订阅者时关闭，让 SSE 处理器知道该收尾了。
	dropped chan struct{}
	// once 保证 dropped 只被关闭一次——publish 和 Unsubscribe 都可能走到那一步。
	once sync.Once
}

// NewHub 创建一个空的广播器。
func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		subscribers: make(map[*subscriber]struct{}),
		logger:      logger,
	}
}

// Emit 实现 agent.EventSink：把事件广播给所有订阅者。
//
// 它不返回错误，也不会阻塞——这是行为底线第 12 条：事件推送失败不能改变 Agent
// 的结果。一个卡住的浏览器不该让本机的命令执行跟着停下来。
func (hub *Hub) Emit(event domain.RunEvent) {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	if hub.closed {
		return
	}

	for target := range hub.subscribers {
		select {
		// event发给每个订阅者的events通道
		case target.events <- event:
		default:
			// 缓冲已满：这个订阅者跟不上。丢弃它而不是阻塞在这里——阻塞会让整个
			// Agent 停在一个慢客户端上，而内存无限增长同样不可接受。
			//
			// 丢连接是安全的：浏览器的 EventSource 会自动重连，并带上
			// Last-Event-ID，durable 事件会从数据库里重放补齐，一条都不会少。
			// 丢掉的只有断开那一刻的 delta，而 delta 本来就不是恢复事实。
			hub.remove(target)
			hub.logger.Warn("订阅者跟不上事件流，已断开", "buffer", subscriberBuffer)
		}
	}
}

// Subscribe 加入一个订阅者，返回事件通道和一个"被丢弃"的信号。
//
// 调用方必须在结束时 Unsubscribe，否则这个订阅者会一直留在广播列表里。
func (hub *Hub) Subscribe() (<-chan domain.RunEvent, <-chan struct{}, func()) {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()

	target := &subscriber{
		events:  make(chan domain.RunEvent, subscriberBuffer),
		dropped: make(chan struct{}),
	}
	if hub.closed {
		// Hub 已关闭：直接给一个立刻结束的订阅，调用方的循环会马上退出。
		target.once.Do(func() { close(target.dropped) })
		return target.events, target.dropped, func() {}
	}
	hub.subscribers[target] = struct{}{}

	unsubscribe := func() {
		hub.mutex.Lock()
		defer hub.mutex.Unlock()
		hub.remove(target)
	}
	return target.events, target.dropped, unsubscribe
}

// Close 关闭广播器并断开全部订阅者。
func (hub *Hub) Close() {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	hub.closed = true
	for target := range hub.subscribers {
		hub.remove(target)
	}
}

// remove 从广播列表里摘掉一个订阅者并通知它。调用方必须已经持有锁。
//
// 只关闭 dropped，不关闭 events：Emit 可能正好在另一个 goroutine 里往 events 写，
// 关闭一个正在被写的通道会 panic。让 events 被 GC 回收即可。
func (hub *Hub) remove(target *subscriber) {
	delete(hub.subscribers, target)
	target.once.Do(func() { close(target.dropped) })
}
