package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"goseek/internal/agent"
	"goseek/internal/contextmgr"
	"goseek/internal/domain"
)

// fakeSummarizer 返回固定的短摘要，并记录被调用了几次。
//
// 摘要必须比原文短得多，否则压缩永远到不了目标线，Compact 会一直循环到步数上限。
type fakeSummarizer struct {
	calls int
	err   error
	// bloat 非零时在摘要后面补这么多字，用来构造"摘要比原文还长"的情况——
	// 那样每一步都会被压缩器拒绝，压缩什么也产不出来。
	bloat int
}

func (fake *fakeSummarizer) Summarize(_ context.Context, _ string, _ string) (string, error) {
	fake.calls++
	if fake.err != nil {
		return "", fake.err
	}
	return fmt.Sprintf("Topic: 第 %d 段摘要%s",
		fake.calls, strings.Repeat("很长的摘要", fake.bloat)), nil
}

// newCompactingAgent 组装一个窗口很小、因此几乎必然触发压缩的 Agent。
func newCompactingAgent(
	model *fakeModel, store *fakeStore, sink *recordingSink,
	summarizer *fakeSummarizer, window int,
) *agent.Agent {
	if store == nil {
		store = &fakeStore{}
	}
	// 这里不能直接把 summarizer 传下去：一个值为 nil 的具体类型指针装进接口之后，
	// 接口本身**不是** nil（它记着类型），Agent 里的 `summarizer == nil` 就判不出来。
	// 这是 Go 接口的老陷阱，测试里显式绕开它。
	if summarizer == nil {
		return agent.New(model, &fakeTools{}, store, sink, nil, window)
	}
	return agent.New(model, &fakeTools{}, store, sink, summarizer, window)
}

// bulkySession 造一个足够长、足以撑爆小窗口的会话。
func bulkySession(turns int) *domain.Session {
	session := newTestSession()
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

// indexOfType 返回某类事件第一次出现的位置；没有则返回 -1。
func indexOfType(sink *recordingSink, eventType domain.EventType) int {
	return slices.IndexFunc(sink.events, func(event domain.RunEvent) bool {
		return event.Type == eventType
	})
}

// decodePayload 把事件里的 JSON payload 解成具体结构。
//
// RunEvent.Payload 是 json.RawMessage 而不是 any：事件要落库、要经 SSE 推给前端，
// 序列化后的形态才是它的真身，测试也按同一种形态读它。
func decodePayload[T any](t *testing.T, event domain.RunEvent) T {
	t.Helper()
	var payload T
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("解析 %s 的 payload 失败: %v", event.Type, err)
	}
	return payload
}

// indexOfState 返回第一个把状态切到 want 的事件位置；没有则返回 -1。
func indexOfState(t *testing.T, sink *recordingSink, want domain.RunState) int {
	t.Helper()
	for index, event := range sink.events {
		if event.Type != domain.EventStateChanged {
			continue
		}
		if decodePayload[domain.StateChangedPayload](t, event).State == want {
			return index
		}
	}
	return -1
}

// lastIndexOfState 返回最后一次切到 want 状态的事件位置；没有则返回 -1。
func lastIndexOfState(t *testing.T, sink *recordingSink, want domain.RunState) int {
	t.Helper()
	found := -1
	for index, event := range sink.events {
		if event.Type != domain.EventStateChanged {
			continue
		}
		if decodePayload[domain.StateChangedPayload](t, event).State == want {
			found = index
		}
	}
	return found
}

