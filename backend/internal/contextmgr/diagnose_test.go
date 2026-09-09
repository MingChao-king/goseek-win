package contextmgr

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// quoteHeavySession 造一个**用户原话本身就撑爆目标线**的会话。
//
// 这是压缩第 3 步存在的唯一理由。它要用上千条用户消息才成立——正常会话里用户消息
// 只占 0.4%，这条路径几乎不会触发。构造它是为了证明"无限轮次"那句话确实成立，
// 而不是一句好听的话。
func quoteHeavySession(t *testing.T, count int) *domain.Session {
	t.Helper()
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	for index := range count {
		turnID := domain.TurnID(fmt.Sprintf("trn_%032d", index))
		session.Append(domain.Message{
			Role: domain.RoleUser, TurnID: turnID,
			Content: fmt.Sprintf("第 %d 条要求：把这件事记住，之后一直照这个来", index+1),
		})
		session.Append(domain.Message{
			Role: domain.RoleAssistant, TurnID: turnID, Content: "好的。",
		})
	}
	return session
}

// 用户原话撑爆触发线时，第 3 步把**全部历史**（含原话）交给模型总结成一个摘要。
//
// 关键是"交给模型"而不是程序性丢弃：那些话里有这个会话一直要遵守的长期约束，
// 机械裁掉就把它们连语义一起丢了。
func TestStepThreeCollapsesEverythingIncludingQuotes(t *testing.T) {
	session := quoteHeavySession(t, 400)
	summarizer := &fakeSummarizer{}
	compactor := newTestCompactor(summarizer, 12000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	// 全塌之后：水位线推到末尾（原话已经写进摘要，不该再逐条渲染一遍），
	// 整理结果清空（它的内容现在在摘要正文里）。
	if result.Memory.QuotesCollapsedBefore != len(session.Messages()) {
		t.Errorf("水位线 = %d; want %d",
			result.Memory.QuotesCollapsedBefore, len(session.Messages()))
	}
	if result.Memory.CollapsedQuotes != "" {
		t.Errorf("整理结果没有清空，会和摘要正文里的那份重复: %q", result.Memory.CollapsedQuotes)
	}
	// 渲染出来不该再有"期间用户说过"那一栏——否则同一批内容进两次上下文。
	view := buildMessages(session.Messages(), result.Memory, nil)
	for _, message := range view {
		if strings.Contains(message.Content, "期间用户说过") {
			t.Error("全塌之后仍然渲染了用户原话列表，同一批内容进了两次上下文")
		}
	}

	// 指令里要点名"用户要的是什么"单独成段，否则它会被过程记录稀释掉——
	// 实测工具观察占 95%、用户消息占 0.8%，模型按篇幅分配注意力。
	found := false
	for _, instructions := range summarizer.instructions {
		if strings.Contains(instructions, "用户要的是什么") {
			found = true
		}
	}
	if !found {
		t.Error("终结性压缩的指令里没有要求把用户诉求单独成段")
	}
}

// 交给模型的源文本里必须**真的有那些原话**，而不是一句"这里有很多话"。
func TestCollapsedQuotesGoIntoTheSourceText(t *testing.T) {
	session := quoteHeavySession(t, 400)
	summarizer := &fakeSummarizer{}
	compactor := newTestCompactor(summarizer, 12000)

	if _, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{}); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	found := false
	for _, source := range summarizer.sources {
		if strings.Contains(source, "第 1 条要求") && strings.Contains(source, "第 100 条要求") {
			found = true
		}
	}
	if !found {
		t.Error("没有任何一次摘要请求带上了用户原话本身")
	}
}

// 水位线只前进不后退。
//
// 单调增长的量，降级也应当是单调的：每次渲染重算的裁剪会让同一段内容在"在上下文 /
// 不在上下文"之间来回摆动，而它其实永远回不来了——摆动本身就是在骗人。
func TestQuoteWatermarkOnlyMovesForward(t *testing.T) {
	session := quoteHeavySession(t, 400)
	compactor := newTestCompactor(&fakeSummarizer{}, 12000)

	first, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("第一次 Compact 返回错误: %v", err)
	}
	if first.Memory.QuotesCollapsedBefore == 0 {
		t.Skip("这次压缩没走到终结性兜底，后面的断言无从谈起")
	}

	// 再压一次，起点是上一次的结果。
	second, err := compactor.Compact(context.Background(), session, first.Memory)
	if err != nil {
		t.Fatalf("第二次 Compact 返回错误: %v", err)
	}

	if second.Memory.QuotesCollapsedBefore < first.Memory.QuotesCollapsedBefore {
		t.Errorf("水位线后退了：%d → %d",
			first.Memory.QuotesCollapsedBefore, second.Memory.QuotesCollapsedBefore)
	}
}

