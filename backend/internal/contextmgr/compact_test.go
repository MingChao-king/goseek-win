package contextmgr

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// fakeSummarizer 返回可预测的短摘要，并记录被调用了几次、每次收到什么。
//
// 摘要必须**短**：压缩的效果就是"用更少的 token 表达同一段历史"，如果假摘要和
// 原文一样长，收敛条件就永远不满足，测试会卡在死循环上——那恰恰说明这些测试
// 真的在检验收敛。
type fakeSummarizer struct {
	calls        int
	sources      []string
	instructions []string
	// fail 非 nil 时每次调用都返回它。
	fail error
	// expand 非空时把它拼到摘要后面，用来构造"摘要比原文还长"的情况。
	expand string
}

func (fake *fakeSummarizer) Summarize(_ context.Context, instructions string, source string) (string, error) {
	fake.calls++
	fake.sources = append(fake.sources, source)
	fake.instructions = append(fake.instructions, instructions)
	if fake.fail != nil {
		return "", fake.fail
	}
	return fmt.Sprintf("摘要%d%s", fake.calls, fake.expand), nil
}

// buildSession 造一个有若干轮对话的会话，每轮包含一条用户消息和一条长回复。
//
// 回复故意很长：只有原文足够占地方，压缩才有可观察的效果。
func buildSession(t *testing.T, turns int) *domain.Session {
	t.Helper()
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	for index := range turns {
		turnID := domain.TurnID(fmt.Sprintf("trn_%032d", index))
		session.Append(domain.Message{
			Role: domain.RoleUser, TurnID: turnID,
			Content: fmt.Sprintf("第 %d 个问题", index),
		})
		session.Append(domain.Message{
			Role: domain.RoleAssistant, TurnID: turnID,
			Content: fmt.Sprintf("第 %d 个回答：%s", index, strings.Repeat("很长的内容", 60)),
		})
	}
	return session
}

// newTestCompactor 造一个使用固定时钟的压缩器。
func newTestCompactor(summarizer Summarizer, window int) *Compactor {
	compactor := NewCompactor(summarizer, NewThresholds(window), nil, nil)
	compactor.now = func() time.Time { return time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC) }
	return compactor
}

func TestThresholdsAreOrdered(t *testing.T) {
	thresholds := NewThresholds(100000)

	if !(thresholds.CompactTo < thresholds.CompactAt && thresholds.CompactAt < thresholds.HardLimit) {
		t.Fatalf("三条线的顺序不对: %+v", thresholds)
	}
	// 触发线和目标线拉得足够开，否则压完一点点又会立刻再触发，
	// 每轮都要多花一次模型调用。
	if thresholds.CompactAt < thresholds.CompactTo*2 {
		t.Errorf("触发线 %d 与目标线 %d 挨得太近", thresholds.CompactAt, thresholds.CompactTo)
	}
}

// 窗口未知时永不压缩：压缩要花钱、要等待，还会把早期对话变成有损摘要，
// 基于一个编出来的窗口做这件事，代价由用户承担。
func TestNoCompactionWhenWindowIsUnknown(t *testing.T) {
	thresholds := NewThresholds(0)

	if thresholds.ShouldCompact(999999) {
		t.Error("窗口未知时触发了压缩")
	}
}

// 压缩之后活跃摘要必须无缝覆盖 [0, 游标)。这是整套记忆结构唯一的不变量，
// 也是它正确性的全部：少一段是丢历史，多一段是同一段内容进了两次上下文。
func TestCompactionKeepsTheCoverageInvariant(t *testing.T) {
	session := buildSession(t, 8)
	summarizer := &fakeSummarizer{}
	// 窗口取得很小，逼出多步压缩。
	compactor := newTestCompactor(summarizer, 6000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}
	if len(result.NewBatches) == 0 {
		t.Fatal("没有产生任何摘要节点")
	}
	if err := result.Memory.CheckInvariant(); err != nil {
		t.Fatalf("不变量被破坏: %v", err)
	}
	if result.AfterTokens >= result.BeforeTokens {
		t.Errorf("压缩之后没有变小: %d → %d", result.BeforeTokens, result.AfterTokens)
	}
}

