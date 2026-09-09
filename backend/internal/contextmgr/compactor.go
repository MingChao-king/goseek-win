package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"goseek/internal/domain"
)

// Compactor 执行一次上下文压缩。
//
// 它不碰会话历史，也不落库：输入是"当前历史 + 当前记忆 + 预算"，输出是"新的记忆
// 状态 + 新生成的节点"。持久化和事件由调用方（Agent）负责——那样压缩本身可以
// 完全用内存里的假 Summarizer 测试，不需要数据库。
type Compactor struct {
	summarizer Summarizer
	thresholds Thresholds
	tools      []domain.ToolSpec
	// now 提供节点的创建时间，测试用固定时钟替换。
	now func() time.Time
	// onBatch 在每个节点生成后被调用，让调用方能把进度报给界面。
	//
	// 压缩可能要好几次模型调用、几十秒，全程没有反馈的话用户只会看到界面卡住。
	onBatch func(domain.MemoryBatch)
	// safetyFactor 是估算用的安全系数。
	//
	// 必须和 Agent 真正发请求时用的是同一个值——压缩时算的和发请求时算的对不上，
	// 是这类逻辑最常见的错误来源（同 estimate 的注释）。
	safetyFactor float64
}

// SetSafetyFactor 指定估算用的安全系数。零值或负值表示沿用默认。
func (compactor *Compactor) SetSafetyFactor(factor float64) {
	if factor > 0 {
		compactor.safetyFactor = factor
	}
}

// NewCompactor 构造一个压缩器。onBatch 可以为 nil。
func NewCompactor(
	summarizer Summarizer,
	thresholds Thresholds,
	tools []domain.ToolSpec,
	onBatch func(domain.MemoryBatch),
) *Compactor {
	if onBatch == nil {
		onBatch = func(domain.MemoryBatch) {}
	}
	return &Compactor{
		summarizer:   summarizer,
		thresholds:   thresholds,
		tools:        tools,
		now:          time.Now,
		onBatch:      onBatch,
		safetyFactor: domain.DefaultSafetyFactor,
	}
}

