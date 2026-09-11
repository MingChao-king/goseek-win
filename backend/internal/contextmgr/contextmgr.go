// Package contextmgr 把会话历史组装成一次模型调用的输入视图。
//
// 会话历史是已经发生的事实，视图是只对一次调用有效的临时输入，两者不是同一个对象。
// 本包是这条边界所在：Agent 不直接把历史交给模型，模型看到什么完全由这里决定。
//
// "消息按什么顺序送入模型"属于确定性协议，因此由代码而不是模型负责。
package contextmgr

import (
	"strings"

	"goseek/internal/domain"
)

// systemPrompt 是每次调用都会前置的稳定指令。
//
// 它由 ContextManager 持有而不是由 Agent 或会话持有：它是模型可见视图的一部分，
// 不是会话事实，因此永远不会进入历史。
// systemPromptBase 是系统指令的固定部分：身份、行为约定和结果格式。
// 它不包含具体工具的使用指导（那是 ToolSpec 的责任）和 skill 清单（动态追加）。
const systemPromptBase = `你是 GoSeek，一个运行在用户本机的编码助手，通过命令行与用户交流。

你可以调用工具在用户的机器上执行操作。凡是需要了解文件、目录或程序真实状态才能回答的
问题，先调用工具确认，不要凭猜测断言事实。用户提到的文件名和路径可能不准确，用工具核对
之后再继续。

## 文件操作

读取文件用 read_file，写入文件用 write_file，文本搜索用 search。read_file 和
write_file 接受当前工作目录下的相对路径或绝对路径；search 默认搜索当前工作
目录，也可用 path 参数搜索其他目录。bash 用于运行命令、程序和管道，不用于读写
单个文件。

## Skill 系统

GoSeek 支持用户安装的 skill（技能指令）。当用户提到某个 skill、使用 /skill: 前缀、
或者你判断某个 skill 的指引会帮助完成任务时：先用 list_skills 查看有哪些可用，
再用 load_skill 读取完整指令，然后按照指令行动。skill 的正文只对当次请求有效，
需要时重新加载。

## 多模态

用户可能发送图片（截图、照片、图表）。看到图片时，先描述你看到了什么（它和你手头
任务的关系），再继续回答或行动。如果图片和文字同时出现，图片是对文字的补充
或者就是任务的对象本身。

## 工具结果格式

工具结果以 JSON 返回，字段含义如下：

- status：success 表示工具正常完成，error 表示调用被阻止或执行失败；
- content：完整内容，不会被截断；
- exit_code：命令的退出码（bash 工具才有）。

status=success 且 content 为空，只能说明工具成功执行且没有输出，不能据此断定某个文件
不存在或某项工作已经完成。

调用 bash 时，用 purpose 一句话说明这条命令想达成什么，它会展示给用户。

只陈述工具结果能够证明的事情。需要继续操作时就继续调用工具；确认工作已经完成后，用一条
不含工具调用的回复告诉用户你做了什么、依据是什么。使用用户所用的语言，保持简洁直接。`

// SystemPrompt 返回当前的系统提示词。availableSkills 是 skill 名称列表（可为空），
// 非空时在末尾附上清单，让模型知道有哪些 skill 存在。
func SystemPrompt(availableSkills []string) string {
	if len(availableSkills) == 0 {
		return systemPromptBase
	}
	return systemPromptBase + "\n\n## 已安装的 skills\n\n" +
		strings.Join(availableSkills, "、") +
		"\n用 list_skills 查看描述，用 load_skill 读取某个 skill 的完整指令。"
}

// ContextView 是为一次模型调用组装出来的全部输入，以及它占了多少上下文容量。
//
// 它从 M0 的一个裸 []ModelMessage 升格成结构体，正是因为现在有了第二个字段——
// M0 的设计里预告过这一刻："ContextView 作为概念从 M0 就存在，作为类型要等到有
// 第二个字段。"
//
// 它只服务这一次调用，调用方不应保存它：下一轮的历史不一样，占用也不一样。
type ContextView struct {
	// Messages 是本次要发给模型的完整消息序列。
	Messages []domain.ModelMessage
	// Usage 是这份视图的上下文占用。此刻它一定是本地估算的（Source=estimated），
	// 供应商的实测值要等响应回来才有。
	Usage domain.ContextUsage
}

// Build 组装本次模型调用的输入视图。
//
// 视图由三段拼成，顺序固定：
//
//	[system 指令] + [活跃摘要（较早对话的压缩）] + [游标之后的原始消息]
//
// 中间那段在没有压缩过的会话里是空的，此时视图就退化成 M0 的样子：指令加全部历史。
//
// 摘要与原文之间是**无缝衔接**的：活跃摘要恰好覆盖 [0, cursor)，原文从 cursor 开始，
// 两者拼起来正好是完整历史的一个分割，不重不漏。这条不变量由 ConversationMemory
// 保证（见它的 CheckInvariant）。
//
// tools 参与的是**计数**而不是消息序列：工具定义由 Agent 直接交给 Model（视图管的
// 是"模型看到哪些消息"），但它们同样占额度，不算进去会让估算系统性偏低。而估算
// 偏低正是要避免的方向——它会让请求被供应商拒。
//
// contextWindow 为 0 表示用户没有配置窗口大小；此时占用的 Source 是 unknown，
// 界面显示"未知"而不是编一个比例。
func Build(
	session *domain.Session,
	memory domain.ConversationMemory,
	tools []domain.ToolSpec,
	contextWindow int,
) ContextView {
	return BuildWith(session, memory, tools, contextWindow, domain.DefaultSafetyFactor, nil)
}

