package contextmgr

import (
	"strings"
	"testing"

	"goseek/internal/domain"
)

// editedSession 造一个有一段活跃摘要的会话。
func editedSession(t *testing.T, edited string) (*domain.Session, domain.ConversationMemory) {
	t.Helper()
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	for index := range 4 {
		turnID := domain.TurnID("trn_0000000000000000000000000000000" + string(rune('0'+index)))
		session.Append(domain.Message{Role: domain.RoleUser, TurnID: turnID, Content: "问题"})
		session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: turnID, Content: "回答"})
	}
	batch := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000a", Level: 0,
		Content:           "模型写的摘要：讨论了目录结构。",
		EditedContent:     edited,
		StartMessageIndex: 0, EndMessageIndex: 4,
	}
	return session, domain.ConversationMemory{
		RawCompactionCursor: 4,
		Batches:             []domain.MemoryBatch{batch},
		ActiveBatchIDs:      []domain.MemoryBatchID{batch.ID},
	}
}

// 修订之后，**真正发给模型的那条消息**里装的是修订版，不是模型原文。
//
// 这一条锁的是整条链路的终点。前面几层（EditedContent 字段、EffectiveContent、
// 落库、HTTP 返回）各自都有测试，但"它最后确实进了模型上下文"此前没有任何东西
// 锁住——把 renderMemory 里那次 EffectiveContent() 改回 batch.Content，
// 所有其他测试都会照常通过。
func TestEditedSummaryIsWhatTheModelActuallyReceives(t *testing.T) {
	session, memory := editedSession(t, "人工校订：机房门禁码 ZZ-8842。")

	view := Build(session, memory, nil, 100000)

	var summaryMessage string
	for _, message := range view.Messages {
		if strings.Contains(message.Content, "batch_id=") {
			summaryMessage = message.Content
		}
	}
	if summaryMessage == "" {
		t.Fatal("视图里没有摘要消息")
	}
	if !strings.Contains(summaryMessage, "ZZ-8842") {
		t.Errorf("发给模型的摘要里没有修订内容:\n%s", summaryMessage)
	}
	// 更要紧的是：模型原文**不应该**同时出现，否则等于两版都发过去了。
	if strings.Contains(summaryMessage, "模型写的摘要") {
		t.Errorf("修订版和模型原文同时进了上下文:\n%s", summaryMessage)
	}
}

// 修订之后估算出来的占用要跟着变——因为被数的就是那条已经渲染好的消息。
//
// EditedContent 本身从不"参与计算"：它在 renderMemory 那一刻就被摊平成了普通的
// 消息正文，之后 EstimateRequestTokens 只认那个字符串，根本不知道"原文 / 修订"
// 这回事存在。这条测试锁的就是这个摊平确实发生了。
func TestEditingChangesTheEstimatedUsage(t *testing.T) {
	plain, plainMemory := editedSession(t, "")
	edited, editedMemory := editedSession(t, "模型写的摘要：讨论了目录结构。"+strings.Repeat("补充说明。", 200))

	before := Build(plain, plainMemory, nil, 100000).Usage.InputTokens
	after := Build(edited, editedMemory, nil, 100000).Usage.InputTokens

	if after <= before {
		t.Errorf("把摘要改长之后占用没有变大：%d → %d", before, after)
	}
	t.Logf("占用 %d → %d（改长约 1000 字）", before, after)
}

// 撤销修订（空串）之后，模型看到的回到原文。
func TestRevertingPutsTheOriginalBackIntoTheContext(t *testing.T) {
	session, memory := editedSession(t, "")

	view := Build(session, memory, nil, 100000)

	joined := ""
	for _, message := range view.Messages {
		joined += message.Content
	}
	if !strings.Contains(joined, "模型写的摘要") {
		t.Error("没有修订时上下文里应当是模型原文")
	}
}

// 合并时喂给模型的也是修订版：否则人改过的内容会在下一次合并时被悄悄丢掉。
func TestMergingFeedsTheEditedVersionToTheSummarizer(t *testing.T) {
	summarizer := &fakeSummarizer{}
	pair := []domain.MemoryBatch{
		{ID: "mem_a", Content: "原文甲", EditedContent: "校订甲 ZZ-8842", StartMessageIndex: 0, EndMessageIndex: 2},
		{ID: "mem_b", Content: "原文乙", StartMessageIndex: 2, EndMessageIndex: 4},
	}

	if _, err := summarizeBatches(t.Context(), summarizer, pair); err != nil {
		t.Fatalf("summarizeBatches 返回错误: %v", err)
	}

	source := summarizer.sources[0]
	if !strings.Contains(source, "校订甲 ZZ-8842") {
		t.Errorf("合并的输入里没有修订版:\n%s", source)
	}
	if strings.Contains(source, "原文甲") {
		t.Errorf("合并的输入里混进了模型原文:\n%s", source)
	}
	// 没改过的那个照常用原文。
	if !strings.Contains(source, "原文乙") {
		t.Errorf("没改过的节点没有进入合并输入:\n%s", source)
	}
}