// Compact 把上下文压到目标线以下，返回新的记忆状态。
//
// # 至多三步
//
//	第 1 步  生成叶子      把「游标 → 保留区起点」之间的全部原文压成一个叶子
//	         ── 低于目标线 → 结束 ──
//	第 2 步  塌缩前沿      把除最新叶子之外的全部活跃节点一次塌成一个父节点
//	         ── 低于目标线 → 结束（绝大多数情况到此为止）──
//	第 3 步  收尾诊断      两步都没压到，说明问题不在原文。判断是谁撑爆的再动手
//
// # 为什么是"至多三步"而不是原来的成对合并循环
//
// 成对合并把 K 个活跃摘要收成 1 个需要**恰好 K−1 次**模型调用，与合并顺序无关。
// 实测：K=20 时撞上 12 步的上限，前沿还剩 8 段；K=40 时剩 28 段。那个"第二道保险"
// 实际上是一条会被撞到的成本闸门。
//
// 一次塌完不仅更省调用，**保真度也更高**——分层合并是复利压缩：6 个 300 字的叶子
// 成对合并三层后剩 111 字、过了 3 遍模型改写；一次塌完是 1800→600 字、过 1 遍。
// 五倍的篇幅差，而且少走两遍改写。
//
// # 每一步都要验算
//
// 每一步都必须让上下文真的变小，否则整步丢掉（acceptIfSmaller）。摘要是模型写的，
// 没人能保证它比原文短——一次真实运行里，模型把一轮只有两条消息的对话总结成了更长
// 的一段，上下文从 4828 涨到 4919。这道验算同时是**收敛条件**的实现。
//
// # 每次都重新完整计数
//
// 而不是用"旧值减去被压掉的、加上新摘要的"去推算：推算会累积误差，而且一旦某一步
// 的估算写错，后面全错且很难发现。完整计数的代价只是遍历一遍消息，相比一次模型调用
// 可以忽略。
func (compactor *Compactor) Compact(
	ctx context.Context,
	session *domain.Session,
	memory domain.ConversationMemory,
) (CompactionResult, error) {
	//获取会话中所有消息
	messages := session.Messages()
	//按轮次切分
	turns := splitTurns(messages)
	//最初上下文占用
	before := compactor.estimate(messages, memory)
	//保留一个原始情况，万一压缩失败还能返回这个值
	result := CompactionResult{Memory: memory, BeforeTokens: before, AfterTokens: before}

	// calls 记录已经花掉的模型调用次数。maxCompactionSteps 是保险丝，正常路径
	// 用不到它：第 1、2 步各一次，第 3 步至多两次，正好是它的值。
	calls := 0

	// 第 1 步：把游标到保留区之间的原文压成一个叶子。
	//
	// 先做这一步而不是先合并摘要：把原文换成摘要的收益远大于把两个摘要合并成一个
	// ——原文里 84% 是工具观察，而摘要已经是压过一轮的东西了。
	if err := compactor.step(ctx, &result, messages, &calls,
		compactor.makeLeafStep(messages, turns)); err != nil {
		return result, err
	}
	if result.AfterTokens < compactor.thresholds.CompactTo {
		return result, nil
	}

	// 第 2 步：把活跃前沿（除最新叶子外）一次塌成一个父节点。
	// 一般来说在此终止，即变为一个合并摘要+叶子+两轮对话的形式
	// 终止阈值为20%
	if err := compactor.step(ctx, &result, messages, &calls,
		compactor.collapseStep()); err != nil {
		return result, err
	}
	if result.AfterTokens < compactor.thresholds.CompactTo {
		return result, nil
	}

	// # 第 3 步：终结性兜底。判据是**触发线**，不是硬边界
	//
	// 判据必须留出余量，理由很硬：**第 3 步要把保留区交给模型总结，而摘要请求的
	// 输入就是那段保留区**。上下文卡在硬边界时，那段源文本自己就接近整个窗口，
	// 摘要请求根本发不出去——**救援动作需要余量，而硬边界的定义就是没有余量。**
	//
	// 上一版按硬边界触发，只算了"降级是永久的所以要晚点触发"这一面，漏了救援本身
	// 的成本。实测把这个错误暴露得很清楚：一次压缩在 147763 触发（超硬边界 26k），
	// 压完停在 120637——距硬边界只剩 963 token，而第 3 步因为 120637 < 121600
	// 没有触发。它靠下一轮压缩侥幸自我纠正了，但那 963 是运气不是设计。
	//
	// 用触发线的含义是：**前两步等于什么都没做**（摘要 + 保留区自己就占到 80%），
	// 那是病态状态，必须一次解决。而"到了目标线和触发线之间"仍然是 TargetUnreachable
	// ——可接受的中间状态，下一轮继续。
	//
	// # 为什么它是终结性的，不循环
	//
	// 走到这里意味着这个会话本身已经超出了正常规模。兜底做的是**一次全塌**：把既有
	// 摘要、全部用户原话、以及最后那几轮的全部消息，一起交给模型总结成**一个**摘要。
	// 之后上下文里只剩 system + 工具定义 + 那一个摘要，没有任何原文。
	//
	// 这样它一次就到位，不需要循环判断"够不够小"；而循环恰恰是危险的——每多转一圈
	// 就多一次摘要请求，而那时上下文已经很紧了。

	// 如果结果还大于压缩触发线，可以认为要么是过大的用户原文，要么是产生了超大的两轮对话
	// 必须进行最终压缩
	if result.AfterTokens >= compactor.thresholds.CompactAt {
		if err := compactor.step(ctx, &result, messages, &calls,
			compactor.finalCollapseStep(messages, &result)); err != nil {
			return result, err
		}
	}

	//最终兜底，发生可能性仅出现于，摘要本身超过上下文窗口
	// 到这里仍没到目标线就是 TargetUnreachable —— 它**不是失败**：只要低于硬边界，
	// 这一轮照常请求，下一轮压缩会再来一次。语义见 CompactionResult。
	if result.AfterTokens >= compactor.thresholds.CompactTo {
		result.TargetUnreachable = true
		if result.Reason == "" {
			result.Reason = "已经压到只剩摘要和最近两轮原文，再压就会丢掉当前正在做的事；下一轮会继续"
		}
	}
	return result, nil
}

// compactionStep 是一步压缩：产出一个候选记忆，或者说明自己无事可做。
//
// 三步都长成这个样子，于是"调用 → 验算是否更小 → 接受或丢弃 → 校验不变量"这段
// 纪律只写一遍（见 step）。漏掉其中任何一环的后果都很隐蔽：不验算会让上下文越压
// 越大，不校验不变量会让某段历史悄悄消失，而症状要到很久以后才显现。
type compactionStep func(ctx context.Context, memory domain.ConversationMemory) (
	candidate domain.ConversationMemory, batches []domain.MemoryBatch, ok bool, err error)

