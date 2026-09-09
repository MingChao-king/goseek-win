package contextmgr

import (
	"fmt"
	"strings"

	"goseek/internal/domain"
)

// 用户原话：摘要里唯一**不存储、每次渲染时重新派生**的部分。
//
// # 为什么完整保留用户说过的每一句话
//
// 实测它几乎免费。一段真实会话，68 条消息 82766 token：
//
//	用户消息    6 条     328 tok  =  0.4%   平均 55 tok/条，最长 226
//	工具观察   32 条   69600 tok  = 84.1%
//
// 按这个比例推下去，即使 100 万 token 的会话，全部用户原话也只有约 4000 tok。
// 而它是历史里**唯一无法重新推导**的东西：模型的输出可以重新生成，工具输出可以
// 重新执行，用户的意图不能。花 0.4% 的额度买"永远不丢用户的要求"，是这个系统里
// 最划算的一笔交易。
//
// # 为什么必须是派生的，而不是让模型写进摘要
//
// 如果摘要里有一节是"枚举型"（必须完整列出全部用户消息），那么合并两个节点时，
// 父节点的这一节必须包含两个子节点的全部条目——于是这一节的大小**单调累积**，
// "整理压缩"退化成"拼接"，层级越高的摘要反而越大。分层合并的意义就没了。
//
// 正确的结论不是"少枚举一点"，而是**枚举型内容根本不该进存储**。原始消息永不
// 删除，任何区间的用户原话都可以随时重新抽出来，而且是逐字的、100% 准确的。
//
//	                     写进摘要                   渲染时派生
//	合并时的处理     模型要逐字复制 4000 tok      模型根本不看这一节
//	层级间重复       level 1 一份、level 2 再一份  不存，无重复
//	准确性           取决于模型                   100%，且永远
//	人工修订         修订可能改坏原话             只能改散文
//
// # 一个字都不裁
//
// 用户原话逐字进入上下文，无论多长。后端不做任何截断——见 tool/output.go 里那段
// 说明：截断制造的不是"信息少了"，是"模型开始猜，而猜出来的东西和事实长得一样"。
// 用户消息是历史里唯一无法重新推导的东西，在它身上省字节最不划算。
//
// # 每条为什么带上区间
//
// 一条用户消息标记了一轮的**起点**，终点由 splitTurns 算得出来。把区间一并渲染
// 是零成本的，而它让这份列表同时成为索引：模型看到 `#13–#26 改成数据库模式吧`，
// 一步 `read from=13 count=14` 就到位。
//
// 这也是"父节点不渲染子目录"的理由——两份索引摆在一起，用户原话严格更优：条目
// 粒度是每一轮（子目录是每个子节点，覆盖多轮）、用词是用户自己的词（子目录是模型
// 写的技术词）、准确性 100%（子目录有损）。

// 这里曾经有一个 maxQuoteTokens = 500 的单条截断上限。**它已经被移除。**
//
// 后端不再截断任何东西。对用户原话来说这条尤其没有道理：用户消息只占全部内容的
// 0.4%–0.8%（下面的实测），为一个占比不到 1% 的东西设上限，等于拿一个几乎不存在的
// 成本风险去换真实的信息损失——而这些恰恰是唯一无法重新推导的内容。

// userQuote 是渲染用的一条用户原话。
//
// 它是**派生值**，不进任何存储：每次组装上下文时从 Session.Messages() 重新算。
type userQuote struct {
	// Index 是这条消息在完整历史里的序号，从 1 开始（和 read/search 的口径一致）。
	Index int
	// TurnStart 与 TurnEnd 是它引发的那一轮的序号区间，闭区间、从 1 开始。
	//
	// 闭区间而不是右开：这一栏是给**模型**看的，而模型看到 "#13–#26" 会去
	// `read from=13 count=14`，闭区间的读法更符合直觉。程序内部仍然一律右开。
	TurnStart int
	TurnEnd   int
	// Text 是这条消息的正文，**逐字原文，不做任何裁剪**。
	Text string
}