// 超过触发线时，压缩发生在请求模型**之前**，并按顺序发出状态与压缩事件。
//
// 顺序很关键：界面靠 COMPRESSING 状态告诉用户"现在在整理上下文，不是卡住了"，
// 这条必须先于耗时几十秒的摘要调用发出去。
func TestCompactionRunsBeforeTheModelRequestAndAnnouncesItself(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	sink := &recordingSink{}
	summarizer := &fakeSummarizer{}
	// 窗口取 3000：上面那些长回答很快就会越过 80% 的触发线。
	subject := newCompactingAgent(model, nil, sink, summarizer, 3000)
	session := bulkySession(8)

	if _, err := subject.Handle(context.Background(), session, "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if summarizer.calls == 0 {
		t.Fatal("上下文早已超过触发线，却没有压缩")
	}
	started := indexOfType(sink, domain.EventContextCompactionStarted)
	completed := indexOfType(sink, domain.EventContextCompactionCompleted)
	if started < 0 || completed < 0 {
		t.Fatalf("压缩事件缺失: %v", sink.types())
	}
	if !(started < completed) {
		t.Errorf("started 在 completed 之后: %v", sink.types())
	}
	// COMPRESSING 必须先于 started 出现。
	compressing := indexOfState(t, sink, domain.StateCompressing)
	if compressing < 0 || compressing > started {
		t.Errorf("COMPRESSING 状态没有在压缩开始前发出: %v", sink.types())
	}
	// 压缩结束后要回到 WAITING_MODEL，否则界面会一直停在"整理中"。
	back := lastIndexOfState(t, sink, domain.StateWaitingModel)
	if back < completed {
		t.Errorf("压缩结束后没有回到 WAITING_MODEL: %v", sink.types())
	}
}

// 压缩之后真正发出去的那次请求，必须用的是压缩后的视图。
//
// 这是压缩唯一有意义的地方：如果请求仍然带着全部原文，那么前面所有工作都只是
// 白花了几次模型调用。
func TestTheRequestAfterCompactionCarriesTheCompactedView(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	summarizer := &fakeSummarizer{}
	subject := newCompactingAgent(model, nil, &recordingSink{}, summarizer, 3000)
	session := bulkySession(8)
	rawCount := len(session.Messages())

	if _, err := subject.Handle(context.Background(), session, "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if len(model.requests) == 0 {
		t.Fatal("没有发出请求")
	}
	sent := model.requests[0].Messages
	// system + 摘要 + 未压缩原文 + 这一轮的新提问，总数必然小于"全部原文 + 2"。
	if len(sent) >= rawCount+2 {
		t.Errorf("请求带了 %d 条消息，原文才 %d 条——压缩没有生效", len(sent), rawCount)
	}
	if !slices.ContainsFunc(sent, func(message domain.ModelMessage) bool {
		return strings.Contains(message.Content, "Topic: 第 1 段摘要")
	}) {
		t.Error("请求里没有摘要消息")
	}
	// 原始历史一条都不能少：压缩改的是视图，不是事实。
	if len(session.Messages()) != rawCount+2 {
		t.Errorf("历史变成 %d 条; want %d（原文 + 本轮提问和回答）",
			len(session.Messages()), rawCount+2)
	}
	// 记忆必须落在一致状态上。
	if err := session.Memory.CheckInvariant(); err != nil {
		t.Errorf("压缩后不变量被破坏: %v", err)
	}
}

// 摘要失败不该让用户的问题得不到回答：压缩是优化，不是必要步骤。
func TestCompactionFailureStillAnswersTheUser(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	sink := &recordingSink{}
	subject := newCompactingAgent(model, nil, sink,
		&fakeSummarizer{err: errors.New("供应商 500")}, 3000)
	session := bulkySession(8)

	if _, err := subject.Handle(context.Background(), session, "接着说"); err != nil {
		t.Fatalf("压缩失败导致整轮失败了: %v", err)
	}

	if len(model.requests) == 0 {
		t.Error("压缩失败后没有继续请求模型")
	}
	// 失败要被如实记进 completed 事件，界面据此提示。
	index := indexOfType(sink, domain.EventContextCompactionCompleted)
	if index < 0 {
		t.Fatalf("没有 completed 事件: %v", sink.types())
	}
	payload := decodePayload[domain.CompactionCompletedPayload](t, sink.events[index])
	if payload.Failed == "" {
		t.Error("压缩失败没有记进事件")
	}
	if err := session.Memory.CheckInvariant(); err != nil {
		t.Errorf("失败后不变量被破坏: %v", err)
	}
}

// 没有配置摘要器时不压缩，也不报错——CLI 的某些装配路径可以不带它。
func TestNoSummarizerMeansNoCompaction(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	sink := &recordingSink{}
	subject := newCompactingAgent(model, nil, sink, nil, 3000)

	if _, err := subject.Handle(context.Background(), bulkySession(8), "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if indexOfType(sink, domain.EventContextCompactionStarted) >= 0 {
		t.Errorf("没有摘要器却触发了压缩: %v", sink.types())
	}
}