// step 跑一步压缩，验算之后决定接受还是丢弃。
//
// ok=false（这一步无事可做）不是错误：三步流程里每一步都可能不适用——没有可压的
// 原文、前沿上只有一个节点、诊断不出病因。此时原样返回，交给下一步。
func (compactor *Compactor) step(
	ctx context.Context,
	result *CompactionResult,
	messages []domain.Message,
	calls *int,
	run compactionStep,
) error {
	if *calls >= maxCompactionSteps {
		return nil
	}
	current := compactor.estimate(messages, result.Memory)
	candidate, batches, ok, err := run(ctx, result.Memory)
	if err != nil {
		return err
	}
	if !ok {
		// 这一步无事可做，没有花掉调用。ok=false 不是错误：三步流程里每一步都可能
		// 不适用——没有可压的原文、前沿上只有一个节点、诊断不出病因。
		return nil
	}
	// 走到这里说明模型确实被调用了一次。**不管候选最后被不被接受都要计数**：
	// 花出去的额度不会因为结果被丢弃而退回来。
	*calls++

	next, accepted := compactor.acceptIfSmaller(messages, candidate, current)
	if !accepted {
		// 这一步整个丢掉，不"先用着下次再说"：记忆是只增不删的，一旦写进去就再也
		// 去不掉了，让一个负收益的节点永久留在上下文里比白花一次调用更糟。
		return nil
	}

	result.Memory = next
	result.NewBatches = append(result.NewBatches, batches...)
	result.AfterTokens = compactor.estimate(messages, next)
	for _, batch := range batches {
		compactor.onBatch(batch)
	}
	// 校验"活跃前沿恰好无缝覆盖 [0, cursor)"。压缩涉及生成节点、推进游标、替换
	// 前沿好几步，任何一步写错都会破坏这个等式，而症状（模型忘了某段对话）会在
	// 很久以后才显现。
	return result.Memory.CheckInvariant()
}

// makeLeafStep 是第 1 步：把游标到保留区之间的完整轮次压成一个叶子。
func (compactor *Compactor) makeLeafStep(
	messages []domain.Message,
	//一个轮次在历史里的位置，右开区间 [Start, End)
	turns []turnBoundary,
) compactionStep {
	return func(ctx context.Context, memory domain.ConversationMemory) (
		domain.ConversationMemory, []domain.MemoryBatch, bool, error) {
		start, end, ok := compactableRange(turns, memory.RawCompactionCursor, retainedTurns)
		if !ok {
			// 游标已经追上保留区：没有原文可压。
			return domain.ConversationMemory{}, nil, false, nil
		}
		batch, err := compactor.summarizeRange(ctx, messages, start, end)
		if err != nil {
			return domain.ConversationMemory{}, nil, false, err
		}
		return appendLeaf(memory, batch), []domain.MemoryBatch{batch}, true, nil
	}
}

