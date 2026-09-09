package contextmgr

import (
	"fmt"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// quotesSession 造一段有三轮对话的历史，每轮一条用户消息加两条别的。
func quotesMessages(turns int) []domain.Message {
	var messages []domain.Message
	for index := range turns {
		turnID := domain.TurnID(fmt.Sprintf("trn_%032d", index))
		messages = append(messages,
			domain.Message{Role: domain.RoleUser, TurnID: turnID,
				Content: fmt.Sprintf("第 %d 个要求", index+1)},
			domain.Message{Role: domain.RoleAssistant, TurnID: turnID,
				Content: fmt.Sprintf("第 %d 个回答", index+1)},
			domain.Message{Role: domain.RoleTool, TurnID: turnID, ToolCallID: "c",
				Content: fmt.Sprintf("第 %d 个观察", index+1)},
		)
	}
	return messages
}

// 用户原话在渲染时从原始消息派生，逐条完整，一条不漏。
func TestQuotesAreDerivedCompletely(t *testing.T) {
	messages := quotesMessages(3)
	quotes := deriveQuotes(messages, splitTurns(messages), 0, len(messages))

	if len(quotes) != 3 {
		t.Fatalf("派生出 %d 条用户原话; want 3", len(quotes))
	}
	for index, quote := range quotes {
		want := fmt.Sprintf("第 %d 个要求", index+1)
		if quote.Text != want {
			t.Errorf("第 %d 条是 %q; want %q", index+1, quote.Text, want)
		}
	}
}

// 每条带它引发那一轮的区间，且与 splitTurns 的结果一致。
//
// 这份列表因此同时是索引：模型看到 #4–#6，一步 read from=4 count=3 就到位。
func TestQuotesCarryTheTurnRange(t *testing.T) {
	messages := quotesMessages(3)
	turns := splitTurns(messages)
	quotes := deriveQuotes(messages, turns, 0, len(messages))

	for index, quote := range quotes {
		turn := turns[index]
		if quote.TurnStart != turn.Start+1 || quote.TurnEnd != turn.End {
			t.Errorf("第 %d 条的区间 = #%d–#%d; want #%d–#%d",
				index+1, quote.TurnStart, quote.TurnEnd, turn.Start+1, turn.End)
		}
	}
}

// 只派生指定区间里的原话：一个摘要节点只该列出它自己覆盖的那些。
func TestQuotesRespectTheRange(t *testing.T) {
	messages := quotesMessages(3)
	quotes := deriveQuotes(messages, splitTurns(messages), 3, 6)

	if len(quotes) != 1 || quotes[0].Text != "第 2 个要求" {
		t.Errorf("区间 [3,6) 派生出 %+v; want 只有第 2 个要求", quotes)
	}
}

// 超长的用户消息**一个字都不裁**。
//
// 这一条从"截断并标注"改成了"完全不裁"：后端不再截断任何东西。用户消息只占全部
// 内容的 0.4%–0.8%，为一个占比不到 1% 的东西设上限，是拿一个几乎不存在的成本风险
// 去换真实的信息损失——而它恰恰是唯一无法重新推导的内容。
func TestOverlongQuoteIsKeptWholeNow(t *testing.T) {
	long := strings.Repeat("这是我贴进来的一大段服务日志", 200)
	messages := []domain.Message{{Role: domain.RoleUser, TurnID: "t1", Content: long}}

	quotes := deriveQuotes(messages, splitTurns(messages), 0, 1)

	if len(quotes) != 1 {
		t.Fatalf("派生出 %d 条; want 1", len(quotes))
	}
	if quotes[0].Text != long {
		t.Errorf("超长消息被改动了：原文 %d 字符，派生出 %d 字符",
			len([]rune(long)), len([]rune(quotes[0].Text)))
	}
	// 渲染进上下文的那份同样是完整的。
	rendered := renderQuotes(quotes, 0, "")
	if !strings.Contains(rendered, long[:60]) || !strings.Contains(rendered, long[len(long)-60:]) {
		t.Error("渲染结果里缺了原文的开头或结尾")
	}
	if strings.Contains(rendered, "已截断") {
		t.Errorf("渲染结果里仍然出现了截断标注:\n%s", rendered[:200])
	}
}

// 正常长度的消息一个字都不动。
func TestNormalQuoteIsNotTouched(t *testing.T) {
	content := "注释要详细，行内注释、行外注释都要"
	messages := []domain.Message{{Role: domain.RoleUser, TurnID: "t1", Content: content}}
	quotes := deriveQuotes(messages, splitTurns(messages), 0, 1)

	if quotes[0].Text != content {
		t.Errorf("正常消息被改动了: %q", quotes[0].Text)
	}
}

// 水位线之前只渲染一行说明加整理结果，之后逐条完整。
func TestWatermarkSplitsCollapsedFromListed(t *testing.T) {
	messages := quotesMessages(3)
	quotes := deriveQuotes(messages, splitTurns(messages), 0, len(messages))

	// 前两条（#1 和 #4）并掉，第三条（#7）仍逐条列出。
	rendered := renderQuotes(quotes, 4, "用户在这一段里反复要求：注释要写详细。")

	if strings.Contains(rendered, "第 1 个要求") || strings.Contains(rendered, "第 2 个要求") {
		t.Errorf("水位线之前的原话还在逐条列:\n%s", rendered)
	}
	if !strings.Contains(rendered, "第 3 个要求") {
		t.Errorf("水位线之后的原话没有列出来:\n%s", rendered)
	}
	if !strings.Contains(rendered, "（已压缩）#1–#4") {
		t.Errorf("没有标出被并掉的区间:\n%s", rendered)
	}
	if !strings.Contains(rendered, "注释要写详细") {
		t.Errorf("整理结果没有渲染出来:\n%s", rendered)
	}
	// 必须写清楚怎么取回原文，否则模型只会认为那些话没了。
	if !strings.Contains(rendered, "search") {
		t.Errorf("没有说明原文怎么取回:\n%s", rendered)
	}
}

// 水位线为 0（绝大多数会话的终生状态）时就是一份逐条列表，不多出任何东西。
func TestNoWatermarkMeansPlainList(t *testing.T) {
	messages := quotesMessages(2)
	quotes := deriveQuotes(messages, splitTurns(messages), 0, len(messages))

	rendered := renderQuotes(quotes, 0, "")

	if strings.Contains(rendered, "已压缩") {
		t.Errorf("水位线为 0 却渲染了压缩说明:\n%s", rendered)
	}
	if !strings.Contains(rendered, "第 1 个要求") || !strings.Contains(rendered, "第 2 个要求") {
		t.Errorf("逐条列表不完整:\n%s", rendered)
	}
}

// 一条用户消息都没有的区间不渲染这一栏——不该多出一个空标题。
func TestEmptyQuoteSectionIsNotRendered(t *testing.T) {
	if rendered := renderQuotes(nil, 0, ""); rendered != "" {
		t.Errorf("空区间渲染出了内容:\n%s", rendered)
	}
}

// 多行的用户消息压成一行，保留"这里断过行"这个信息。
func TestMultilineQuoteBecomesOneLine(t *testing.T) {
	messages := []domain.Message{{
		Role: domain.RoleUser, TurnID: "t1", Content: "第一点\n第二点\n第三点",
	}}
	quotes := deriveQuotes(messages, splitTurns(messages), 0, 1)
	rendered := renderQuotes(quotes, 0, "")

	body := strings.TrimSpace(strings.Split(rendered, "\n")[1])
	if strings.Contains(body, "\n") {
		t.Errorf("多行没有压成一行:\n%s", rendered)
	}
	if !strings.Contains(body, "⏎") {
		t.Errorf("换行的痕迹被抹掉了:\n%s", body)
	}
}

// 用户原话**不占存储**：batch.Content 里一个字都没有它们。
//
// 这是整套派生化设计的核心断言。如果原话进了存储，合并时父节点必须包含全部子节点
// 的条目，这一节就会单调累积，"整理压缩"退化成"拼接"，层级越高的摘要反而越大。
func TestQuotesNeverEnterBatchContent(t *testing.T) {
	session := buildSession(t, 8)
	summarizer := &fakeSummarizer{}
	compactor := newTestCompactor(summarizer, 6000)

	result, err := compactor.Compact(t.Context(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}
	if len(result.NewBatches) == 0 {
		t.Fatal("没有生成任何节点，这条测试没测到东西")
	}

	// 交给摘要器的**指令**里必须写明这件事——否则模型会照着记录再抄一遍原话。
	for index, instructions := range summarizer.instructions {
		if !strings.Contains(instructions, "不要复述用户说过的话") {
			t.Errorf("第 %d 次摘要请求没有告诉模型别复述用户原话", index+1)
		}
	}
}

// 渲染进上下文的摘要里必须带着派生的原话——这是"每一层都完整"的直接检验。
func TestRenderedMemoryCarriesTheQuotes(t *testing.T) {
	messages := quotesMessages(3)
	batch := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000a", Level: 0,
		Content: "主题：三轮对话", StartMessageIndex: 0, EndMessageIndex: 9,
	}

	rendered := renderMemory(
		[]domain.MemoryBatch{batch}, messages, splitTurns(messages), 0, "")

	if len(rendered) != 1 {
		t.Fatalf("渲染出 %d 条消息; want 1", len(rendered))
	}
	for index := range 3 {
		want := fmt.Sprintf("第 %d 个要求", index+1)
		if !strings.Contains(rendered[0].Content, want) {
			t.Errorf("渲染结果里缺 %q:\n%s", want, rendered[0].Content)
		}
	}
	// 散文仍然在里面——派生的部分是**附加**的，不是替代。
	if !strings.Contains(rendered[0].Content, "主题：三轮对话") {
		t.Errorf("散文正文丢了:\n%s", rendered[0].Content)
	}
}

// 合并之后的父节点同样带着它覆盖范围内的全部原话。
//
// "每一层都完整"正是派生化换来的性质：不管树长到几层，原话都是从原始消息重新抽
// 出来的，100% 准确，而且没有任何一层为它付存储代价。
func TestQuotesStayCompleteAtEveryLevel(t *testing.T) {
	messages := quotesMessages(4)
	parent := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000f", Level: 3,
		Content: "合并摘要", StartMessageIndex: 0, EndMessageIndex: 12,
	}

	rendered := renderMemory(
		[]domain.MemoryBatch{parent}, messages, splitTurns(messages), 0, "")

	for index := range 4 {
		want := fmt.Sprintf("第 %d 个要求", index+1)
		if !strings.Contains(rendered[0].Content, want) {
			t.Errorf("level 3 的节点里缺 %q:\n%s", want, rendered[0].Content)
		}
	}
}