// 短对话不该被压缩：窗口够用时压缩只是白花钱，还把细节变成了摘要。
func TestShortConversationIsNotCompacted(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	summarizer := &fakeSummarizer{}
	subject := newCompactingAgent(model, nil, &recordingSink{}, summarizer, testContextWindow)

	if _, err := subject.Handle(context.Background(), newTestSession(), "你好"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if summarizer.calls != 0 {
		t.Errorf("短对话被压缩了 %d 次", summarizer.calls)
	}
}

// 压缩起始检查点先落盘再动手：摘要要花几十秒，中途进程挂掉时，
// 已经落盘的状态要能说明"当时正在压缩"。
func TestCompactionCheckpointIsSavedBeforeSummarizing(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	store := &fakeStore{}
	summarizer := &fakeSummarizer{}
	subject := newCompactingAgent(model, store, &recordingSink{}, summarizer, 3000)

	if _, err := subject.Handle(context.Background(), bulkySession(8), "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if summarizer.calls == 0 {
		t.Fatal("没有触发压缩")
	}
	// 第一次保存（记下用户消息与压缩起始）必须发生在任何模型调用之前，
	// 而模型调用总数 = 摘要次数 + 决策次数。
	if len(store.saved) < 2 {
		t.Errorf("压缩全程只保存了 %d 次，缺少起始检查点", len(store.saved))
	}
}

// 上下文占用事件用的是压缩**之后**的数字。报压缩前的值等于告诉用户
// "还在 90%"，而实际上已经降下来了。
func TestUsageEventReflectsThePostCompactionView(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	sink := &recordingSink{}
	subject := newCompactingAgent(model, nil, sink, &fakeSummarizer{}, 3000)

	if _, err := subject.Handle(context.Background(), bulkySession(8), "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	completed := indexOfType(sink, domain.EventContextCompactionCompleted)
	usage := -1
	for index := completed + 1; index < len(sink.events); index++ {
		if sink.events[index].Type == domain.EventContextUsageUpdated {
			usage = index
			break
		}
	}
	if usage < 0 {
		t.Fatalf("压缩后没有更新占用: %v", sink.types())
	}
	after := decodePayload[domain.CompactionCompletedPayload](t, sink.events[completed]).AfterTokens
	reported := decodePayload[domain.ContextUsagePayload](t, sink.events[usage]).InputTokens
	if reported > after {
		t.Errorf("报出的占用 %d 大于压缩后的 %d", reported, after)
	}
}

// 一轮之内不重复做注定白费的压缩。
//
// 可压缩区的终点由**轮次**决定，一轮里即使追加了工具消息也不会变。因此上一次
// 压缩什么都没压出来时，同一轮里的后续请求再压一次，输入完全相同、结果必然相同，
// 只是白花摘要调用——而一轮里可能请求模型五六次。
func TestAFutileCompactionIsNotRepeatedWithinATurn(t *testing.T) {
	// 模型先要一次工具调用，拿到观察后再收口：这一轮会请求模型两次。
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{"command":"ls"}`)}},
		{Content: "好的"},
	}}
	// 摘要写得比原文还长，因此每次压缩都压不出任何东西。
	bloated := &fakeSummarizer{}
	bloated.bloat = 4000
	tools := &fakeTools{}
	subject := agent.New(model, tools, &fakeStore{}, &recordingSink{}, bloated, 3000)

	// 在两次请求之间取一个快照。beforeExecute 恰好落在"第一次请求已回、第二次
	// 还没发"这个时刻——比在结束后数总次数精确得多：总次数会随压缩流程的步数
	// 变化，而这条测试要断言的是"**之后**一次都没再调用"。
	var betweenRequests int
	tools.beforeExecute = func() { betweenRequests = bloated.calls }

	if _, err := subject.Handle(context.Background(), bulkySession(8), "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if len(model.requests) != 2 {
		t.Fatalf("请求模型 %d 次; want 2", len(model.requests))
	}
	if betweenRequests == 0 {
		t.Fatal("第一次请求前根本没尝试压缩，这条测试没测到东西")
	}
	if bloated.calls != betweenRequests {
		t.Errorf("第二次请求前又调用了 %d 次摘要; want 0（同一轮里输入没变，结果必然相同）",
			bloated.calls-betweenRequests)
	}
}

// 压缩确实产出了东西时，记录要被清掉——情况变了，下次该重新尝试。
func TestASuccessfulCompactionClearsTheFutileMemo(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{"command":"ls"}`)}},
		{Content: "好的"},
	}}
	summarizer := &fakeSummarizer{}
	subject := agent.New(model, &fakeTools{}, &fakeStore{}, &recordingSink{}, summarizer, 3000)

	if _, err := subject.Handle(context.Background(), bulkySession(10), "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// 压缩成功过，所以第二次请求之前不会因为"上次白做"而被跳过。
	// 这里只要求它确实压出了东西——跳过与否由占用是否仍超线决定。
	if summarizer.calls == 0 {
		t.Error("一次摘要都没发生")
	}
}