// collapseStep 是第 2 步：把活跃前沿一次塌成一个父节点。
//
// # 为什么保留最新的那个叶子
//
// 全塌会让刚生成的叶子立刻被二次压缩（同一段内容过两遍模型），而它恰恰是模型下一轮
// 最可能回头看的内容。留一个的代价是零——仍然一次调用，前沿从 1 段变 2 段，
// 之后长期稳定在 [根, 最新叶子]。
func (compactor *Compactor) collapseStep() compactionStep {
	return func(ctx context.Context, memory domain.ConversationMemory) (
		domain.ConversationMemory, []domain.MemoryBatch, bool, error) {
		//获取有效摘要
		active := memory.ActiveBatches()
		// 要塌的是"除最新叶子之外的全部"，因此至少要有三个节点才划算：
		// 两个的话塌完还是两个（一个父 + 最新叶子），白花一次调用。
		// 很显然的道理，走到这里说明生成了一个叶子节点
		// 由于塌缩逻辑为对之前的所有摘要进行一个统一合并压缩，所以如果之前的摘要小于两个
		// 就意味着一个压缩完了之后还是一个，没有压缩的意义
		// 但是不继续压缩不代表就这样不继续进行了，而是到第三步去判断上下文大小决定是否进行终态压缩。
		if len(active) < 3 {
			return domain.ConversationMemory{}, nil, false, nil
		}
		//排除掉叶子节点
		collapsing := active[:len(active)-1]
		// 把之前的所有摘要进行一个统一合并
		content, err := summarizeBatches(ctx, compactor.summarizer, collapsing)
		if err != nil {
			return domain.ConversationMemory{}, nil, false, fmt.Errorf("合并摘要失败: %w", err)
		}
		if content == "" {
			return domain.ConversationMemory{}, nil, false, errors.New("模型返回了空摘要")
		}
		id, err := domain.NewMemoryBatchID()
		if err != nil {
			return domain.ConversationMemory{}, nil, false, err
		}

		parent := domain.MemoryBatch{
			ID: id,
			// 层级是被塌掉的那些里最高的加一。它只用于展示和调试——新算法不再按
			// 层级选合并对（pickMergePair 已删除），树的形状由流程本身决定。
			Level:   highestLevel(collapsing) + 1,
			Content: content,
			// 覆盖范围是那一串连续区间的并集。它们首尾相接（不变量保证），
			// 所以直接取首尾。
			StartMessageIndex: collapsing[0].StartMessageIndex,
			EndMessageIndex:   collapsing[len(collapsing)-1].EndMessageIndex,
			// 记录子节点，即摘要的子摘要
			SourceBatchIDs: idsOf(collapsing),
			CreatedAt:      compactor.now(),
		}
		//传入的count即为塌缩的节点个数
		return collapseFrontier(memory, len(collapsing), parent),
			[]domain.MemoryBatch{parent}, true, nil
	}
}

// finalCollapseStep 是第 3 步：**终结性兜底**，一次全塌。
//
// # 它做什么
//
// 把三样东西一起交给模型，总结成**一个**摘要：
//
//	既有的活跃摘要        —— 之前压缩的成果
//	全部用户原话          —— 明确要求模型对"用户在这个会话里要的是什么"单独成段
//	游标之后的全部原文     —— 包括最后那两轮，它们不再被保留
//
// 之后上下文里只剩 system + 工具定义 + 这一个摘要，游标推到历史末尾。
//
// # 为什么最后两轮也压
//
// "保留最近两个完整轮次"是让模型好用的经验值，不是硬要求。走到这一步说明保留区
// 本身就是撑爆上下文的主因——继续保留它就等于什么都没做。用户那句话不会因此丢失：
// 指令里明确要求把用户的诉求单独总结成一段，而原始消息一条没删，`search` 随时能查。
//
// # 协议安全
//
// 全塌之后视图里没有任何原文，因此不存在悬空的工具调用——一次调用和它的观察要么
// 都在摘要里，要么都不在。这比在中间找配对边界更简单，也更不容易出错。
func (compactor *Compactor) finalCollapseStep(
	messages []domain.Message,
	result *CompactionResult,
) compactionStep {
	return func(ctx context.Context, memory domain.ConversationMemory) (
		domain.ConversationMemory, []domain.MemoryBatch, bool, error) {
		if len(messages) == 0 {
			return domain.ConversationMemory{}, nil, false, nil
		}
		active := memory.ActiveBatches()
		// 已经是"一个摘要 + 没有原文"了，再塌一次什么也压不掉。
		if len(active) <= 1 && memory.RawCompactionCursor >= len(messages) {
			return domain.ConversationMemory{}, nil, false, nil
		}

		turns := splitTurns(messages)
		//得到所有的用户消息，每条用户消息被标注所属轮次的起点和终点
		quotes := deriveQuotes(messages, turns, 0, len(messages))
		// 保留区的起点一并传进去：源文本里的超大观察要按视图口径收边界，
		// 而标注里的序号必须是它在完整历史里的真实位置，模型才取得回来。
		retainedFrom := min(memory.RawCompactionCursor, len(messages))
		//一次压缩中的最终压缩
		content, err := summarizeEverything(ctx, compactor.summarizer,
			active, memory.CollapsedQuotes, quotes, messages[retainedFrom:], retainedFrom)
		if err != nil {
			return domain.ConversationMemory{}, nil, false, fmt.Errorf("终结性压缩失败: %w", err)
		}
		if content == "" {
			return domain.ConversationMemory{}, nil, false, errors.New("模型返回了空的终结性摘要")
		}
		id, err := domain.NewMemoryBatchID()
		if err != nil {
			return domain.ConversationMemory{}, nil, false, err
		}

		batch := domain.MemoryBatch{
			ID: id,
			// 层级要和 MemoryBatch.Level 的定义对上：**直接由原始消息生成的叶子为 0**，
			// 父节点才是子节点最大层级加一。
			//
			// 前沿为空时这次全塌没有吸收任何摘要，它总结的就是原始消息——那是一个
			// 叶子，层级 0。写成 highestLevel(nil)+1 会得到 1，于是库里出现一个
			// "L1 但没有子节点"的节点，面板上展开 L2 → L1 之后就到底了，看起来像
			// 树断了一层。这是真机跑出来被看出来的。
			Level:             collapsedLevel(active),
			Content:           content,
			StartMessageIndex: 0,
			EndMessageIndex:   len(messages),
			SourceBatchIDs:    idsOf(active),
			CreatedAt:         compactor.now(),
		}

		next := memory
		next.RawCompactionCursor = len(messages)
		// 用户原话已经写进这个摘要了，水位线推到末尾、整理结果清空——
		// 再渲染一份逐条列表就是同一批内容进两次上下文。
		next.QuotesCollapsedBefore = len(messages)
		next.CollapsedQuotes = ""
		next.Batches = append(append([]domain.MemoryBatch(nil), memory.Batches...), batch)
		next.ActiveBatchIDs = []domain.MemoryBatchID{batch.ID}

		result.Reason = fmt.Sprintf(
			"前两步之后仍占 %d tokens（触发线 %d），已把全部历史（含最近两轮原文与用户原话）"+
				"塌成一个摘要。这不是正常工作状态：这个会话该收尾了，或者窗口配小了。",
			result.AfterTokens, compactor.thresholds.CompactAt)
		return next, []domain.MemoryBatch{batch}, true, nil
	}
}

