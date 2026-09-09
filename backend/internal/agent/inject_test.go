// 运行中注入消息的行为。
//
// 注入解决的是这样一件事：一轮跑起来之后，用户看出方向不对，想补一句"不用管
// 那个目录"。以前他只能等——等模型跑完一整串命令，把已经错了的路走完。
//
// 这里钉住四条：注入发生在**请求模型之前**（否则这一次请求看不到它，用户会觉得
// 自己那句话被吞了）、落库在请求之前、共享当前 TurnID、按提交顺序追加。

package agent_test

import (
	"context"
	"strings"
	"testing"

	agentpkg "goseek/internal/agent"
	"goseek/internal/domain"
)

// queueInjector 是一个最小的 Injector：交出一批就清空。
//
// 清空这件事很关键——Pending 的语义是"取走"，不是"看一眼"。不清空的话每次请求
// 都会把同一句话再追加一遍，历史里会堆出几十条重复的用户消息。真实实现
// （httpapi.Runner）也是这个语义。
type queueInjector struct {
	queued []agentpkg.PendingMessage
	// calls 记录被取走了几次，用来确认"取走即清空"。
	calls int
}

func (injector *queueInjector) Pending() []agentpkg.PendingMessage {
	injector.calls++
	taken := injector.queued
	injector.queued = nil
	return taken
}

func TestInjectedMessagesReachTheModelInTheSameRequest(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的，跳过那个目录。"}}}
	agent := newAgent(model, nil, nil, nil)
	agent.SetInjector(&queueInjector{queued: []agentpkg.PendingMessage{{Content: "不用管 node_modules"}}})

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "统计一下代码行数"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// 关键断言：**第一次**请求就带上了注入的内容。如果注入点放在请求之后，
	// 这句话要等到下一次工具调用回来才生效，用户看着模型继续跑错的方向。
	if len(model.requests) == 0 {
		t.Fatal("模型没有被请求")
	}
	first := model.requests[0].Messages
	last := first[len(first)-1]
	if last.Role != domain.ModelRoleUser || last.Content != "不用管 node_modules" {
		t.Fatalf("注入的消息没有出现在第一次请求的末尾，最后一条是 %s/%q", last.Role, last.Content)
	}
}

func TestAmbientContextIsVisibleButNeverPersisted(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "看到了。"}}}
	sink := &recordingSink{}
	agent := newAgent(model, nil, nil, sink)
	session := newTestSession()

	if _, err := agent.HandleWithAmbient(
		context.Background(),
		session,
		"分析当前页面",
		"[发送时侧边栏]\n- 页面：https://example.com",
	); err != nil {
		t.Fatalf("HandleWithAmbient 返回错误: %v", err)
	}

	if len(model.requests) != 1 {
		t.Fatalf("模型请求数 = %d; want 1", len(model.requests))
	}
	last := model.requests[0].Messages[len(model.requests[0].Messages)-1].Content
	if !strings.Contains(last, "https://example.com") || !strings.Contains(last, "分析当前页面") {
		t.Fatalf("模型没有同时看到侧栏与用户原文: %q", last)
	}
	history := session.Messages()
	if len(history) == 0 || history[0].Content != "分析当前页面" {
		t.Fatalf("历史正文被污染: %+v", history)
	}
	for _, event := range sink.events {
		if event.Type == domain.EventUserMessage && strings.Contains(string(event.Payload), "example.com") {
			t.Fatalf("user.message 事件被侧栏上下文污染: %s", event.Payload)
		}
	}
}

func TestInjectedMessageIsPersistedBeforeTheModelCall(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "收到。"}}}
	store := &fakeStore{}
	agent := newAgent(model, nil, store, nil)
	agent.SetInjector(&queueInjector{queued: []agentpkg.PendingMessage{{Content: "改用 rg"}}})

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "搜一下 TODO"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// 检查点纪律：任何外部调用之前，已经发生的事实必须先落库。注入的消息也是事实
	// ——进程如果在模型请求中途挂掉，这句话不能凭空消失（用户以为自己说过了）。
	//
	// fakeStore 记的是每次保存时的**角色序列**。第一次保存是开场（只有 user），
	// 第二次就应该是注入之后、请求模型之前的那次，历史里已经有两条 user。
	if len(store.saved) < 2 {
		t.Fatalf("保存次数不足，只有 %v", store.saved)
	}
	if store.saved[1] != "user,user" {
		t.Fatalf("第二次落库时的历史是 %q，期望注入已经在里面（user,user）", store.saved[1])
	}
}