// **终止性**：用户原话就超过目标线的会话，压完之后必须低于硬边界。
//
// 这是 5.13.1 那条大前提的直接检验——"压不到硬边界"不是一个合法终态，因为那意味着
// 请求发不出去、对话终止。
func TestCompactionAlwaysGetsBelowTheHardLimit(t *testing.T) {
	cases := []struct {
		name    string
		session *domain.Session
		window  int
	}{
		{"用户原话撑爆", quoteHeavySession(t, 800), 20000},
		{"单轮原文撑爆", bulkyOneTurnSession(t), 20000},
		{"两者同时", bothOverloadedSession(t), 20000},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			summarizer := &fakeSummarizer{}
			compactor := newTestCompactor(summarizer, testCase.window)

			result, err := compactor.Compact(
				context.Background(), testCase.session, domain.ConversationMemory{})
			if err != nil {
				t.Fatalf("Compact 返回错误: %v", err)
			}

			if result.AfterTokens >= compactor.thresholds.HardLimit {
				t.Errorf("压完仍在硬边界之上：%d ≥ %d（这个会话的请求发不出去）",
					result.AfterTokens, compactor.thresholds.HardLimit)
			}
			if summarizer.calls > maxCompactionSteps {
				t.Errorf("调用了 %d 次模型; want 至多 %d", summarizer.calls, maxCompactionSteps)
			}
			if err := result.Memory.CheckInvariant(); err != nil {
				t.Errorf("不变量被破坏: %v", err)
			}
			assertNoDanglingCalls(t, messagesAfterCursor(testCase.session, result.Memory))
		})
	}
}

// bulkyOneTurnSession 造一个"一轮里几十次大输出命令"的会话。
func bulkyOneTurnSession(t *testing.T) *domain.Session {
	t.Helper()
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "trn_00000000000000000000000000000000",
		Content: "先做点别的"})
	session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: "trn_00000000000000000000000000000000",
		Content: "好的"})

	// 第二轮：一条用户消息 + 40 次调用/观察，每次观察都很大。
	turn := domain.TurnID("trn_00000000000000000000000000000001")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: turn, Content: "把所有文件都列一遍"})
	for index := range 40 {
		id := fmt.Sprintf("call-%d", index)
		session.Append(domain.Message{
			Role: domain.RoleAssistant, TurnID: turn,
			ToolCalls: []domain.ToolCall{{ID: id, Name: "bash"}},
		})
		session.Append(domain.Message{
			Role: domain.RoleTool, TurnID: turn, ToolCallID: id,
			Content: strings.Repeat("一大堆输出", 200),
		})
	}
	return session
}

// bothOverloadedSession 造一个两个病因同时成立的会话。
//
// 这一档是真正的判据：如果它压不下去，"压缩一定成功"那条保证就有洞。
func bothOverloadedSession(t *testing.T) *domain.Session {
	t.Helper()
	session := quoteHeavySession(t, 600)
	turn := domain.TurnID("trn_99999999999999999999999999999999")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: turn, Content: "现在把所有文件列一遍"})
	for index := range 30 {
		id := fmt.Sprintf("late-call-%d", index)
		session.Append(domain.Message{
			Role: domain.RoleAssistant, TurnID: turn,
			ToolCalls: []domain.ToolCall{{ID: id, Name: "bash"}},
		})
		session.Append(domain.Message{
			Role: domain.RoleTool, TurnID: turn, ToolCallID: id,
			Content: strings.Repeat("一大堆输出", 200),
		})
	}
	return session
}

// messagesAfterCursor 取出游标之后仍以原文进入上下文的那些消息。
func messagesAfterCursor(session *domain.Session, memory domain.ConversationMemory) []domain.Message {
	messages := session.Messages()
	cursor := min(max(memory.RawCompactionCursor, 0), len(messages))
	return messages[cursor:]
}