// 最近两个完整轮次的原文必须保留：模型最可能需要它们的细节。
func TestCompactionRetainsTheLastTwoTurns(t *testing.T) {
	session := buildSession(t, 6)
	compactor := newTestCompactor(&fakeSummarizer{}, 6000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	// 六轮、每轮两条消息共 12 条；保留最后两轮就是保留最后 4 条，
	// 因此游标最多推进到 8。
	if result.Memory.RawCompactionCursor > 8 {
		t.Errorf("游标推进到 %d，侵入了保留区（应当不超过 8）", result.Memory.RawCompactionCursor)
	}
}

// 叶子只在完整轮次的边界上切。把一次工具调用和它的观察拆到两个摘要里，
// 两边都会变得难以理解，而且会留下无法配对的悬空调用。
func TestLeafBatchesSplitOnTurnBoundaries(t *testing.T) {
	session := buildSession(t, 8)
	compactor := newTestCompactor(&fakeSummarizer{}, 6000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	turns := splitTurns(session.Messages())
	boundaries := map[int]bool{0: true}
	for _, turn := range turns {
		boundaries[turn.End] = true
	}
	for _, batch := range result.NewBatches {
		if batch.Level != 0 {
			continue // 父节点的边界由子节点决定
		}
		if !boundaries[batch.StartMessageIndex] || !boundaries[batch.EndMessageIndex] {
			t.Errorf("叶子 %s 覆盖 [%d,%d)，不在轮次边界上",
				batch.ID, batch.StartMessageIndex, batch.EndMessageIndex)
		}
	}
}

// 塌缩之后前沿恒为 2：[根, 最新叶子]。
//
// 保留最新那个叶子不是可有可无的细节——全塌会让刚生成的叶子立刻被二次压缩
// （同一段内容过两遍模型），而它恰恰是模型下一轮最可能回头看的内容。
// 留一个的代价是零：仍然一次调用。
func TestCollapseLeavesTheNewestLeafOnTheFrontier(t *testing.T) {
	memory := domain.ConversationMemory{
		RawCompactionCursor: 40,
		Batches: []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 0, 0, 10),
			batchAt("mem_0000000000000000000000000000000b", 0, 10, 20),
			batchAt("mem_0000000000000000000000000000000c", 0, 20, 30),
			batchAt("mem_0000000000000000000000000000000d", 0, 30, 40),
		},
		ActiveBatchIDs: []domain.MemoryBatchID{
			"mem_0000000000000000000000000000000a",
			"mem_0000000000000000000000000000000b",
			"mem_0000000000000000000000000000000c",
			"mem_0000000000000000000000000000000d",
		},
	}
	parent := batchAt("mem_0000000000000000000000000000000e", 1, 0, 30)

	// 塌掉前三个（最新的 d 留在前沿上）。
	collapsed := collapseFrontier(memory, 3, parent)

	if len(collapsed.ActiveBatchIDs) != 2 {
		t.Fatalf("塌缩后前沿有 %d 段; want 2", len(collapsed.ActiveBatchIDs))
	}
	if collapsed.ActiveBatchIDs[0] != parent.ID {
		t.Errorf("前沿第一段是 %s; want 父节点 %s", collapsed.ActiveBatchIDs[0], parent.ID)
	}
	if collapsed.ActiveBatchIDs[1] != "mem_0000000000000000000000000000000d" {
		t.Errorf("前沿第二段是 %s; want 最新的叶子 d", collapsed.ActiveBatchIDs[1])
	}
	// 覆盖不变量必须仍然成立：父节点接上最新叶子，正好覆盖 [0, 40)。
	if err := collapsed.CheckInvariant(); err != nil {
		t.Errorf("塌缩之后不变量被破坏: %v", err)
	}
	// 被塌掉的三个仍在仓库里——这是可回查的基础。
	for _, id := range []domain.MemoryBatchID{
		"mem_0000000000000000000000000000000a",
		"mem_0000000000000000000000000000000b",
		"mem_0000000000000000000000000000000c",
	} {
		if _, found := collapsed.Batch(id); !found {
			t.Errorf("被塌掉的节点 %s 从仓库里消失了", id)
		}
	}
}

