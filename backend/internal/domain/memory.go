package domain

import (
	"fmt"
	"time"
)

// 会话记忆：把较早的原始消息压缩成摘要，让有限的上下文窗口装得下更长的对话。
//
// # 三个概念不能混（继承设计文档 2.3）
//
//   - **完整会话**：所有已保存的消息，跨进程保存，**永不因压缩被删除**；
//   - **模型上下文视图**：一次调用真正发出去的东西；
//   - **会话记忆**：本文件——摘要节点的仓库，加上"当前哪些摘要在生效"。
//
// 压缩改变的只有第二样。原始消息一条都不动，模型需要原话时用 conversation_history
// 工具沿摘要树逐层回查。这是行为底线第 10 条。
//
// # 为什么是分层合并，不是一份滚动摘要
//
// 滚动摘要（把"旧摘要 + 新消息"重新总结成一份新摘要）每触发一次就要重写全部旧
// 记忆，早期事实反复经过模型改写，失真会一层层累积。分层合并里每个节点只被生成
// 一次，之后只作为父节点的输入被引用一次，而且原节点永久保留、可以回查。
//
// 四个叶子的典型演化：B1+B2→P12，B3+B4→P34，P12+P34→P1234。此时仓库里七个节点
// 都在，但真正进入上下文的只有 P1234 一个。

// MemoryBatchID 是一个摘要节点的 ID，形如 mem_ 加 32 位小写十六进制。
//
// 它会出现在发给模型的摘要标签里，模型据此调用 conversation_history 回查，
// 因此必须是模型能原样复述的短标识，而不是一串结构化描述。
type MemoryBatchID string

// NewMemoryBatchID 生成一个新的摘要节点 ID。
func NewMemoryBatchID() (MemoryBatchID, error) {
	text, err := newRandomID(memoryBatchIDPrefix)
	return MemoryBatchID(text), err
}

// Validate 校验摘要节点 ID 的格式。
//
// 它是外部输入：模型会在 conversation_history 的参数里给出一个 batch_id，
// 而模型完全可能编一个出来。校验之后再去查仓库，错的 ID 得到的是一条明确的
// error 观察，而不是一次莫名其妙的空结果。
func (id MemoryBatchID) Validate() error {
	return validateRandomID(string(id), memoryBatchIDPrefix)
}

// MemoryBatch 是一个不可变的摘要节点。
//
// "不可变"是这套结构的基础：节点一旦生成就不再修改，合并只产生新的父节点，
// 子节点原样保留。因此任何时刻都能沿 source_batch_ids 往下走到原始消息，
// 不存在"这个摘要被改过、和它当初覆盖的消息对不上了"的情况。
type MemoryBatch struct {
	// ID 是这个节点的唯一标识。
	ID MemoryBatchID
	// Level 是层级：直接由原始消息生成的叶子为 0，父节点是子节点最大层级加一。
	Level int
	// Content 是模型生成的摘要正文。
	Content string
	// StartMessageIndex 与 EndMessageIndex 是它覆盖的原始消息范围，**右开区间**
	// [start, end)，下标对应 Session.Messages() 的位置。
	//
	// 用右开是为了让相邻区间可以直接首尾相接：[0,10) 和 [10,20) 拼起来正好是
	// [0,20)，不需要到处 +1/-1。
	StartMessageIndex int
	EndMessageIndex   int
	// EditedContent 是人工修订过的正文。空表示没改过。
	//
	// # 为什么加这一列，而不是直接改 Content
	//
	// 上面那句"不可变"是这套结构的地基：它保证任何时刻都能沿 SourceBatchIDs 走到
	// 原始消息，不存在"这个摘要被改过、和它当初覆盖的消息对不上"的情况。直接改
	// Content 就把这条保证拆了——父节点是从子节点**当时的**正文合并出来的，改了
	// 子节点，父子之间的推导关系就断了，而且再也无从知道模型当初写的是什么。
	//
	// 所以把两件事分开：Content 是**模型当初生成了什么**，写入之后永不修改；
	// EditedContent 是**现在拿哪一版进上下文**。不可变的是事实，可变的是取用。
	// 这和"完整会话（事实，永不删）/ 模型上下文视图（临时组装）"是同一个思路。
	EditedContent string
	// SourceBatchIDs 是直接子节点。叶子为空；父节点记录被它合并的那几个。
	//
	// 只记直接子节点，不记整棵子树：conversation_history 的 inspect 一次只展开
	// 一层，避免模型一口气把整棵树拉回上下文——那样压缩就白做了。
	SourceBatchIDs []MemoryBatchID
	// CreatedAt 是生成时间。
	CreatedAt time.Time
}

