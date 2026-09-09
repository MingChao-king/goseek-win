package agent

import (
	"fmt"
	"time"

	"goseek/internal/domain"
)

// turnLog 收集一轮交互中产生的事件，并负责它们的两条去处。
//
// 事件有两种走法，区别不能含糊：
//
//   - transient（各种 delta）：立刻推给 EventSink 供展示，不落库、不占序号。
//     它们是打字动画，丢了不影响事实。
//   - durable：先攒着，等到下一个检查点连同新增消息在**一个事务里**落库，
//     提交成功之后才推给 EventSink。
//
// durable 事件之所以"先落库、再推送"，是因为序号只有在提交时才确定，而消费端
// （M3.2 的 SSE 断线重连）要靠它续传；提前推一个序号为 0 的事件，消费端就无从
// 判断自己看到哪儿了。
//
// 这个类型只在一轮交互期间存在，由 Handle 独占，因此自身不需要加锁。
type turnLog struct {
	store  Store
	sink   EventSink
	turnID domain.TurnID
	// now 提供事件时间，测试用固定时钟替换。
	now func() time.Time
	// pending 是还没落库的 durable 事件，按产生顺序排列。
	pending []domain.RunEvent
}

// newTurnLog 为一轮交互创建事件收集器。
func newTurnLog(store Store, sink EventSink, turnID domain.TurnID, now func() time.Time) *turnLog {
	return &turnLog{store: store, sink: sink, turnID: turnID, now: now}
}

// record 记下一个事件。
//
// transient 事件在这里就直接推出去；durable 事件排队等下一次 commit。
// 调用方不需要自己判断是哪一种——那个判断只在 RunEvent.Durable 里有一份。
func (log *turnLog) record(eventType domain.EventType, payload any) {
	//创建一个事件
	event := domain.NewEvent(log.turnID, eventType, payload, log.now())
	// transient 事件（各类 delta）不落库、不占序号，产生即推给 sink 用于展示。
	// 凡是流式进行的事件，不需要落库，如流式输出内容，流式输出思考
	if !event.Durable() {
		log.sink.Emit(event)
		return
	}
	// durable 事件先排队，等下一次 commit 与新增消息一起落库后再推。
	// 需要落库的事件需要加入pending中
	// 消息和事件，需要在一个事务内都落库
	log.pending = append(log.pending, event)
}

// commit 把攒下的 durable 事件连同会话的新增消息一起落库，成功后推送它们。
//
// 这就是行为底线第 8 条说的检查点：返回错误意味着这次变更没有落库，调用方必须
// 停下——继续请求模型或执行命令会产生数据库里没有记录的事实。
//
// 失败时 pending 保持原样不清空：那些事件确实还没有落库，若在这里丢掉，
// 上层若有重试路径就会静默少写一段历史。当前上层遇错即终止本轮，但把状态留在
// 正确的位置，比依赖调用方的行为更稳妥。
func (log *turnLog) commit(session *domain.Session) error {
	persisted, err := log.store.Save(session, log.pending)
	if err != nil {
		return err
	}
	log.pending = nil

	for _, event := range persisted {
		// 落库成功后，把已经分配好序号的事件推给消费端（终端或 SSE）。
		log.sink.Emit(event)
	}
	return nil
}

// fail 记录本轮失败，并尽力把它落库。
//
// 它在返回错误的路径上被调用，此时通常已经有一次保存失败了。这里再试一次保存，
// 成功则事件序列里留下一个明确的终态；失败也不能覆盖原始错误——原始错误才是
// 用户需要知道的原因，因此把落库失败包在它后面一起交出去。
//
// 为什么一定要留下终态：没有 turn.failed 的话，重放出来的历史里会有一轮永远
// 停在"正在执行工具"，而实际上它早就结束了。
func (log *turnLog) fail(session *domain.Session, cause error) error {
	log.record(domain.EventTurnFailed, domain.TurnFailedPayload{Reason: cause.Error()})
	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateFailed})

	if err := log.commit(session); err != nil {
		return fmt.Errorf("%w（另外，本轮失败状态也没能落库: %v）", cause, err)
	}
	return cause
}
