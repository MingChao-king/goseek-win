package contextmgr

import "goseek/internal/domain"

// 按轮次边界切分历史。
//
// 压缩只能在**完整轮次**的边界上切。一轮从一条 user 消息开始，包含它引发的全部
// assistant 消息、工具调用和观察，直到下一条 user 消息之前。
//
// 为什么不能随便切：一次工具调用和它的观察必须在一起——供应商的协议要求每个
// tool_call 都有配对的 tool 消息，把它们分到"摘要里"和"原文里"两边，剩下的那半
// 会成为一个无法配对的悬空调用，请求直接被拒。而即使抛开协议，一个只有调用没有
// 结果的片段，模型读了也无从判断那次调用到底做成了什么。

// turnBoundary 是一个轮次在历史里的位置，右开区间 [Start, End)。
type turnBoundary struct {
	// TurnID 是这一轮的身份。
	TurnID domain.TurnID
	// Start 与 End 是它在 Session.Messages() 里的下标范围，右开。
	Start int
	End   int
}

// splitTurns 把历史切成一串轮次。
//
// 按 TurnID 的变化划界，而不是按"遇到 user 消息"：TurnID 是后端为每一轮明确生成
// 的身份，比"角色是不是 user"这种推断可靠——一轮里其实可能出现多条 user 消息
// （比如上一轮失败后用户重说了一遍，两条 user 挨在一起）。
//
// TurnID 为空的消息（M3.1 之前留下的老数据）各自成为独立的一段。它们无法与任何
// 轮次归组，单独切开至少不会把不相关的内容混进同一个摘要。
func splitTurns(messages []domain.Message) []turnBoundary {
	var turns []turnBoundary
	for index, message := range messages {
		// 与上一段属于同一轮，且这一轮的 ID 非空，就并进去。
		if len(turns) > 0 {
			//简单来说，就是逐个判断消息的turn_id是否与上一个turn一致，一致就索引继续后移，直到不一样
			//那就是一个新turn
			last := &turns[len(turns)-1]
			if last.TurnID != "" && last.TurnID == message.TurnID {
				last.End = index + 1
				continue
			}
		}
		turns = append(turns, turnBoundary{TurnID: message.TurnID, Start: index, End: index + 1})
	}
	//最后整个会话消息（包含用户、模型和工具消息）就被按照轮次进行了拆分
	return turns
}

// retainedFrom 返回"保留区"的起始下标：从这里到末尾的消息保持原文，不参与压缩。
//
// 保留最近 retained 个完整轮次。历史不足这么多轮时，全部保留——那说明会话还很短，
// 本来也没什么可压的。
//
// 轮数是参数而不是常数，因为它有两个取值：正常情况保留 retainedTurns（2）轮，
// 而当压到极限仍然超出硬边界时，压缩器会退到 minRetainedTurns（1）轮再试一次。
// "保留两轮"是让模型好用的经验值，"不超过硬边界"是请求能不能发出去的硬要求，
// 后者优先。
func retainedFrom(turns []turnBoundary, retained int) int {
	if len(turns) <= retained {
		return 0
	}
	return turns[len(turns)-retained].Start
}

// CompactableEnd 返回在保留最近 retainedTurns 轮的前提下，可压缩区的终点。
//
// 它是给调用方（Agent）判断"这次压缩会不会白做"用的：压缩的输入完全由
// (游标, 可压缩区终点) 决定，两者都没变就意味着输入没变，同样的输入不会得到
// 不同的结果——上一次没压出东西，这一次同样压不出，只会白花一次模型调用。
//
// 注意一轮之内它是不变的：可压缩区的终点由**轮次**决定，而一轮里即使追加了
// 好几条工具消息，轮次数量也没变。这正是需要它的原因——一轮里可能请求模型
// 五六次，没有这个判断就会白压五六次。
func CompactableEnd(session *domain.Session) int {
	// 默认的话，这里返回的就是倒数第二，因为后两轮不可压缩的
	return retainedFrom(splitTurns(session.Messages()), retainedTurns)
}

// compactableRange 返回本次可以压缩的消息范围 [cursor, end)。
//
// 上界是保留区的起点，并且必须落在轮次边界上——cursor 本身也一定是某个轮次的
// 起点，因为它只会被推进到叶子摘要的终点，而叶子终点就是轮次边界。
//
// 返回 ok=false 表示没有可压的内容：要么游标已经追上保留区，要么历史太短。
func compactableRange(turns []turnBoundary, cursor, retained int) (start, end int, ok bool) {
	end = retainedFrom(turns, retained)
	if end <= cursor {
		return 0, 0, false
	}
	return cursor, end, true
}