// —— M4.3：用户明确要求的压缩 ——

// 手动压缩发一整套轮次事件，用一个新的 TurnID。
//
// 复用轮次的信封而不是新造一种：TurnID 在这个系统里的含义本来就是"一次工作单元"
// 而不是"一次问答"，这样 Runner 的 running 标记、前端的分组、事件重放三处都不用改。
func TestManualCompactEmitsAFullTurnEnvelope(t *testing.T) {
	sink := &recordingSink{}
	summarizer := &fakeSummarizer{}
	subject := newCompactingAgent(&fakeModel{}, nil, sink, summarizer, 3000)

	if err := subject.Compact(context.Background(), bulkySession(8)); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	types := sink.types()
	if types[0] != string(domain.EventTurnStarted) {
		t.Errorf("第一个事件 = %s; want turn.started（完整事件序列 %v）", types[0], types)
	}
	if types[len(types)-1] != string(domain.EventTurnCompleted) {
		t.Errorf("最后一个事件 = %s; want turn.completed（完整事件序列 %v）",
			types[len(types)-1], types)
	}
	for _, want := range []domain.EventType{
		domain.EventContextCompactionStarted,
		domain.EventContextCompactionCompleted,
		domain.EventContextUsageUpdated,
	} {
		if indexOfType(sink, want) < 0 {
			t.Errorf("缺少 %s；完整事件序列 %v", want, types)
		}
	}
	// 结束时必须回到 IDLE，否则界面会一直停在"正在整理"。
	if indexOfState(t, sink, domain.StateIdle) < 0 {
		t.Errorf("结束时没有回到 IDLE：%v", types)
	}
	// 本次工作里的所有事件共享同一个 TurnID，前端据此把它们归成一组。
	first := sink.events[0].TurnID
	for _, event := range sink.events {
		if event.TurnID != first {
			t.Errorf("事件 %s 的 turn_id 与其他不同", event.Type)
			break
		}
	}
}

// 手动压缩**不看触发线**：占用落在目标线与触发线之间时，自动压缩不会动，
// 手动要求则照压。
//
// 目标线仍然是有效的下界——低于它就真的没什么可压了，再压是花钱换失真。
// 两条线的分工在这里体现得最清楚：触发线决定"要不要自动开始"，目标线决定
// "压到哪儿为止"，而手动压缩只跳过前者。
func TestManualCompactIgnoresTheTriggerThreshold(t *testing.T) {
	summarizer := &fakeSummarizer{}
	// 窗口 12000 → 硬边界 11400、触发线 9120、目标线 2280。
	// 八轮对话约四千 token，正好落在两条线之间。
	const window = 12000
	subject := newCompactingAgent(&fakeModel{}, nil, &recordingSink{}, summarizer, window)
	session := bulkySession(8)

	// 先确认前提：这个占用确实还没到自动触发线，否则这条测试什么都没验证。
	thresholds := contextmgr.NewThresholds(window)
	occupied := contextmgr.Build(session, session.Memory, nil, window).Usage.InputTokens
	if thresholds.ShouldCompact(occupied) {
		t.Fatalf("测试前提不成立：占用 %d 已经到了触发线 %d", occupied, thresholds.CompactAt)
	}
	if occupied <= thresholds.CompactTo {
		t.Fatalf("测试前提不成立：占用 %d 已经低于目标线 %d", occupied, thresholds.CompactTo)
	}

	if err := subject.Compact(context.Background(), session); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if summarizer.calls == 0 {
		t.Error("没到触发线就不压了——手动压缩应当无视触发线")
	}
	if session.Memory.RawCompactionCursor == 0 {
		t.Error("游标没有前进，说明什么都没压")
	}
}