// 全塌产生的节点，层级必须和 MemoryBatch.Level 的定义对上：
// **直接由原始消息生成的叶子为 0**，父节点才是子节点最高层级加一。
//
// 这条是真机跑出来被看出来的：一个只有一轮的会话连续两次全塌，库里留下
// 「L1 覆盖 #1-5、子节点 0 个」和「L2 覆盖 #1-9、子节点 1 个」——面板上展开
// L2 → L1 之后就到底了，看起来像树断了一层。根因是 highestLevel(nil)+1 = 1，
// 而那个节点其实是叶子。
func TestCollapsedLevelMatchesTheDefinition(t *testing.T) {
	cases := []struct {
		name     string
		absorbed []domain.MemoryBatch
		want     int
	}{
		{"没吸收任何摘要 → 叶子", nil, 0},
		{"吸收了一个 L0 → L1", []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 0, 0, 4)}, 1},
		{"吸收了 L0 和 L2 → L3", []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 0, 0, 4),
			batchAt("mem_0000000000000000000000000000000b", 2, 4, 8)}, 3},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := collapsedLevel(testCase.absorbed); got != testCase.want {
				t.Errorf("collapsedLevel = %d; want %d", got, testCase.want)
			}
		})
	}
}

// 端到端：单轮会话连续两次全塌，树必须是 L0（叶子，无子）→ L1（父，一个子）。
//
// 顺带说明为什么这个会话里第 1 步一次都没跑：只有一轮时"保留最近两个完整轮次"
// 等于保留全部，可压缩区是空的。所有节点都出自第 3 步。
func TestTwoCollapsesInOneTurnProduceAConsistentTree(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	// 全部消息同一个 TurnID —— 一轮。
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "跑三条命令"})
	for index := range 2 {
		id := fmt.Sprintf("c%d", index)
		session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: "t1",
			ToolCalls: []domain.ToolCall{{ID: id, Name: "bash"}}})
		session.Append(domain.Message{Role: domain.RoleTool, TurnID: "t1", ToolCallID: id,
			Content: strings.Repeat("很大的一段输出", 1500)})
	}

	compactor := newTestCompactor(&fakeSummarizer{}, 12000)
	first, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("第一次 Compact 返回错误: %v", err)
	}
	if len(first.NewBatches) != 1 || first.NewBatches[0].Level != 0 {
		t.Fatalf("第一次全塌产生 %+v; want 一个 L0 叶子",
			levelsOf(first.NewBatches))
	}

	// 这一轮继续跑，再撑爆一次。
	for index := 2; index < 4; index++ {
		id := fmt.Sprintf("c%d", index)
		session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: "t1",
			ToolCalls: []domain.ToolCall{{ID: id, Name: "bash"}}})
		session.Append(domain.Message{Role: domain.RoleTool, TurnID: "t1", ToolCallID: id,
			Content: strings.Repeat("又是很大的一段输出", 1500)})
	}
	second, err := compactor.Compact(context.Background(), session, first.Memory)
	if err != nil {
		t.Fatalf("第二次 Compact 返回错误: %v", err)
	}
	if len(second.NewBatches) != 1 || second.NewBatches[0].Level != 1 {
		t.Fatalf("第二次全塌产生 %v; want 一个 L1 父节点", levelsOf(second.NewBatches))
	}

	// 树自洽：L1 有一个子节点 L0，而 L0 是叶子（没有子节点）。
	parent := second.NewBatches[0]
	if len(parent.SourceBatchIDs) != 1 {
		t.Fatalf("L1 有 %d 个子节点; want 1", len(parent.SourceBatchIDs))
	}
	child, found := second.Memory.Batch(parent.SourceBatchIDs[0])
	if !found {
		t.Fatal("L1 的子节点不在仓库里")
	}
	if child.Level != 0 || len(child.SourceBatchIDs) != 0 {
		t.Errorf("子节点 = L%d 带 %d 个子; want L0 且无子（它直接覆盖原始消息）",
			child.Level, len(child.SourceBatchIDs))
	}
}

// levelsOf 取出一组节点的层级，失败信息里打出来。
func levelsOf(batches []domain.MemoryBatch) []int {
	levels := make([]int, 0, len(batches))
	for _, batch := range batches {
		levels = append(levels, batch.Level)
	}
	return levels
}