// EffectiveContent 返回当前真正生效的正文：有修订用修订，没有用原文。
//
// 凡是"这段摘要现在说什么"的地方都要走它——进上下文的渲染、合并时作为父节点的
// 输入、界面上的展示。直接读 Content 的地方只剩一处：界面上"对照模型原始版本"。
func (batch MemoryBatch) EffectiveContent() string {
	if batch.EditedContent != "" {
		return batch.EditedContent
	}
	return batch.Content
}

// Edited 表示这个节点被人工修订过。
func (batch MemoryBatch) Edited() bool {
	return batch.EditedContent != ""
}

// Covers 返回这个节点覆盖的消息条数。
func (batch MemoryBatch) Covers() int {
	return batch.EndMessageIndex - batch.StartMessageIndex
}

// Title 返回摘要的首个非空行，用作节点的简述。
//
// 压缩 prompt 要求模型第一行写 Topic，因此首行天然就是一句概括。不额外存一个
// title 字段：那会和正文产生两份可能互相矛盾的说法，而正文才是权威。
func (batch MemoryBatch) Title() string {
	for line := range splitLines(batch.EffectiveContent()) {
		if trimmed := trimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return "（空摘要）"
}

// ConversationMemory 是一个会话的全部摘要节点，以及当前生效的那一层。
type ConversationMemory struct {
	// RawCompactionCursor 是第一条**尚未**进入任何叶子摘要的原始消息下标。
	//
	// 也就是说 [0, cursor) 已经被摘要覆盖，[cursor, len) 仍以原文进入上下文。
	// 它只在叶子摘要成功保存之后才前进——合并父节点不改变它，因为合并没有覆盖
	// 新的原始消息。
	RawCompactionCursor int
	// QuotesCollapsedBefore 是第二条水位线：[0, before) 的用户原话已经被并进摘要，
	// 渲染时不再逐条列出。0 表示全部逐条列出（绝大多数会话一直是 0）。
	//
	// # 为什么用户原话要单独有一条水位线
	//
	// 用户说过的话是历史里**唯一无法重新推导**的东西：模型的输出可以重新生成，
	// 工具输出可以重新执行，用户的意图不能。而实测它几乎免费——一段 82766 token
	// 的真实会话里，用户消息只有 328 token，占 0.4%。所以默认全部逐条渲染，
	// 由代码在渲染时从原始消息里派生，不进任何摘要的存储。
	//
	// 但"几乎免费"不是"永远免费"：用户原话的总量单调增长（消息只增不删），
	// 极端情况下它自己就能撑爆目标线。那时才由压缩的第 3 步把一段原话交给模型
	// 整理成一段话，并把这条水位线推到那一段的终点。
	//
	// # 为什么是水位线，而不是每次渲染按预算裁一部分
	//
	// 单调增长的量，降级也应当是单调的。每次渲染重算的裁剪会让同一段内容在
	// "在上下文 / 不在上下文"之间来回摆动，而它其实永远回不来了——摆动本身
	// 就是在骗人。
	//
	// 两条水位线含义完全平行，只是覆盖的东西不同（一条覆盖原文，一条覆盖原话），
	// **都只前进不后退**，都由压缩推动。
	QuotesCollapsedBefore int
	// CollapsedQuotes 是被并掉的那段用户原话的整理结果，由模型写。
	//
	// 它和 QuotesCollapsedBefore 成对出现：水位线说"到哪为止不再逐条列"，
	// 这一段说"那些话的意思是什么"。空串且水位线为 0 是常态。
	//
	// 不放进某个 MemoryBatch.Content：那些节点是不可变的，而这段整理结果会随着
	// 水位线继续前进而被重写（下一次整理要把新并进来的原话也算上）。放进不可变
	// 的节点里，要么破坏不可变，要么每次整理都要新建一个节点。
	CollapsedQuotes string
	// Batches 是全部节点，只增不删。合并之后子节点仍然留在这里供回查。
	Batches []MemoryBatch
	// ActiveBatchIDs 是当前真正进入上下文的摘要前沿，按覆盖时间排序。
	//
	// 它是"树的一个横切面"而不是树根：四个叶子刚合并成两个父节点时，
	// 前沿是 [P12, P34] 两个。
	ActiveBatchIDs []MemoryBatchID
}

// IsActive 判断一个节点是否在当前生效的前沿上。
//
// 编辑只允许改活跃节点：改一个已经被合并进上层的节点，模型看到的东西一个字都不会
// 变——那会让人以为自己改了什么，实际上什么都没发生。
func (memory ConversationMemory) IsActive(id MemoryBatchID) bool {
	for _, active := range memory.ActiveBatchIDs {
		if active == id {
			return true
		}
	}
	return false
}

// EditBatch 返回一份把指定节点的正文换成 content 的新记忆。
//
// 它不修改接收者：和这个包里其他地方一样，"改"一律是构造新值。节点本身也只是
// 换掉 EditedContent 那一个字段，Content、覆盖范围、子节点、层级全都原样保留。
//
// content 为空串表示**撤销修订**，回到模型原始生成的那一版。
func (memory ConversationMemory) EditBatch(id MemoryBatchID, content string) (ConversationMemory, error) {
	// 这里用普通错误而不是 invariantError：**没有任何不变量被破坏**。
	// invariantError 的含义是"程序自己算错了，出现一次就说明有 bug"，而这只是
	// 一次没有意义的请求——改一个已经被合并进上层的节点，模型看到的东西一个字
	// 都不会变。把它报成不变量破坏，会让日志里真正的 bug 淹没在噪音里。

	//只允许最高层级的摘要被修改，因为最高层级本身就来源于低层级
	//有效摘要为最高层级，因此对最低层级修改毫无意义，因此在第一道关直接卡住
	if !memory.IsActive(id) {
		return memory, fmt.Errorf(
			"摘要节点 %s 不在当前生效的前沿上，改它不会影响模型看到的内容", id)
	}
	// 创建一个副本
	batches := make([]MemoryBatch, len(memory.Batches))
	copy(batches, memory.Batches)
	found := false
	for index := range batches {
		// 原摘要id跟要修改的id对上了，那么原来的editedcontent就被替换掉
		if batches[index].ID == id {
			batches[index].EditedContent = content
			found = true
			break
		}
	}
	// 这一条相反：ID 在活跃前沿里却在仓库里找不到，那确实是不变量被破坏了
	// （CheckInvariant 检查的第一条就是它），说明数据被外部改过或者哪里写错了。
	if !found {
		return memory, invariantError("活跃前沿引用了不存在的节点 %s", id)
	}
	//这次返回的，就是摘要的edit content被替换后的内容
	//
	// 两条水位线和 CollapsedQuotes 原样带过来。漏掉它们的后果很隐蔽：用户在面板上
	// 改一段摘要，改完之后那些本已并掉的用户原话又整段冒回上下文里，占用凭空上涨。
	return ConversationMemory{
		RawCompactionCursor:   memory.RawCompactionCursor,
		QuotesCollapsedBefore: memory.QuotesCollapsedBefore,
		CollapsedQuotes:       memory.CollapsedQuotes,
		Batches:               batches,
		ActiveBatchIDs:        append([]MemoryBatchID(nil), memory.ActiveBatchIDs...),
	}, nil
}

// Batch 按 ID 取出一个节点。
func (memory ConversationMemory) Batch(id MemoryBatchID) (MemoryBatch, bool) {
	for _, batch := range memory.Batches {
		if batch.ID == id {
			return batch, true
		}
	}
	return MemoryBatch{}, false
}

// ActiveBatches 按顺序返回当前生效的摘要节点。
//
// 跳过找不到的 ID：那意味着数据被外部改过，此时少显示一段摘要，比让整个会话
// 打不开要好——原始消息还在，模型仍然能回答，只是少一段背景。
func (memory ConversationMemory) ActiveBatches() []MemoryBatch {
	active := make([]MemoryBatch, 0, len(memory.ActiveBatchIDs))
	for _, id := range memory.ActiveBatchIDs {
		if batch, ok := memory.Batch(id); ok {
			active = append(active, batch)
		}
	}
	return active
}

// CheckInvariant 校验"活跃前沿恰好无缝覆盖 [0, cursor)"。
//
// 这是整套记忆结构唯一的不变量，也是它正确性的全部：活跃摘要加上游标之后的原文，
// 必须正好等于完整历史的一个无缝分割。少一段是丢历史，多一段是同一段内容进了
// 两次上下文。
//
// 它在每次压缩改动之后被调用——压缩涉及"生成节点、推进游标、替换前沿"好几步，
// 任何一步写错都会破坏这个等式，而症状（模型忘了某段对话）会在很久以后才显现。
func (memory ConversationMemory) CheckInvariant() error {
	expected := 0
	// 区分一点
	// memory包含batches，即当前所有的摘要
	// ActiveBatches 则是当前实际上参与构建模型上下文的摘要，因为有的摘要已经被合并了。
	//该for用于检查区间正确性
	for _, id := range memory.ActiveBatchIDs {
		//按id查出一个batch
		batch, ok := memory.Batch(id)
		if !ok {
			return invariantError("活跃前沿引用了不存在的节点 %s", id)
		}
		//循环最开始的expected一定是0，毋庸置疑
		if batch.StartMessageIndex != expected {
			return invariantError("节点 %s 覆盖 [%d,%d)，但前面的部分只到 %d——中间有空洞或重叠",
				id, batch.StartMessageIndex, batch.EndMessageIndex, expected)
		}
		expected = batch.EndMessageIndex
	}
	//来到最后，expected值必须为压缩游标，小于则说明漏压缩，大于说明多压缩。
	if expected != memory.RawCompactionCursor {
		return invariantError("活跃前沿覆盖到 %d，而游标在 %d——两者必须相等",
			expected, memory.RawCompactionCursor)
	}
	return nil
}