// summarizeRange 把 [start, end) 的原文压成一个叶子节点。
func (compactor *Compactor) summarizeRange(
	ctx context.Context,
	messages []domain.Message,
	start, end int,
) (domain.MemoryBatch, error) {
	// start 是这段原文在完整历史里的起点，用于超大观察标注里的序号。
	content, err := summarizeMessages(ctx, compactor.summarizer, messages[start:end], start)
	if err != nil {
		return domain.MemoryBatch{}, fmt.Errorf("生成摘要失败: %w", err)
	}
	if content == "" {
		return domain.MemoryBatch{}, errors.New("模型返回了空摘要")
	}
	id, err := domain.NewMemoryBatchID()
	if err != nil {
		return domain.MemoryBatch{}, err
	}
	return domain.MemoryBatch{
		ID:                id,
		Level:             0,
		Content:           content,
		StartMessageIndex: start,
		EndMessageIndex:   end,
		CreatedAt:         compactor.now(),
	}, nil
}

// acceptIfSmaller 只在候选记忆确实让上下文变小时才接受它。
//
// # 为什么每一步都要验一次
//
// 摘要是模型写的，没人能保证它比原文短。一次真实运行里，模型把一轮只有两条消息
// 的对话总结成了更长的一段，上下文从 4828 涨到 4919——压缩产生了负收益，还白花
// 了一次调用。指令里现在有篇幅要求（见 summarize.go），但那是"请求模型配合"，
// 不是保证；真正的保证只能是这里的这道验算。
//
// 这同时也是收敛条件的实现：每个被接受的步骤都让活跃 token 严格减少。
//
// 被拒绝的候选整个丢掉，而不是"先用着下次再说"：记忆是只增不删的，一旦写进去
// 就再也去不掉了，让一个负收益的节点永久留在上下文里比白花一次调用更糟。
func (compactor *Compactor) acceptIfSmaller(
	messages []domain.Message,
	candidate domain.ConversationMemory,
	current int,
) (domain.ConversationMemory, bool) {
	if compactor.estimate(messages, candidate) >= current {
		return domain.ConversationMemory{}, false
	}
	return candidate, true
}

// estimate 完整估算"system + 活跃摘要 + 游标之后的原文 + 工具定义"的输入量。
//
// 它和真正发请求时走的是同一条路（buildMessages），因此这里算出来的数字就是
// 那时会看到的数字——两处用不同的方式计数，是压缩类逻辑最常见的错误来源。
func (compactor *Compactor) estimate(messages []domain.Message, memory domain.ConversationMemory) int {
	view := buildMessages(messages, memory, nil)
	return domain.EstimateRequestTokensWith(
		domain.ModelRequest{Messages: view, Tools: compactor.tools}, compactor.safetyFactor)
}