// 合并只产生新的父节点，**子节点留在仓库里**。这是分层合并可回查的基础：
// 模型看到父摘要后能 inspect 出子节点，一层层走到原始消息。
func TestMergeKeepsChildrenInTheRepository(t *testing.T) {
	session := buildSession(t, 10)
	compactor := newTestCompactor(&fakeSummarizer{}, 1500)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	var parents []domain.MemoryBatch
	for _, batch := range result.Memory.Batches {
		if len(batch.SourceBatchIDs) > 0 {
			parents = append(parents, batch)
		}
	}
	if len(parents) == 0 {
		t.Skip("这次压缩没有触发合并")
	}
	for _, parent := range parents {
		for _, childID := range parent.SourceBatchIDs {
			if _, found := result.Memory.Batch(childID); !found {
				t.Errorf("父节点 %s 的子节点 %s 不在仓库里", parent.ID, childID)
			}
		}
		// 父节点必须比子节点高一层，覆盖范围是子范围的并集。
		for _, childID := range parent.SourceBatchIDs {
			child, _ := result.Memory.Batch(childID)
			if parent.Level <= child.Level {
				t.Errorf("父节点 %s 层级 %d 不高于子节点 %s 的 %d",
					parent.ID, parent.Level, childID, child.Level)
			}
			if child.StartMessageIndex < parent.StartMessageIndex ||
				child.EndMessageIndex > parent.EndMessageIndex {
				t.Errorf("子节点 %s 的范围超出父节点 %s", childID, parent.ID)
			}
		}
	}
}