// BuildWith 与 Build 相同，但用调用方指定的安全系数估算占用。
//
// 系数会随"上界被击穿"而上调（见 domain.RaisedSafetyFactor），因此持有它的
// Agent 要能把当前值传进来——否则自我校验调高了系数，实际估算却还在用旧值。
func BuildWith(
	session *domain.Session,
	memory domain.ConversationMemory,
	tools []domain.ToolSpec,
	contextWindow int,
	safetyFactor float64,
	skills []string,
) ContextView {
	return BuildWithAmbient(session, memory, tools, contextWindow, safetyFactor, skills, nil)
}

// BuildWithAmbient 与 BuildWith 相同，但会把本次请求专属的临时界面上下文附到
// 最近的用户消息上。它只改模型视图的副本，不改会话历史。
//
// ambient 按对应用户消息的提交顺序排列，空字符串是有意义的占位：运行中连续提交
// 多条消息时，只有这样才能保证每份侧栏快照仍绑定到原来的那条消息。
func BuildWithAmbient(
	session *domain.Session,
	memory domain.ConversationMemory,
	tools []domain.ToolSpec,
	contextWindow int,
	safetyFactor float64,
	skills []string,
	ambient []string,
) ContextView {
	messages := buildMessages(session.Messages(), memory, skills)
	applyAmbientContext(messages, ambient)

	// 用即将真正发出去的那份请求来计数，而不是只数消息——这样估算的对象和实际
	// 发送的对象是同一个，不会因为漏掉工具定义之类的东西而偏低。
	usage := domain.ContextUsage{
		//比如说ds，系统的配置就是1m上下文
		ContextWindow: contextWindow,
		// 这里预估输入token大小
		InputTokens: domain.EstimateRequestTokensWith(
			domain.ModelRequest{Messages: messages, Tools: tools}, safetyFactor),
		//表示来源为本地预估，因为模型提供商在返回结果时，会给出本次实际的token用量
		Source: domain.ContextUsageEstimated,
	}
	if contextWindow <= 0 {
		// 没有窗口就没有比例可言。仍然给出 InputTokens——它本身是有意义的，
		// 只是无法判断"占了多少"。
		usage.Source = domain.ContextUsageUnknown
	}

	return ContextView{Messages: messages, Usage: usage}
}

// applyAmbientContext 从后往前找最近的用户消息，把临时上下文附在对应正文之前。
// 视图里的 message 都是新值，可以原地改；session.Messages() 返回的历史不受影响。
func applyAmbientContext(messages []domain.ModelMessage, ambient []string) {
	if len(ambient) == 0 {
		return
	}
	contextIndex := len(ambient) - 1
	for messageIndex := len(messages) - 1; messageIndex >= 0 && contextIndex >= 0; messageIndex-- {
		if messages[messageIndex].Role != domain.ModelRoleUser {
			continue
		}
		if content := strings.TrimSpace(ambient[contextIndex]); content != "" {
			messages[messageIndex].Content = content + "\n\n[用户原文]\n" + messages[messageIndex].Content
		}
		contextIndex--
	}
}

// buildMessages 拼出模型这次能看到的消息序列。
//
// 它被两处调用：真正发请求时（Build），以及压缩过程中反复估算时（Compactor）。
// **必须是同一个函数**——两处用不同方式拼消息，算出来的 token 就对不上，压缩会
// 依据一个和实际请求不一样的数字来决定停不停。
// 入参的history，是当前会话的所有消息，当然，模型需要的上下文视图并不需要这样的全部消息
func buildMessages(history []domain.Message, memory domain.ConversationMemory, skills []string) []domain.ModelMessage {
	// 当前的有效摘要，在多次合并过程中，部分老摘要已经被合并，所以并非active
	active := memory.ActiveBatches()
	messages := make([]domain.ModelMessage, 0, len(history)+len(active)+1)
	//把系统提示词加进去
	messages = append(messages, domain.ModelMessage{
		Role:    domain.ModelRoleSystem,
		Content: SystemPrompt(skills),
	})
	// 这里使用renderMemory，这个时候传入的就是被编辑过的摘要（如果被编辑）
	// 确保对摘要编辑之后，可以正确的参与上下文构建、压缩等过程
	//
	// 除了摘要本身，还要把**完整历史**交给它：摘要里"期间用户说过"那一栏是
	// 渲染时从原始消息派生的，不进存储（理由见 quotes.go）。turns 一并传进去，
	// 免得每个节点各切一遍——切分的结果对整个历史是同一份。
	messages = append(messages, renderMemory(
		active, history, splitTurns(history),
		memory.QuotesCollapsedBefore, memory.CollapsedQuotes)...)

	// 接缝：摘要（assistant 角色）与原文之间可能需要补一条用户消息。**必须在这里，
	// 不能在末尾**——理由见 bridgeSummaryToRaw。
	messages = append(messages, bridgeSummaryToRaw(active, history, memory.RawCompactionCursor)...)

	// 游标之后的原始消息。游标之前的已经被上面的摘要覆盖了。
	//
	// 防御性地夹一下范围：游标理论上不会越界（它只会被推进到某个叶子的终点，
	// 而叶子终点来自当时的历史长度，而历史只增不减），但一旦数据被外部改过，
	// 这里越界会 panic，而少读几条消息只是少一点上下文。
	cursor := min(max(memory.RawCompactionCursor, 0), len(history))
	// 把压缩游标之后的未被压缩过的轮次对话消息加入到消息列表中。
	//
	// 走 renderRawMessage 而不是直接映射字段：超大的工具观察要在**视图层**收边界
	// （存储仍然完整），理由与截断/分页的区别见 observation.go。
	for offset, message := range history[cursor:] {
		messages = append(messages, renderRawMessage(message, cursor+offset))
	}
	// 由此便构建出了模型的上下文视图
	return messages
}