// deriveQuotes 从历史里抽出 [from, to) 区间内的全部用户原话。
//
// from/to 是消息下标（右开、从 0 开始），通常是某个摘要节点的覆盖范围。
//
// 每条都配上它所属那一轮的区间——用 turns 反查而不是"往后扫到下一条 user 消息"：
// 一轮里其实可能出现多条 user 消息（上一轮失败后用户重说了一遍，或者运行中注入
// 的消息），按 TurnID 分组才是准的。
func deriveQuotes(messages []domain.Message, turns []turnBoundary, from, to int) []userQuote {
	from = max(from, 0)
	to = min(to, len(messages))
	if from >= to {
		return nil
	}

	var quotes []userQuote
	for index := from; index < to; index++ {
		message := messages[index]
		if message.Role != domain.RoleUser {
			continue
		}
		start, end := turnRangeOf(turns, index)
		quotes = append(quotes, userQuote{
			Index:     index + 1,
			TurnStart: start,
			TurnEnd:   end,
			Text:      message.Content,
		})
	}
	// 返回一个用户消息列表，其中每条用户消息被标注所属的轮次起点终点
	return quotes
}

// turnRangeOf 返回下标 index 所在那一轮的序号区间（闭区间、从 1 开始）。
//
// 找不到时退回这条消息自己的位置：turns 是从同一份 messages 算出来的，正常情况下
// 每条消息都落在某一轮里。退回自己而不是报错，是因为区间只是给模型的导航提示，
// 算不准的代价是模型多读几条，而不是任何正确性问题。
func turnRangeOf(turns []turnBoundary, index int) (start, end int) {
	for _, turn := range turns {
		//返回结果呈现为以1作为下标起点，因为凡是模型涉及到的消息，实际上基点都是1
		if index >= turn.Start && index < turn.End {
			return turn.Start + 1, turn.End
		}
	}
	return index + 1, index + 1
}

// renderQuotes 把一段区间的用户原话渲染成摘要里的"期间用户说过"一栏。
//
// collapsedBefore 是第二条水位线：序号小于它的原话不再逐条列出，改为一行说明加上
// 模型整理出来的那段话（collapsed）。水位线为 0 时（绝大多数会话的终生状态）
// 整栏就是逐条列表。
//
// 返回空串表示这一栏整个不渲染——一个没有任何用户消息的区间（比如全是工具往返的
// 一轮）不该多出一个空标题。
func renderQuotes(quotes []userQuote, collapsedBefore int, collapsed string) string {
	var listed []userQuote
	for _, quote := range quotes {
		if quote.Index > collapsedBefore {
			listed = append(listed, quote)
		}
	}
	// 水位线之前什么都没有、之后也什么都没有：这一栏没有内容。
	if len(listed) == 0 && (collapsedBefore == 0 || collapsed == "") {
		return ""
	}

	var out strings.Builder
	out.WriteString("期间用户说过（每条带它引发那一轮的区间）：\n")

	if collapsedBefore > 0 && collapsed != "" {
		// 水位线之前的那一段：一行说明它去哪了，加上模型整理的结果。
		//
		// 说明里必须写清楚**怎么取回原文**。不写的话，模型看到"已压缩"只会认为
		// 那些话没了，而它们其实一个字都没少，就在库里。
		fmt.Fprintf(&out, "  （已压缩）#1–#%d 的用户消息已由模型整理为下面这段，"+
			"原文用 conversation_history 的 search 查：\n", collapsedBefore)
		for _, line := range strings.Split(strings.TrimSpace(collapsed), "\n") {
			fmt.Fprintf(&out, "  %s\n", line)
		}
	}

	for _, quote := range listed {
		// 区间写成 #start–#end，正好是 read from=start count=end-start+1 的输入。
		// 单条消息自成一轮时两端相同，写成 #7–#7 而不是 #7：格式统一，
		// 模型不需要分辨两种写法。
		fmt.Fprintf(&out, "  #%d–#%d  %s\n",
			quote.TurnStart, quote.TurnEnd, singleLine(quote.Text))
	}
	return out.String()
}

// singleLine 把多行文本压成一行，供列表渲染。
//
// 用户消息里常有换行（贴进来的代码、分点写的要求）。逐行原样渲染会让列表失去
// "一行一条"的结构，模型很难分清哪几行属于同一条。换行替换成 ⏎ 而不是空格：
// 保留"这里原本断过行"这个信息，同时不破坏行结构。
func singleLine(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", " ⏎ ")
}

// quotesTokens 估算一批用户原话渲染之后的 token 量。
//
// 压缩的第 3 步用它诊断"是不是用户原话把目标线撑爆了"。
func quotesTokens(quotes []userQuote, collapsedBefore int, collapsed string) int {
	return domain.EstimateTokens(renderQuotes(quotes, collapsedBefore, collapsed))
}