// 摘要失败不该让已经生成的节点白费，也不该破坏不变量。
func TestCompactionFailureKeepsMemoryConsistent(t *testing.T) {
	session := buildSession(t, 6)
	compactor := newTestCompactor(&fakeSummarizer{fail: context.DeadlineExceeded}, 2000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err == nil {
		t.Fatal("摘要失败时 Compact 返回了成功")
	}
	// 一个节点都没生成时，记忆应当原样不动。
	if err := result.Memory.CheckInvariant(); err != nil {
		t.Errorf("失败后不变量被破坏: %v", err)
	}
}

// 已经低于目标线时不做任何事，也不该白花一次模型调用。
func TestNoWorkWhenAlreadyBelowTarget(t *testing.T) {
	session := buildSession(t, 1)
	summarizer := &fakeSummarizer{}
	compactor := newTestCompactor(summarizer, 1000000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}
	if summarizer.calls != 0 {
		t.Errorf("已经够小了还调用了 %d 次模型", summarizer.calls)
	}
	if result.Changed() {
		t.Error("已经够小了还产生了节点")
	}
}

// 视图 = system + 活跃摘要 + 游标之后的原文，三段必须无缝衔接。
func TestBuildSplicesMemoryAndRawMessages(t *testing.T) {
	session := buildSession(t, 4)
	compactor := newTestCompactor(&fakeSummarizer{}, 6000)
	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	view := Build(session, result.Memory, nil, 100000)
	cursor := result.Memory.RawCompactionCursor
	active := len(result.Memory.ActiveBatchIDs)
	history := len(session.Messages())

	// 1 条 system + 活跃摘要 + (总消息数 - 游标) 条原文。
	want := 1 + active + (history - cursor)
	if len(view.Messages) != want {
		t.Errorf("视图有 %d 条消息; want %d（system %d + 摘要 %d + 原文 %d）",
			len(view.Messages), want, 1, active, history-cursor)
	}
	if view.Messages[0].Role != domain.ModelRoleSystem {
		t.Error("首条不是 system 指令")
	}
	// 摘要必须带上 batch_id，模型才能回查。
	if active > 0 && !strings.Contains(view.Messages[1].Content, "batch_id=") {
		t.Errorf("摘要消息里没有 batch_id: %q", view.Messages[1].Content)
	}
}

// 不变量检查要能抓出空洞、重叠和游标对不上。
func TestCheckInvariantDetectsBrokenCoverage(t *testing.T) {
	cases := map[string]domain.ConversationMemory{
		"有空洞": {
			RawCompactionCursor: 20,
			Batches: []domain.MemoryBatch{
				{ID: "a", StartMessageIndex: 0, EndMessageIndex: 8},
				{ID: "b", StartMessageIndex: 10, EndMessageIndex: 20},
			},
			ActiveBatchIDs: []domain.MemoryBatchID{"a", "b"},
		},
		"有重叠": {
			RawCompactionCursor: 18,
			Batches: []domain.MemoryBatch{
				{ID: "a", StartMessageIndex: 0, EndMessageIndex: 10},
				{ID: "b", StartMessageIndex: 8, EndMessageIndex: 18},
			},
			ActiveBatchIDs: []domain.MemoryBatchID{"a", "b"},
		},
		"游标对不上": {
			RawCompactionCursor: 99,
			Batches:             []domain.MemoryBatch{{ID: "a", StartMessageIndex: 0, EndMessageIndex: 10}},
			ActiveBatchIDs:      []domain.MemoryBatchID{"a"},
		},
		"引用了不存在的节点": {
			RawCompactionCursor: 10,
			ActiveBatchIDs:      []domain.MemoryBatchID{"nope"},
		},
	}

	for name, memory := range cases {
		t.Run(name, func(t *testing.T) {
			if err := memory.CheckInvariant(); err == nil {
				t.Error("没有检测出来")
			}
		})
	}
}

// 摘要比原文还长时，这一步必须被丢掉。
//
// 这是一次真实运行暴露的缺陷：模型把两条消息的一轮对话总结成了更长的一段，
// 上下文从 4828 涨到 4919——压缩产生了负收益。记忆是只增不删的，让这样一个
// 节点永久留在上下文里，比白花一次模型调用更糟。
func TestASummaryLongerThanTheSourceIsDiscarded(t *testing.T) {
	session := buildSession(t, 8)
	// 这个假摘要器返回一大段文字，任何一段原文被它"压缩"之后都会变长。
	bloated := &fakeSummarizer{}
	bloated.expand = strings.Repeat("这段摘要写得比原文还长", 400)
	compactor := newTestCompactor(bloated, 2000)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})

	if err != nil {
		t.Fatalf("摘要变长不该被当成错误: %v", err)
	}
	if len(result.NewBatches) != 0 {
		t.Errorf("接受了 %d 个让上下文变大的节点", len(result.NewBatches))
	}
	if result.AfterTokens > result.BeforeTokens {
		t.Errorf("上下文被压大了: %d → %d", result.BeforeTokens, result.AfterTokens)
	}
	if !result.TargetUnreachable {
		t.Error("压不下去时没有标记 TargetUnreachable")
	}
	if err := result.Memory.CheckInvariant(); err != nil {
		t.Errorf("不变量被破坏: %v", err)
	}
}

// 一次 Compact 的模型调用次数上界是 maxCompactionSteps，**含全部路径**。
//
// 这是新算法相对成对合并的核心改进：成对合并把 K 个摘要收成 1 个要恰好 K−1 次调用，
// K≥20 时必然撞上限。现在流程本身只有三步。
func TestCompactionNeverExceedsTheCallBudget(t *testing.T) {
	// 摘要写得比原文还长：每一步都会被 acceptIfSmaller 拒掉，压缩推进不了。
	// 这是最容易让"多试几次"型代码失控的输入。
	bloated := &fakeSummarizer{expand: strings.Repeat("很长很长", 500)}
	session := buildSession(t, 8)
	compactor := newTestCompactor(bloated, 2000)

	if _, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{}); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if bloated.calls > maxCompactionSteps {
		t.Errorf("调用了 %d 次模型; want 至多 %d", bloated.calls, maxCompactionSteps)
	}
}