// lastPairBoundary 返回 [cursor, len) 里最后一个合法切点。
//
// 合法切点的定义：切点之前的每一次工具调用，都能在切点之前找到配对的观察。
// 这样切开之后，前半截（进摘要）和后半截（留原文）都不含悬空调用。
//
// 返回的是**最后一个**合法切点而不是第一个：切得越靠后，并进摘要的原文越多、
// 这一步的收益越大。而这一步之所以存在，就是因为前两步的收益已经不够了。
//
// 切点不能等于 cursor（那等于什么都没切），也不能等于 len(messages)（那会把
// 当前用户消息也压掉，模型就不知道自己在回答什么了）。
func lastPairBoundary(messages []domain.Message, cursor int) (int, bool) {
	// pending 记录已经发出、还没等到观察的调用 id。为空时说明此刻是一个合法切点。
	pending := make(map[string]bool)
	best := 0
	found := false

	for index := cursor; index < len(messages); index++ {
		message := messages[index]
		if message.ToolCallID != "" {
			delete(pending, message.ToolCallID)
		}
		for _, call := range message.ToolCalls {
			pending[call.ID] = true
		}
		// index+1 是"读完这条之后"的位置。此刻没有悬空调用就是一个合法切点。
		//
		// 排除最后一条：切在末尾等于把整个保留区都压掉，包括用户刚提的问题。
		if len(pending) == 0 && index+1 > cursor && index+1 < len(messages) {
			best = index + 1
			found = true
		}
	}
	return best, found
}

// collapsedLevel 返回一次全塌产生的节点该记什么层级。
//
// 没有子节点就是叶子（0）；有子节点才是父节点（子节点最高层级 + 1）。这条区分
// 直接对应 MemoryBatch.Level 的定义，也决定了面板上那棵树看起来是否自洽。
func collapsedLevel(absorbed []domain.MemoryBatch) int {
	if len(absorbed) == 0 {
		return 0
	}
	return highestLevel(absorbed) + 1
}

// highestLevel 返回一组节点里最高的层级。
func highestLevel(batches []domain.MemoryBatch) int {
	highest := 0
	for _, batch := range batches {
		highest = max(highest, batch.Level)
	}
	return highest
}

// idsOf 取出一组节点的 ID。
func idsOf(batches []domain.MemoryBatch) []domain.MemoryBatchID {
	ids := make([]domain.MemoryBatchID, 0, len(batches))
	for _, batch := range batches {
		ids = append(ids, batch.ID)
	}
	return ids
}

// appendLeaf 把新叶子加进仓库和前沿，并推进游标。
//
// 游标只在这里前进——塌缩父节点不改变它，因为塌缩没有覆盖任何新的原始消息。
func appendLeaf(memory domain.ConversationMemory, batch domain.MemoryBatch) domain.ConversationMemory {
	next := memory
	next.RawCompactionCursor = batch.EndMessageIndex
	//构造一个完整的摘要历史
	next.Batches = append(append([]domain.MemoryBatch(nil), memory.Batches...), batch)
	//把这个新的叶子节点的id加入有效摘要id列表中
	next.ActiveBatchIDs = append(append([]domain.MemoryBatchID(nil), memory.ActiveBatchIDs...), batch.ID)
	//返回完整摘要
	return next
}

// collapseFrontier 把前沿最前面 count 个节点原位换成它们的父节点。
//
// 子节点**留在仓库里**，只从前沿移除。这是分层记忆可回查的基础：模型看到父摘要
// 之后，可以 inspect 它拿到全部子 ID，再往下走，直到叶子和原始消息。
func collapseFrontier(
	memory domain.ConversationMemory,
	count int,
	parent domain.MemoryBatch,
) domain.ConversationMemory {
	// 现在的容量为：
	// 原有效摘要数-合并摘要数+一个叶子
	active := make([]domain.MemoryBatchID, 0, len(memory.ActiveBatchIDs)-count+1)
	active = append(active, parent.ID)
	// 实际上就是把新生成的叶子节点加在后面
	active = append(active, memory.ActiveBatchIDs[count:]...)

	next := memory
	next.Batches = append(append([]domain.MemoryBatch(nil), memory.Batches...), parent)
	next.ActiveBatchIDs = active
	return next
}