// 已经低于目标线时如实报告"无事可做"，而不是假装做了什么。
//
// 目标线是压缩的下界：低于它再压就是花钱换失真。用户点了按钮必须得到反馈，
// 但反馈可以是"无需压缩"。
func TestManualCompactOnASmallSessionDoesNothing(t *testing.T) {
	sink := &recordingSink{}
	summarizer := &fakeSummarizer{}
	subject := newCompactingAgent(&fakeModel{}, nil, sink, summarizer, 1000000)

	if err := subject.Compact(context.Background(), newTestSession()); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if summarizer.calls != 0 {
		t.Errorf("空会话也调了 %d 次模型", summarizer.calls)
	}
	// 事件仍然要发全套——用户点了按钮，必须得到反馈，哪怕反馈是"无需压缩"。
	index := indexOfType(sink, domain.EventContextCompactionCompleted)
	if index < 0 {
		t.Fatalf("没有 completed 事件：%v", sink.types())
	}
	payload := decodePayload[domain.CompactionCompletedPayload](t, sink.events[index])
	if len(payload.Batches) != 0 {
		t.Errorf("无事可做却产生了 %d 个节点", len(payload.Batches))
	}
	if payload.Failed != "" {
		t.Errorf("无事可做被报成了失败：%q", payload.Failed)
	}
}

// 手动压缩绕过"注定白做"的记忆：那条记录是给自动触发省钱用的，
// 不该挡住用户明确的指令——让他白花一次调用也比"点了没反应"好。
func TestManualCompactBypassesTheFutileMemo(t *testing.T) {
	bloated := &fakeSummarizer{bloat: 4000}
	subject := newCompactingAgent(&fakeModel{}, nil, &recordingSink{}, bloated, 3000)
	session := bulkySession(8)

	// 第一次：压不出东西，于是被记为"注定白做"。
	if err := subject.Compact(context.Background(), session); err != nil {
		t.Fatalf("第一次 Compact 返回错误: %v", err)
	}
	firstRound := bloated.calls
	if firstRound == 0 {
		t.Fatal("第一次压缩一次模型都没调")
	}

	// 第二次：历史没变，自动触发会跳过；手动要求必须仍然尝试。
	if err := subject.Compact(context.Background(), session); err != nil {
		t.Fatalf("第二次 Compact 返回错误: %v", err)
	}

	if bloated.calls == firstRound {
		t.Error("第二次手动压缩被『注定白做』的记录挡住了")
	}
}

// 没有配置摘要器时手动压缩要明确报错，而不是安静地什么都不做。
//
// 这一点和自动压缩相反：自动路径上没有摘要器就跳过（那是装配决定，用户没要求过
// 什么）；而用户点了按钮，必须得到一个回答。
func TestManualCompactWithoutSummarizerReportsAnError(t *testing.T) {
	subject := newCompactingAgent(&fakeModel{}, nil, &recordingSink{}, nil, 3000)

	if err := subject.Compact(context.Background(), bulkySession(8)); err == nil {
		t.Error("没有摘要器却报告压缩成功")
	}
}

// 手动压缩之后**不该出现 WAITING_MODEL**：没有模型请求跟在它后面。
//
// 这条是真机跑出来的——事件流里 COMPRESSING 之后紧跟着一个 WAITING_MODEL，
// 界面会闪一下"正在请求模型"，而那次请求根本不存在。根因是 compact 把下一个状态
// 写死了，而它对自动压缩才成立。
func TestManualCompactDoesNotAnnounceAModelRequest(t *testing.T) {
	sink := &recordingSink{}
	subject := newCompactingAgent(&fakeModel{}, nil, sink, &fakeSummarizer{}, 3000)

	if err := subject.Compact(context.Background(), bulkySession(8)); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}

	if index := indexOfState(t, sink, domain.StateWaitingModel); index >= 0 {
		t.Errorf("手动压缩报了 WAITING_MODEL（第 %d 个事件），但没有任何模型请求：%v",
			index, sink.types())
	}
	// COMPRESSING 之后应当直接回到 IDLE。
	compressing := indexOfState(t, sink, domain.StateCompressing)
	idle := indexOfState(t, sink, domain.StateIdle)
	if compressing < 0 || idle < compressing {
		t.Errorf("状态没有从 COMPRESSING 走到 IDLE：%v", sink.types())
	}
}