// 第 1 步压到目标线以下就不进第 2 步。
func TestCompactionStopsAfterTheLeafWhenTheTargetIsReached(t *testing.T) {
	summarizer := &fakeSummarizer{}
	session := buildSession(t, 8)
	// 目标线宽松：一个叶子就够了。
	compactor := newTestCompactor(summarizer, 100000)
	compactor.thresholds = Thresholds{HardLimit: 100000, CompactAt: 1, CompactTo: 99000}

	if _, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{}); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if summarizer.calls != 1 {
		t.Errorf("调用了 %d 次模型; want 1（第 1 步就够了）", summarizer.calls)
	}
}

// 仍在触发线之上时，第 3 步做**终结性全塌**：全部历史变成一个摘要。
//
// 判据是**触发线**而不是硬边界，理由很硬：第 3 步要把保留区交给模型总结，而摘要
// 请求的输入**就是那段保留区**——上下文卡在硬边界时那段源文本自己就接近整个窗口，
// 请求发不出去。**救援动作需要余量，而硬边界的定义就是没有余量。**
func TestStepThreeCollapsesEverythingWhenStillOverTheTrigger(t *testing.T) {
	session := buildSession(t, 3)
	compactor := newTestCompactor(&fakeSummarizer{}, 1200)

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	// 全塌之后：游标到末尾，前沿只剩一个摘要，没有任何原文。
	if result.Memory.RawCompactionCursor != len(session.Messages()) {
		t.Errorf("游标 = %d; want %d（全部历史都该进摘要）",
			result.Memory.RawCompactionCursor, len(session.Messages()))
	}
	if len(result.Memory.ActiveBatchIDs) != 1 {
		t.Errorf("前沿有 %d 段; want 1", len(result.Memory.ActiveBatchIDs))
	}
	if err := result.Memory.CheckInvariant(); err != nil {
		t.Errorf("不变量被破坏: %v", err)
	}
	// 第 3 步不是正常工作状态，必须说清楚发生了什么。
	if result.Reason == "" {
		t.Error("走到第 3 步却没有给出原因")
	}
}

// 第 3 步是**终结性**的：它只跑一次，不循环。
//
// 循环是危险的——每多转一圈就多一次摘要请求，而走到这里时上下文已经很紧了。
// 全塌一次就到位，所以不需要循环判断"够不够小"。
func TestStepThreeRunsAtMostOnce(t *testing.T) {
	summarizer := &fakeSummarizer{}
	session := buildSession(t, 3)
	compactor := newTestCompactor(summarizer, 1200)

	if _, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{}); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	// 第 1 步（叶子）+ 第 3 步（全塌）= 2 次；前沿不足 3 段时第 2 步被跳过。
	if summarizer.calls > 2 {
		t.Errorf("调用了 %d 次模型; want 至多 2（第 3 步不该循环）", summarizer.calls)
	}
}

// 全塌之后不存在悬空的工具调用——一次调用和它的观察要么都在摘要里，要么都不在。
//
// 这比在中间找配对边界简单得多，也更不容易出错：视图里根本没有原文。
func TestFinalCollapseLeavesNoRawMessagesAtAll(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "跑一下"})
	session.Append(domain.Message{
		Role: domain.RoleAssistant, TurnID: "t1",
		ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}},
	})
	session.Append(domain.Message{
		Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1",
		Content: strings.Repeat("很长的观察内容", 400),
	})

	compactor := newTestCompactor(&fakeSummarizer{}, 1200)
	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	view := buildMessages(session.Messages(), result.Memory, nil)
	for _, message := range view {
		if len(message.ToolCalls) > 0 {
			t.Errorf("全塌之后视图里还有工具调用：%+v", message.ToolCalls)
		}
	}
}