// bridgeSummaryToRaw 在摘要与原文的接缝处补上当前这一轮的用户消息。
//
// # 这是一条供应商协议约束，实测确证
//
// 摘要用 assistant 角色渲染（理由见 renderMemory）。如果游标之后的第一条原文**也是**
// assistant 消息，视图里就出现两条连续的 assistant——思考模型把它理解成"续写这条
// assistant 回复"，于是要求回传 `reasoning_content`：
//
//	HTTP 400: The `reasoning_content` in the thinking mode must be passed back to the API.
//
// 用库里的真实消息逐字重放，四个形状把条件钉死了：
//
//	[sys][摘要][assistant+tool_calls][tool]        → 400
//	[sys][assistant+tool_calls][tool]              → 正常（没有摘要，不构成连续）
//	[sys][user][assistant+tool_calls][tool]        → 正常
//	[sys][摘要][user][assistant+tool_calls][tool]  → 正常  ← 补一条 user 就好了
//
// 中途还排除了一个假线索：一开始用手写的 `tool_call.id`（"c9"）做探测，任何形状都
// 报同一个 400，看起来像"续跑工具调用必须带 reasoning_content"。换成供应商自己
// 签发的 id（`call_00_…`）之后就正常了——**供应商在 id 里编了信息**，认得出自己
// 签发的调用。合成的 id 会让所有探测结论失效，这一点值得记住。
//
// # 什么时候会走到这一步
//
// 游标落在一条 assistant 消息之前。正常路径不会：游标只会被推进到轮次边界，
// 而轮次以用户消息开头。只有压缩的**终结性全塌**会把游标推到当时的历史末尾，
// 之后这一轮继续追加 assistant 与 tool——接缝就落在了 assistant 上。
//
// # 为什么补的是"当前轮的用户消息"
//
// 因为那正是模型此刻要回答的东西。全塌之后摘要里确实写了"用户要的是什么"，但那是
// 概括；模型需要一条真正的 user 消息来知道"现在轮到我了"。
//
// 这和用户原话派生化是同一个思路：**记忆覆盖全部历史，视图按需从不可变的原文里
// 派生出模型必须看到的部分**。补出来的这一条不进存储、不改游标、不影响覆盖不变量。
func bridgeSummaryToRaw(
	active []domain.MemoryBatch,
	history []domain.Message,
	cursor int,
) []domain.ModelMessage {
	// 没有摘要就没有接缝——视图第一条就是原文，不构成连续 assistant。
	if len(active) == 0 {
		return nil
	}
	cursor = min(max(cursor, 0), len(history))
	// 接缝之后是用户消息，天然不构成连续 assistant，什么都不用做。
	if cursor < len(history) && history[cursor].Role == domain.RoleUser {
		return nil
	}
	// 接缝之后是一条 tool 消息：说明它的调用还在游标之内，那是悬空调用，
	// 覆盖不变量本该拦住。这里不越权修补，交给 CheckInvariant 报出来。
	if cursor < len(history) && history[cursor].Role == domain.RoleTool {
		return nil
	}

	// 找出当前这一轮（包含游标那条消息的那一轮；游标到末尾时就是最后一轮），
	// 把它游标之内的用户消息按原顺序补出来。一轮里可能有多条——运行中注入的
	// 消息共享同一个 TurnID。
	turns := splitTurns(history)
	if len(turns) == 0 {
		return nil
	}
	current := turns[len(turns)-1]
	for _, turn := range turns {
		if cursor >= turn.Start && cursor < turn.End {
			current = turn
			break
		}
	}

	var bridge []domain.ModelMessage
	for index := current.Start; index < min(current.End, cursor); index++ {
		if history[index].Role != domain.RoleUser {
			continue
		}
		bridge = append(bridge, domain.ModelMessage{
			Role:    domain.ModelRoleUser,
			Content: history[index].Content,
		})
	}
	return bridge
}