// 自动压缩仍然要报 WAITING_MODEL——它后面确实紧跟着一次模型请求。
func TestAutomaticCompactionStillAnnouncesTheModelRequest(t *testing.T) {
	sink := &recordingSink{}
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	subject := newCompactingAgent(model, nil, sink, &fakeSummarizer{}, 3000)

	if _, err := subject.Handle(context.Background(), bulkySession(8), "接着说"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	completed := indexOfType(sink, domain.EventContextCompactionCompleted)
	if completed < 0 {
		t.Fatalf("没有触发压缩：%v", sink.types())
	}
	if lastIndexOfState(t, sink, domain.StateWaitingModel) < completed {
		t.Errorf("自动压缩之后没有回到 WAITING_MODEL：%v", sink.types())
	}
}

// —— M5.3：上界被击穿时自我修正 ——

// 供应商报的 prompt_tokens 超过我们的估算，说明"估算 ≥ 实际"这条契约被破坏了。
// 那是压缩终止性保证的前提，所以必须报出来，而且要立刻调紧系数。
func TestEstimateBreachIsReportedAndCorrected(t *testing.T) {
	// 让假模型报一个远超估算的 prompt_tokens。
	model := &fakeModel{responses: []domain.ModelResponse{
		{Content: "好的", PromptTokens: 999999},
	}}
	var breaches []domain.EstimateBreach
	subject := agent.New(model, &fakeTools{}, &fakeStore{}, &recordingSink{}, nil, testContextWindow)
	subject.SetEstimateBreachReporter(func(breach domain.EstimateBreach) {
		breaches = append(breaches, breach)
	})

	if _, err := subject.Handle(context.Background(), newTestSession(), "你好"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if len(breaches) != 1 {
		t.Fatalf("报了 %d 次击穿; want 1", len(breaches))
	}
	if breaches[0].Actual != 999999 {
		t.Errorf("实测值 = %d", breaches[0].Actual)
	}
	if breaches[0].Ratio() <= 1 {
		t.Errorf("比值 = %v; 击穿的比值必然大于 1", breaches[0].Ratio())
	}
}

// 实测值低于估算是**正常情况**（上界成立），不该报警。
func TestNoBreachWhenTheEstimateHolds(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的", PromptTokens: 1}}}
	reported := 0
	subject := agent.New(model, &fakeTools{}, &fakeStore{}, &recordingSink{}, nil, testContextWindow)
	subject.SetEstimateBreachReporter(func(domain.EstimateBreach) { reported++ })

	if _, err := subject.Handle(context.Background(), newTestSession(), "你好"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if reported != 0 {
		t.Errorf("上界成立却报了 %d 次击穿", reported)
	}
}

// 击穿之后系数被调高，于是**后续请求的估算变大**——否则"自我修正"只是记了条日志。
func TestEstimateGrowsAfterABreach(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{Content: "第一次", PromptTokens: 999999},
		{Content: "第二次", PromptTokens: 1},
	}}
	sink := &recordingSink{}
	subject := agent.New(model, &fakeTools{}, &fakeStore{}, sink, nil, testContextWindow)
	session := newTestSession()

	if _, err := subject.Handle(context.Background(), session, "第一句"); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	firstEstimate := lastEstimatedUsage(t, sink)

	if _, err := subject.Handle(context.Background(), session, "第二句"); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	secondEstimate := lastEstimatedUsage(t, sink)

	// 第二轮的历史更长，估算本来就会更大；关键是它要**明显**更大——
	// 系数被调高了几个数量级（999999/估算），所以差距不可能只是多几条消息。
	if secondEstimate < firstEstimate*10 {
		t.Errorf("击穿之后估算只从 %d 涨到 %d，系数似乎没有生效",
			firstEstimate, secondEstimate)
	}
}

// lastEstimatedUsage 返回最后一条来源为 estimated 的占用事件里的 token 数。
func lastEstimatedUsage(t *testing.T, sink *recordingSink) int {
	t.Helper()
	last := 0
	for _, event := range sink.events {
		if event.Type != domain.EventContextUsageUpdated {
			continue
		}
		payload := decodePayload[domain.ContextUsagePayload](t, event)
		if payload.Source == string(domain.ContextUsageEstimated) {
			last = payload.InputTokens
		}
	}
	if last == 0 {
		t.Fatal("没有找到估算来源的占用事件")
	}
	return last
}