// 切点必须落在"调用 + 观察"的配对边界上：切开之后两边都不能有悬空调用。
//
// 少一条配对的观察，之后这个会话的**每一次**请求都会被供应商 400 拒绝——
// 也就是说会话从此报废。这是硬伤，不是瑕疵。
func TestCutPointNeverSeparatesACallFromItsObservation(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, Content: "开始", TurnID: "t1"},
		{Role: domain.RoleAssistant, TurnID: "t1", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: domain.RoleTool, ToolCallID: "c1", Content: "观察 1", TurnID: "t1"},
		{Role: domain.RoleAssistant, TurnID: "t1", ToolCalls: []domain.ToolCall{{ID: "c2", Name: "bash"}}},
		{Role: domain.RoleTool, ToolCallID: "c2", Content: "观察 2", TurnID: "t1"},
		{Role: domain.RoleAssistant, TurnID: "t1", ToolCalls: []domain.ToolCall{{ID: "c3", Name: "bash"}}},
		{Role: domain.RoleTool, ToolCallID: "c3", Content: "观察 3", TurnID: "t1"},
	}

	cut, ok := lastPairBoundary(messages, 0)

	if !ok {
		t.Fatal("找不到合法切点")
	}
	// 最后一个合法切点是 5（读完 #5 之后 c1、c2 都已配对，c3 还没发出）。
	// 取最后一个而不是第一个：切得越靠后并进摘要的原文越多，而这一步之所以存在
	// 就是因为前两步的收益已经不够了。
	if cut != 5 {
		t.Errorf("切点 = %d; want 5", cut)
	}
	assertNoDanglingCalls(t, messages[:cut])
	assertNoDanglingCalls(t, messages[cut:])
}

// 保留区里只剩一对调用/观察时切不动——那是不可分的最小单位。
//
// 唯一"读完之后没有悬空调用"的位置是末尾，而切在末尾等于把全部原文都压掉，
// 这次请求就没有任何原文可发了。所以这里必须报"无处可切"，让第 3 步停手。
func TestAnAtomicCallObservationPairHasNoCutPoint(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleAssistant, TurnID: "t1", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: domain.RoleTool, ToolCallID: "c1", Content: "观察", TurnID: "t1"},
	}

	if cut, ok := lastPairBoundary(messages, 0); ok {
		t.Errorf("竟然切在了 %d——一对调用/观察是不可分的", cut)
	}
}

// assertNoDanglingCalls 断言一段消息里每个工具调用都有配对的观察。
func assertNoDanglingCalls(t *testing.T, messages []domain.Message) {
	t.Helper()
	observed := make(map[string]bool)
	for _, message := range messages {
		if message.ToolCallID != "" {
			observed[message.ToolCallID] = true
		}
	}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if !observed[call.ID] {
				t.Errorf("调用 %s 没有配对的观察", call.ID)
			}
		}
	}
}

// 在触发线**以下**时不动保留区：没压到目标线只是"已经尽力"，不值得为它把
// 最近的原文也压掉。
//
// 这一条钉住 TargetUnreachable 这个终态确实存在——落在目标线与触发线之间时，
// 压缩就停在那里，下一轮继续。
func TestRetentionIsKeptWhileBelowTheTrigger(t *testing.T) {
	session := buildSession(t, 3)
	compactor := newTestCompactor(&fakeSummarizer{}, 100000)

	// 构造"压不到目标线、但也没到触发线"：目标线极低、触发线极高。
	compactor.thresholds = Thresholds{HardLimit: 1000000, CompactAt: 900000, CompactTo: 1}

	result, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{})
	if err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if result.Memory.RawCompactionCursor > 2 {
		t.Errorf("游标推进到 %d，在触发线以下就动了保留区", result.Memory.RawCompactionCursor)
	}
	if !result.TargetUnreachable {
		t.Error("压不到目标线时没有标记 TargetUnreachable")
	}
	// 没走第 3 步就不该给出兜底的原因。
	if strings.Contains(result.Reason, "塌成一个摘要") {
		t.Errorf("没到触发线却走了终结性兜底: %s", result.Reason)
	}
}

// 篇幅要求给的是**比例**，不是一个 token 数字。
//
// 上一版给的是"不超过 N 个字"，N 由原文长度算出来。M5.1 改成"压到原文的三分之一
// 以内"，理由是模型数不了自己的输出 token，而比例它可以靠对照原文判断。
func TestLengthBudgetIsARatioNotANumber(t *testing.T) {
	instructions := withLengthBudget(summaryInstructions)

	if !strings.Contains(instructions, "三分之一") {
		t.Errorf("篇幅要求里没有给出比例:\n%s", instructions)
	}
	// 不能出现"整段摘要不超过 N 个字"这种它观察不到的量。
	if strings.Contains(instructions, "不超过") {
		t.Errorf("篇幅要求里仍然写着字数上限:\n%s", instructions)
	}
	// 负收益的后果要说明白：硬保证在 acceptIfSmaller，但模型该知道这件事。
	if !strings.Contains(instructions, "负收益") {
		t.Error("篇幅要求里没有说明写长了会被整体丢弃")
	}
}