func TestInjectedMessagesShareTheRunningTurnID(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好。"}}}
	agent := newAgent(model, nil, nil, nil)
	agent.SetInjector(&queueInjector{queued: []agentpkg.PendingMessage{{Content: "顺便看看 go.mod"}}})

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "看看项目结构"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// TurnID 的含义是"一个工作单元"，注入是对这个单元的引导而不是另起一件事。
	// 给它新开一个 TurnID 会把一次连续的工作在对话流里切成两段，压缩时的轮次
	// 边界也跟着变碎。
	//
	// TurnID 由 Handle 自己生成，测试拿不到它，所以断言"全都一样"而不是"等于某值"
	// ——要表达的本来就是同一性，不是某个具体的值。
	messages := session.Messages()
	if len(messages) < 3 {
		t.Fatalf("历史太短: %v", contentsOf(messages))
	}
	turnID := messages[0].TurnID
	if turnID == "" {
		t.Fatal("首条消息没有 TurnID")
	}
	for _, message := range messages {
		if message.TurnID != turnID {
			t.Fatalf("消息 %q 的 TurnID 是 %q，期望和本轮一致的 %q",
				message.Content, message.TurnID, turnID)
		}
	}
}

func TestInjectedMessagesKeepSubmissionOrder(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "都记下了。"}}}
	agent := newAgent(model, nil, nil, nil)
	agent.SetInjector(&queueInjector{queued: []agentpkg.PendingMessage{
		{Content: "第一句"}, {Content: "第二句"}, {Content: "第三句"},
	}})

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "开始"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// 顺序是有意义的：后一句往往在修正前一句（"用 rg" → "算了还是用 grep"）。
	// 乱序会让模型照着被撤销的那条执行。
	got := strings.Join(contentsOf(session.Messages()), "|")
	if !strings.Contains(got, "开始|第一句|第二句|第三句") {
		t.Fatalf("注入顺序不对: %s", got)
	}
}

func TestInjectedMessagesAreEmittedAsUserMessageEvents(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的。"}}}
	sink := &recordingSink{}
	agent := newAgent(model, nil, nil, sink)
	agent.SetInjector(&queueInjector{queued: []agentpkg.PendingMessage{{Content: "停一下，先看日志"}}})

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "跑一下测试"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// 没有事件的话，面板上那条乐观显示的"已排队"气泡永远等不到真身，
	// 会一直挂着——直到刷新页面才变成真实消息。
	// 没有事件的话，面板上那条乐观显示的"已排队"气泡永远等不到真身，
	// 会一直挂着——直到刷新页面才变成真实消息。
	found := false
	for _, event := range sink.events {
		if event.Type == domain.EventUserMessage &&
			strings.Contains(string(event.Payload), "停一下，先看日志") {
			found = true
		}
	}
	if !found {
		t.Fatal("注入的消息没有产生 user.message 事件")
	}
}

func TestNothingQueuedMeansNothingChanges(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "答复。"}}}
	injector := &queueInjector{}
	agent := newAgent(model, nil, nil, nil)
	agent.SetInjector(injector)

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "问题"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	// 队列空时不能产生任何多余的写：每次请求模型前都会问一次队列，
	// 要是空队列也触发一次落库，一轮十次工具调用就是十次无谓的写。
	if injector.calls == 0 {
		t.Fatal("根本没问过队列")
	}
	if historyRolesOf(session) != "user,assistant" {
		t.Fatalf("历史被改动了: %s", historyRolesOf(session))
	}
}

func TestNoInjectorMeansNoInjection(t *testing.T) {
	// 注入是可选能力：终端里跑的 Agent 没有面板，也就没有队列。
	// 不设 Injector 时一切照旧，而不是空指针。
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "答复。"}}}
	agent := newAgent(model, nil, nil, nil)

	session := newTestSession()

	if _, err := agent.Handle(context.Background(), session, "问题"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}
	if historyRolesOf(session) != "user,assistant" {
		t.Fatalf("历史不对: %s", historyRolesOf(session))
	}
}

func TestInjectionHappensAgainBeforeEveryModelRequest(t *testing.T) {
	// 一轮里可能有十几次工具调用，每次调用之后都会再请求模型。用户在**任何**
	// 一个间隙说的话都该在下一次请求里生效，而不是只有轮次开头那一次。
	model := &fakeModel{responses: []domain.ModelResponse{
		{Content: "先看看", ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{"command":"ls"}`)}},
		{Content: "看完了。"},
	}}
	tools := &fakeTools{results: []domain.ToolResult{{Status: domain.ToolSuccess, Content: "a.go"}}}
	injector := &queueInjector{}
	agent := newAgent(model, tools, nil, nil)
	agent.SetInjector(injector)

	session := newTestSession()

	// 在第一次请求之后、第二次请求之前把话塞进队列。真实场景里这是由 HTTP 处理器
	// 在另一个 goroutine 里做的；这里用 fakeTools 的执行时机来模拟那个时间点。
	tools.beforeExecute = func() {
		injector.queued = append(injector.queued, agentpkg.PendingMessage{Content: "别看隐藏文件"})
	}

	if _, err := agent.Handle(context.Background(), session, "看看目录"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if len(model.requests) < 2 {
		t.Fatalf("模型只被请求了 %d 次", len(model.requests))
	}
	second := model.requests[1].Messages
	last := second[len(second)-1]
	if last.Content != "别看隐藏文件" {
		t.Fatalf("第二次请求末尾是 %q，期望注入的那句", last.Content)
	}
}

// contentsOf 取出一串消息的正文，便于把顺序拼成一行来断言。
func contentsOf(messages []domain.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}