// 篇幅上限要真的出现在发给模型的指令里。
func TestInstructionsCarryTheLengthBudget(t *testing.T) {
	session := buildSession(t, 8)
	summarizer := &fakeSummarizer{}
	compactor := newTestCompactor(summarizer, 6000)

	if _, err := compactor.Compact(context.Background(), session, domain.ConversationMemory{}); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if len(summarizer.instructions) == 0 {
		t.Fatal("没有发出摘要请求")
	}
	for index, instructions := range summarizer.instructions {
		if !strings.Contains(instructions, "篇幅") {
			t.Errorf("第 %d 次请求的指令里没有篇幅要求", index+1)
		}
		// 摘要请求不能复用决策 prompt——那份 prompt 会让模型以为自己要去执行任务。
		if strings.Contains(instructions, "你可以调用工具") {
			t.Errorf("第 %d 次请求复用了决策 prompt", index+1)
		}
	}
}

// 可压缩区的终点由**轮次**决定，一轮之内不变——即使这一轮里又追加了工具消息。
//
// Agent 靠这个性质判断"这次压缩会不会白做"：终点没变就说明输入没变，
// 上次压不出东西，这次同样压不出。
func TestCompactableEndOnlyMovesWhenANewTurnStarts(t *testing.T) {
	session := buildSession(t, 5)
	before := CompactableEnd(session)

	// 在最后一轮里再追加两条消息（工具调用与观察），轮次数量不变。
	lastTurn := domain.TurnID(fmt.Sprintf("trn_%032d", 4))
	session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: lastTurn, Content: "我看一下"})
	session.Append(domain.Message{Role: domain.RoleTool, TurnID: lastTurn, Content: "观察"})

	if after := CompactableEnd(session); after != before {
		t.Errorf("同一轮里追加消息之后终点从 %d 变成了 %d", before, after)
	}

	// 开启新的一轮，保留区整体后移，终点必须前进。
	session.Append(domain.Message{
		Role: domain.RoleUser, TurnID: domain.TurnID(fmt.Sprintf("trn_%032d", 5)), Content: "再问一句",
	})
	if after := CompactableEnd(session); after <= before {
		t.Errorf("新开一轮之后终点没有前进：%d → %d", before, after)
	}
}

// 历史不足保留轮数时，可压缩区是空的——会话还很短，本来也没什么可压。
func TestNothingIsCompactableInAShortConversation(t *testing.T) {
	if end := CompactableEnd(buildSession(t, 2)); end != 0 {
		t.Errorf("两轮对话的可压缩区终点 = %d; want 0", end)
	}
}

// batchAt 构造一个覆盖 [start, end) 的摘要节点，用于纯结构断言。
//
// 正文里写上覆盖范围而不是留空：断言失败时打印出来一眼能看出是哪个节点，
// 而且 Title() 也就有内容了。
func batchAt(id domain.MemoryBatchID, level, start, end int) domain.MemoryBatch {
	return domain.MemoryBatch{
		ID:                id,
		Level:             level,
		Content:           fmt.Sprintf("摘要 %s 覆盖 [%d,%d)", id, start, end),
		StartMessageIndex: start,
		EndMessageIndex:   end,
	}
}

// 窗口太小时启动就要报错，而不是打一行告警。
//
// 在小于最小可行窗口的配置里，"压缩一定能把上下文压到硬边界以下"这条保证不成立。
// 告警会被划过去，然后在第几百轮的某一次请求上，用户得到的是供应商的一句 400
// 而不是回答，而那时根本看不出是窗口配小了。
func TestTooSmallAWindowIsRejectedAtStartup(t *testing.T) {
	// 6000 正是 M4.2 到 M4.6 全程用的那个值。它的目标线是 1140，**比常量部分
	// （1176）还小**——当时把"每次压缩都 TargetUnreachable"归因为"保留区占满"，
	// 其实光 system 指令加工具定义就已经超了。
	err := CheckWindow(6000)

	if err == nil {
		t.Fatal("6000 的窗口竟然通过了检查")
	}
	// 错误里要写清楚**需要多少**和**为什么**，否则用户只知道"不行"，不知道改成多少。
	for _, want := range []string{"至少需要", "固定开销"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误说明里缺 %q: %v", want, err)
		}
	}
}

// 够大的窗口照常通过。
func TestAViableWindowPasses(t *testing.T) {
	for _, window := range []int{32000, 128000, 1048576} {
		if err := CheckWindow(window); err != nil {
			t.Errorf("窗口 %d 被拒绝了: %v", window, err)
		}
	}
}

// 窗口未知（0）不是配置错误：那表示"永不压缩"，是一个用户可以明确选择的模式。
//
// 猜一个窗口才是错的：压缩要花钱、要等待，还会把早期对话变成有损摘要，
// 基于一个编出来的窗口做这件事，代价由用户承担。
func TestUnknownWindowIsNotAnError(t *testing.T) {
	if err := CheckWindow(0); err != nil {
		t.Errorf("窗口未知被当成了错误: %v", err)
	}
}

// 最小可行窗口是从"压缩一定成功"那条保证反推出来的，不是拍的。
func TestMinimumViableWindowIsDerivedFromTheGuarantee(t *testing.T) {
	minimum := MinimumViableWindow()

	// 它必须装得下那几项之和，而且要留出安全余量。
	needed := constantOverheadTokens + emergencySummaryTokens + lastExchangeTokens
	if int(float64(minimum)*(1-safetyMarginRatio)) < needed {
		t.Errorf("最小可行窗口 %d 的硬边界装不下 %d", minimum, needed)
	}
	// 反过来也不能定得离谱地高——那会把一批本来能用的模型挡在外面。
	if minimum > 32000 {
		t.Errorf("最小可行窗口 %d 太高了，会挡住正常可用的模型", minimum)
	}
}

// 压缩指令必须要求保住"读到哪了"。
//
// 截断 + 分页 + 压缩合起来才让模型能消费任意大的内容：它不断把原文换成结论，
// 字节数不涨而理解在累积。但这个循环有一个致命环节——**压缩会把"读第 1–200 行、
// 第 201–400 行"那些轮次总结掉**，摘要里若没写清游标位置，模型就丢了它，于是
// 要么从头再读（每圈又触发一次压缩，永不收敛），要么谎报完成。
//
// 七节里"未完成 / 阻塞"**能**容纳它，但必须**要求**它——闭环不该靠概率。
func TestSummaryInstructionsDemandProgressCursors(t *testing.T) {
	for name, instructions := range map[string]string{
		"叶子与合并": withLengthBudget(summaryInstructions),
		"终结性兜底": withLengthBudget(finalCollapseInstructions),
	} {
		t.Run(name, func(t *testing.T) {
			// 必须点名"多步操作的进度"这件事，而不只是笼统说"未完成的事项"。
			for _, want := range []string{"进度", "还剩"} {
				if !strings.Contains(instructions, want) {
					t.Errorf("指令里没有要求写出多步操作的进度（缺 %q）", want)
				}
			}
			// 要说清丢了它的后果，否则模型不知道这一条为什么重要。
			if !strings.Contains(instructions, "从头") {
				t.Error("指令里没有说明丢掉进度的后果")
			}
			// **必须要求区分"已拿到结果"和"已发出但结果未知"。**
			//
			// 真机抓到过：一次压缩把"已发出读第 3、4 段的调用"写成了"段 1-4 已通读"，
			// 而实际只读到第 2 段，终考时模型据此给出了两个伪造的标记。
			// 高报比低报危险得多——低报只是重读，高报是漏读且以为做完了。
			if !strings.Contains(instructions, "结果未知") {
				t.Error("指令里没有要求区分「已拿到结果」和「已发出但结果未知」")
			}
		})
	}
}
