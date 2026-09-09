package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"goseek/internal/agent"
	"goseek/internal/domain"
)

// newTestSession 创建一个身份固定的空会话，让测试不依赖随机 ID。
func newTestSession() *domain.Session {
	return domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp/goseek-test")
}

// fakeModel 按顺序给出预设响应，并记录每次收到的请求。
type fakeModel struct {
	responses []domain.ModelResponse
	err       error
	requests  []domain.ModelRequest
	// deltas 在每次调用时按顺序交给 onDelta，模拟流式输出。
	deltas []domain.TextDelta
}

// Complete 记录请求并返回下一个预设响应。
func (fake *fakeModel) Complete(_ context.Context, request domain.ModelRequest, onDelta domain.DeltaFunc) (domain.ModelResponse, error) {
	captured := domain.ModelRequest{
		Messages: append([]domain.ModelMessage(nil), request.Messages...),
		Tools:    append([]domain.ToolSpec(nil), request.Tools...),
	}
	fake.requests = append(fake.requests, captured)

	if fake.err != nil {
		return domain.ModelResponse{}, fake.err
	}
	if len(fake.requests) > len(fake.responses) {
		return domain.ModelResponse{}, errors.New("fakeModel: 预设响应已用尽")
	}
	if len(fake.requests) == 1 {
		for _, delta := range fake.deltas {
			onDelta(delta)
		}
	}
	return fake.responses[len(fake.requests)-1], nil
}

// fakeTools 记录执行过的调用，并按顺序返回预设观察。
type fakeTools struct {
	results  []domain.ToolResult
	executed []domain.ToolCall
	// outputs 在执行时按顺序交给 onOutput，模拟命令的流式输出。
	outputs []string
	// beforeExecute 在每次执行前调用。它给测试提供了一个"在一轮的中途插手"的
	// 时间点——真实场景里这个时刻由 HTTP 处理器在另一个 goroutine 里制造
	// （用户在命令还在跑的时候又说了一句话），这里用同步回调代替，
	// 免得测试要靠 sleep 去猜时序。
	beforeExecute func()
}

// Specs 返回一个固定的工具定义，用于确认请求确实带上了工具。
func (fake *fakeTools) Specs() []domain.ToolSpec {
	return []domain.ToolSpec{{Name: "bash", Description: "运行命令"}}
}

// Describe 返回一个可辨认的固定标题。
func (fake *fakeTools) Describe(call domain.ToolCall) string {
	return "执行 " + call.Name
}

// Execute 记录调用并返回配对好的预设观察。
func (fake *fakeTools) Execute(_ context.Context, call domain.ToolCall, onOutput domain.OutputFunc) domain.ToolResult {
	if fake.beforeExecute != nil {
		fake.beforeExecute()
	}
	fake.executed = append(fake.executed, call)
	for _, chunk := range fake.outputs {
		onOutput(chunk)
	}

	result := domain.ToolResult{Status: domain.ToolSuccess, Content: "默认观察"}
	if index := len(fake.executed) - 1; index < len(fake.results) {
		result = fake.results[index]
	}
	result.ToolCallID = call.ID
	result.Name = call.Name
	return result
}

// fakeStore 记录每次保存时的历史快照，并可以在第 N 次保存时失败。
type fakeStore struct {
	// saved 是每次保存时历史的角色序列，用来断言"外部调用之前已经落盘"。
	saved []string
	// pending 是每次保存时的 PendingToolCallID。
	pending []string
	// failAt 是要失败的保存次序（从 1 开始）；0 表示从不失败。
	failAt int
	// nextSequence 模拟持久化层的序号分配。
	nextSequence int64
	// transientReachedStore 记录是否有 transient 事件被错误地送进了保存路径。
	transientReachedStore bool
}

// Save 记录快照，必要时返回预设的失败。
func (fake *fakeStore) Save(session *domain.Session, events []domain.RunEvent) ([]domain.RunEvent, error) {
	if fake.failAt != 0 && len(fake.saved)+1 == fake.failAt {
		fake.saved = append(fake.saved, "<失败>")
		fake.pending = append(fake.pending, session.PendingToolCallID)
		return nil, errors.New("磁盘写满了")
	}
	fake.saved = append(fake.saved, historyRolesOf(session))
	fake.pending = append(fake.pending, session.PendingToolCallID)

	// 模拟真实 Store：只有 durable 事件落库并分配序号，transient 的不该走到这里。
	var persisted []domain.RunEvent
	for _, event := range events {
		if !event.Durable() {
			fake.transientReachedStore = true
			continue
		}
		fake.nextSequence++
		event.Sequence = fake.nextSequence
		persisted = append(persisted, event)
	}
	return persisted, nil
}

// recordingSink 记录事件的发生顺序与序号，是 EventSink 的测试实现。
type recordingSink struct {
	events []domain.RunEvent
}

// Emit 记下一个事件。
func (sink *recordingSink) Emit(event domain.RunEvent) {
	sink.events = append(sink.events, event)
}

// types 返回收到的事件类型序列，便于整体断言。
func (sink *recordingSink) types() []string {
	types := make([]string, 0, len(sink.events))
	for _, event := range sink.events {
		types = append(types, string(event.Type))
	}
	return types
}

// durableSequences 返回收到的 durable 事件的序号。
func (sink *recordingSink) durableSequences() []int64 {
	var sequences []int64
	for _, event := range sink.events {
		if event.Durable() {
			sequences = append(sequences, event.Sequence)
		}
	}
	return sequences
}

// toolCall 构造一次带参数的工具调用。
func toolCall(id, name, arguments string) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(arguments)}
}

// testContextWindow 是测试里假定的上下文窗口。给一个具体值，占用事件才有比例可算。
//
// 取得足够大，让这些测试里的短对话不会触发压缩——压缩本身在 contextmgr 的测试里
// 单独覆盖，混进来只会让这里的事件序列断言变得难读。
const testContextWindow = 100000

// newAgent 组装一个使用假依赖的 Agent。传 nil 表示用默认的假实现。
func newAgent(model *fakeModel, tools *fakeTools, store *fakeStore, sink *recordingSink) *agent.Agent {
	if tools == nil {
		tools = &fakeTools{}
	}
	if store == nil {
		store = &fakeStore{}
	}
	if sink == nil {
		sink = &recordingSink{}
	}
	return agent.New(model, tools, store, sink, nil, testContextWindow)
}

// rolesOf 返回一次请求中各条消息的角色序列。
func rolesOf(view []domain.ModelMessage) string {
	roles := make([]string, 0, len(view))
	for _, message := range view {
		roles = append(roles, string(message.Role))
	}
	return strings.Join(roles, ",")
}

// historyRolesOf 返回会话历史的角色序列。
func historyRolesOf(session *domain.Session) string {
	roles := make([]string, 0)
	for _, message := range session.Messages() {
		roles = append(roles, string(message.Role))
	}
	return strings.Join(roles, ",")
}

// 这是 M1 的核心断言：工具执行后必须再次请求模型，由模型读过观察后收口本轮。
func TestHandleRunsToolThenAsksModelAgainBeforeFinishing(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{Content: "我先看看这个目录", ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{"command":"ls"}`)}},
		{Content: "目录里有 3 个文件。"},
	}}
	tools := &fakeTools{results: []domain.ToolResult{
		{Status: domain.ToolSuccess, Content: "a\nb\nc"},
	}}
	sink := &recordingSink{}
	session := newTestSession()

	reply, err := newAgent(model, tools, nil, sink).Handle(context.Background(), session, "看看当前目录")
	if err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if reply != "目录里有 3 个文件。" {
		t.Errorf("最终回复 = %q", reply)
	}
	if len(model.requests) != 2 {
		t.Fatalf("模型被请求 %d 次; want 2", len(model.requests))
	}
	if len(tools.executed) != 1 || tools.executed[0].ID != "call-1" {
		t.Fatalf("执行的工具调用 = %+v; want 一次 call-1", tools.executed)
	}

	// 第二次请求必须带上 assistant 的调用和与之配对的 tool 观察。
	if got := rolesOf(model.requests[1].Messages); got != "system,user,assistant,tool" {
		t.Errorf("第二次请求的角色序列 = %q; want system,user,assistant,tool", got)
	}
	if got := historyRolesOf(session); got != "user,assistant,tool,assistant" {
		t.Errorf("历史角色序列 = %q; want user,assistant,tool,assistant", got)
	}

	// 一轮的完整事件序列。它是 Agent 对外唯一的过程表达，终端和将来的前端
	// 消费的是同一份东西，因此这里整体钉住。
	// context.usage.updated 在每次请求模型之前产生一条（估算值）。fakeModel 不报
	// PromptTokens，所以没有随后的实测值那一条。
	wantEvents := []string{
		"turn.started", "user.message", "state.changed",
		"context.usage.updated", "assistant.message", "state.changed",
		"tool.started", "tool.resolved", "state.changed",
		"context.usage.updated", "assistant.message", "turn.completed", "state.changed",
	}
	if strings.Join(sink.types(), "|") != strings.Join(wantEvents, "|") {
		t.Errorf("事件序列 =\n  %v\nwant\n  %v", sink.types(), wantEvents)
	}

	// durable 事件的序号必须连续，重放才不会缺段。
	for index, sequence := range sink.durableSequences() {
		if sequence != int64(index+1) {
			t.Errorf("第 %d 个 durable 事件的序号 = %d; want %d", index+1, sequence, index+1)
		}
	}
}

// 每次请求都要带上工具定义，否则模型无从发起调用。
func TestHandleSendsToolSpecsOnEveryRequest(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}}
	if _, err := newAgent(model, nil, nil, nil).Handle(context.Background(), newTestSession(), "你好"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if len(model.requests[0].Tools) != 1 || model.requests[0].Tools[0].Name != "bash" {
		t.Errorf("请求携带的工具 = %+v; want 一个 bash", model.requests[0].Tools)
	}
}

// 带文字的响应只要还有工具调用，就只是过程说明，不能结束本轮。
func TestHandleDoesNotFinishOnTextThatComesWithToolCalls(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{Content: "这就去查", ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
		{Content: "查完了。"},
	}}

	reply, err := newAgent(model, nil, nil, nil).Handle(context.Background(), newTestSession(), "查一下")
	if err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}
	if reply == "这就去查" {
		t.Error("带工具调用的过程说明被当成了最终回复")
	}
	if reply != "查完了。" {
		t.Errorf("最终回复 = %q; want %q", reply, "查完了。")
	}
}

// 同一响应里的多个调用按模型给出的顺序串行执行，每个都得到配对的观察。
func TestHandleExecutesMultipleToolCallsInOrder(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{
			toolCall("call-1", "bash", `{"command":"pwd"}`),
			toolCall("call-2", "bash", `{"command":"ls"}`),
		}},
		{Content: "都看过了。"},
	}}
	tools := &fakeTools{}
	session := newTestSession()

	if _, err := newAgent(model, tools, nil, nil).Handle(context.Background(), session, "看看"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if len(tools.executed) != 2 || tools.executed[0].ID != "call-1" || tools.executed[1].ID != "call-2" {
		t.Fatalf("执行顺序 = %+v; want call-1、call-2", tools.executed)
	}
	if got := historyRolesOf(session); got != "user,assistant,tool,tool,assistant" {
		t.Errorf("历史角色序列 = %q; want user,assistant,tool,tool,assistant", got)
	}

	// 每个调用恰好对应一条同 id 的 tool 消息。
	paired := map[string]int{}
	for _, message := range session.Messages() {
		if message.Role == domain.RoleTool {
			paired[message.ToolCallID]++
		}
	}
	if paired["call-1"] != 1 || paired["call-2"] != 1 {
		t.Errorf("配对情况 = %v; want 每个调用恰好一条观察", paired)
	}
}

// 工具失败是模型可读的观察，不是本轮失败：Agent 必须把它交回模型继续决策。
func TestHandleFeedsToolErrorBackToModelInsteadOfFailing(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "python", `{}`)}},
		{Content: "那个工具不存在，我换个方式。"},
	}}
	tools := &fakeTools{results: []domain.ToolResult{
		{Status: domain.ToolError, Content: "不存在名为 python 的工具"},
	}}
	session := newTestSession()

	reply, err := newAgent(model, tools, nil, nil).Handle(context.Background(), session, "跑个脚本")
	if err != nil {
		t.Fatalf("工具失败让本轮失败了: %v", err)
	}
	if reply != "那个工具不存在，我换个方式。" {
		t.Errorf("最终回复 = %q", reply)
	}

	var observation string
	for _, message := range session.Messages() {
		if message.Role == domain.RoleTool {
			observation = message.Content
		}
	}
	if !strings.Contains(observation, `"status":"error"`) {
		t.Errorf("tool 消息没有保留 error 状态: %q", observation)
	}
}

// id 有问题的调用无法配对，本轮必须在写入历史之前就停下，不留悬空调用。
func TestHandleRejectsUnusableToolCallIDsWithoutRecordingThem(t *testing.T) {
	cases := []struct {
		name      string
		responses []domain.ModelResponse
		history   string
	}{
		{
			name:      "id 为空",
			responses: []domain.ModelResponse{{ToolCalls: []domain.ToolCall{toolCall("", "bash", `{}`)}}},
			history:   "user",
		},
		{
			name: "同一响应内 id 重复",
			responses: []domain.ModelResponse{{ToolCalls: []domain.ToolCall{
				toolCall("call-1", "bash", `{}`),
				toolCall("call-1", "bash", `{}`),
			}}},
			history: "user",
		},
		{
			name: "id 与历史中已有的调用冲突",
			responses: []domain.ModelResponse{
				{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
				{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
			},
			history: "user,assistant,tool",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model := &fakeModel{responses: testCase.responses}
			session := newTestSession()

			_, err := newAgent(model, nil, nil, nil).Handle(context.Background(), session, "做点什么")
			if err == nil {
				t.Fatal("无法配对的工具调用没有让本轮失败")
			}
			if got := historyRolesOf(session); got != testCase.history {
				t.Errorf("历史 = %q; want %q（不能留下没有观察的调用）", got, testCase.history)
			}
		})
	}
}

// 既无文字也无工具调用的响应推进不了本轮，属于协议错误。
func TestHandleFailsOnEmptyResponse(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "   "}}}
	session := newTestSession()

	if _, err := newAgent(model, nil, nil, nil).Handle(context.Background(), session, "你好"); err == nil {
		t.Fatal("空响应没有让本轮失败")
	}
	if got := historyRolesOf(session); got != "user" {
		t.Errorf("历史 = %q; want user", got)
	}
}

// system 指令属于视图，不属于会话事实。
func TestHandleKeepsSystemPromptOutOfHistory(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "回复"}}}
	session := newTestSession()

	if _, err := newAgent(model, nil, nil, nil).Handle(context.Background(), session, "提问"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	systemPrompt := model.requests[0].Messages[0].Content
	for _, message := range session.Messages() {
		if message.Content == systemPrompt {
			t.Fatal("system 指令被写进了会话历史")
		}
	}
}

// 多轮之间历史持续累积，模型能看到之前每一轮。
func TestHandleSendsGrowingHistoryAcrossTurns(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{Content: "你好，我是 GoSeek"},
		{Content: "你刚才问我是谁"},
	}}
	subject := newAgent(model, nil, nil, nil)
	session := newTestSession()

	if _, err := subject.Handle(context.Background(), session, "你是谁"); err != nil {
		t.Fatalf("第一轮返回错误: %v", err)
	}
	if _, err := subject.Handle(context.Background(), session, "我刚才问了什么"); err != nil {
		t.Fatalf("第二轮返回错误: %v", err)
	}

	if got := rolesOf(model.requests[0].Messages); got != "system,user" {
		t.Errorf("第一轮请求角色 = %q; want system,user", got)
	}
	if got := rolesOf(model.requests[1].Messages); got != "system,user,assistant,user" {
		t.Errorf("第二轮请求角色 = %q; want system,user,assistant,user", got)
	}
}

// 模型调用失败时：用户消息保留（它已经发生了），assistant 消息不写入（它从未被生成）。
func TestHandleKeepsUserMessageAndWritesNoReplyOnModelFailure(t *testing.T) {
	failure := errors.New("供应商 500")
	session := newTestSession()

	reply, err := newAgent(&fakeModel{err: failure}, nil, nil, nil).Handle(context.Background(), session, "你是谁")
	if err == nil {
		t.Fatalf("Handle 返回 %q; want error", reply)
	}
	if !errors.Is(err, failure) {
		t.Errorf("错误链丢失原始错误: %v", err)
	}
	if reply != "" {
		t.Errorf("失败时返回了回复 %q", reply)
	}
	if got := historyRolesOf(session); got != "user" {
		t.Errorf("失败后历史 = %q; want user", got)
	}
}

// 一轮中途失败时，已经完成的工具调用和观察都留在历史里——它们真的发生过。
func TestHandleKeepsCompletedToolObservationsWhenTurnLaterFails(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
	}}
	session := newTestSession()

	// 第二次请求会因为"预设响应已用尽"而失败。
	if _, err := newAgent(model, nil, nil, nil).Handle(context.Background(), session, "做点什么"); err == nil {
		t.Fatal("第二次模型请求失败时本轮没有失败")
	}
	if got := historyRolesOf(session); got != "user,assistant,tool" {
		t.Errorf("历史 = %q; want user,assistant,tool（已完成的观察必须保留）", got)
	}
}

// 这是 M2 的核心断言：每一次外部调用之前，历史都已经落盘。
// 保存的时机就是"用户消息之后、模型响应之后、每条观察之后"。
func TestHandleSavesBeforeEveryExternalCall(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{
			toolCall("call-1", "bash", `{}`),
			toolCall("call-2", "bash", `{}`),
		}},
		{Content: "都看过了。"},
	}}
	store := &fakeStore{}

	if _, err := newAgent(model, nil, store, nil).Handle(context.Background(), newTestSession(), "看看"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	want := []string{
		"user",                               // 请求模型之前
		"user,assistant",                     // 执行 call-1 之前
		"user,assistant,tool",                // 执行 call-2 之前
		"user,assistant,tool,tool",           // 再次请求模型之前
		"user,assistant,tool,tool,assistant", // 最终回复
	}
	if strings.Join(store.saved, "|") != strings.Join(want, "|") {
		t.Errorf("保存时的历史序列 =\n  %v\nwant\n  %v", store.saved, want)
	}
}

// pending 必须在执行之前就写下去，并在拿到观察后指向下一个调用、最后清空。
func TestHandleRecordsPendingToolCallBeforeExecuting(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{
			toolCall("call-1", "bash", `{}`),
			toolCall("call-2", "bash", `{}`),
		}},
		{Content: "完成"},
	}}
	store := &fakeStore{}
	session := newTestSession()

	if _, err := newAgent(model, nil, store, nil).Handle(context.Background(), session, "看看"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	want := []string{"", "call-1", "call-2", "", ""}
	if strings.Join(store.pending, "|") != strings.Join(want, "|") {
		t.Errorf("每次保存时的 pending = %v; want %v", store.pending, want)
	}
	if session.PendingToolCallID != "" {
		t.Errorf("本轮结束后 pending 仍是 %q", session.PendingToolCallID)
	}
}

// 保存失败必须阻止下一次外部调用：磁盘上没有记录的事实不能继续往下做。
func TestHandleStopsAtSaveFailureBeforeMakingExternalCalls(t *testing.T) {
	cases := []struct {
		name        string
		failAt      int
		wantModel   int
		wantExecute int
	}{
		{"保存用户消息失败则不请求模型", 1, 0, 0},
		{"保存模型响应失败则不执行工具", 2, 1, 0},
		{"保存第一条观察失败则不执行第二个调用", 3, 1, 1},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model := &fakeModel{responses: []domain.ModelResponse{
				{ToolCalls: []domain.ToolCall{
					toolCall("call-1", "bash", `{}`),
					toolCall("call-2", "bash", `{}`),
				}},
				{Content: "完成"},
			}}
			tools := &fakeTools{}
			store := &fakeStore{failAt: testCase.failAt}

			_, err := newAgent(model, tools, store, nil).Handle(context.Background(), newTestSession(), "看看")
			if err == nil {
				t.Fatal("保存失败没有让本轮终止")
			}
			if len(model.requests) != testCase.wantModel {
				t.Errorf("模型被请求 %d 次; want %d", len(model.requests), testCase.wantModel)
			}
			if len(tools.executed) != testCase.wantExecute {
				t.Errorf("执行了 %d 个工具调用; want %d", len(tools.executed), testCase.wantExecute)
			}
		})
	}
}

// 最终回复保存失败时不能假装成功：下次启动那条回复确实不在历史里。
func TestHandleReportsFailureWhenFinalReplyCannotBeSaved(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "答案是 42"}}}
	store := &fakeStore{failAt: 2}

	reply, err := newAgent(model, nil, store, nil).Handle(context.Background(), newTestSession(), "问题")
	if err == nil {
		t.Fatal("最终回复保存失败时返回了成功")
	}
	if reply != "" {
		t.Errorf("保存失败却返回了回复 %q", reply)
	}
	if !strings.Contains(err.Error(), "没有被记录") {
		t.Errorf("错误信息 = %q; want 说明这条回复没有被记录", err.Error())
	}
}

// 自洽的会话不需要修复，也不应该产生任何写入。
func TestRestoreLeavesConsistentSessionUntouched(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "你好"})
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "你好"})
	store := &fakeStore{}

	repaired, err := newAgent(&fakeModel{}, nil, store, nil).Restore(session)
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if len(repaired) != 0 {
		t.Errorf("自洽会话被修复了 %d 处: %+v", len(repaired), repaired)
	}
	if len(store.saved) != 0 {
		t.Errorf("自洽会话触发了 %d 次保存", len(store.saved))
	}
}

// 中断修复的核心：正在执行的调用得到"结果未知"，尚未开始的得到"没有执行"，
// 修完之后历史里不能再有悬空调用。
func TestRestoreDistinguishesInterruptedFromNeverStarted(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "删掉临时文件"})
	session.Append(domain.Message{
		Role: domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{
			toolCall("call-1", "bash", `{"command":"rm -rf /tmp/x"}`),
			toolCall("call-2", "bash", `{"command":"ls /tmp"}`),
		},
	})
	// 进程死在 call-1 执行期间：它被声明为 pending，且两个调用都还没有观察。
	session.PendingToolCallID = "call-1"
	store := &fakeStore{}

	repaired, err := newAgent(&fakeModel{}, nil, store, nil).Restore(session)
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}

	if len(repaired) != 2 {
		t.Fatalf("修复了 %d 处; want 2", len(repaired))
	}
	if repaired[0].ToolCallID != "call-1" || repaired[0].Status != domain.ToolError {
		t.Errorf("call-1 的观察 = %+v", repaired[0])
	}
	if !strings.Contains(repaired[0].Content, "无法确定") {
		t.Errorf("正在执行的调用没有被说成结果未知: %q", repaired[0].Content)
	}
	if !strings.Contains(repaired[1].Content, "没有执行") {
		t.Errorf("尚未开始的调用没有被说成未执行: %q", repaired[1].Content)
	}

	if open := session.UnresolvedToolCalls(); len(open) != 0 {
		t.Errorf("修复后仍有 %d 个悬空调用", len(open))
	}
	if session.PendingToolCallID != "" {
		t.Errorf("修复后 pending 仍是 %q", session.PendingToolCallID)
	}
	if len(store.saved) != 1 {
		t.Errorf("修复保存了 %d 次; want 1", len(store.saved))
	}
}

// 修复绝不能重新执行命令——它可能是 rm -rf，重放的代价不可逆。
func TestRestoreNeverReExecutesTheInterruptedCommand(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "删掉临时文件"})
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{"command":"rm -rf /tmp/x"}`)},
	})
	session.PendingToolCallID = "call-1"
	tools := &fakeTools{}

	if _, err := newAgent(&fakeModel{}, tools, &fakeStore{}, nil).Restore(session); err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if len(tools.executed) != 0 {
		t.Fatalf("修复过程执行了 %d 个命令: %+v", len(tools.executed), tools.executed)
	}
}

// 修复之后必须能继续对话：模型收到的视图里每个调用都有配对的观察。
func TestRestoredSessionCanContinueTheConversation(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "看看目录"})
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)},
	})
	session.PendingToolCallID = "call-1"

	model := &fakeModel{responses: []domain.ModelResponse{{Content: "上次那条命令结果未知，我重新确认一下"}}}
	subject := newAgent(model, nil, &fakeStore{}, nil)

	if _, err := subject.Restore(session); err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if _, err := subject.Handle(context.Background(), session, "继续"); err != nil {
		t.Fatalf("修复后无法继续对话: %v", err)
	}
	if got := rolesOf(model.requests[0].Messages); got != "system,user,assistant,tool,user" {
		t.Errorf("修复后的视图角色 = %q; want system,user,assistant,tool,user", got)
	}
}

// 修复结果保存失败要报错：带着悬空调用继续会在下一次请求时被供应商拒绝。
func TestRestoreReportsSaveFailure(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)},
	})

	_, err := newAgent(&fakeModel{}, nil, &fakeStore{failAt: 1}, nil).Restore(session)
	if err == nil {
		t.Fatal("修复结果保存失败时返回了成功")
	}
}

// transient 事件必须立刻推给 sink，而不能进入保存路径——它们不落库也不占序号。
func TestDeltasAreEmittedImmediatelyAndNeverStored(t *testing.T) {
	model := &fakeModel{
		responses: []domain.ModelResponse{{Content: "答案"}},
		deltas: []domain.TextDelta{
			{Text: "先想想", Reasoning: true},
			{Text: "答"},
			{Text: "案"},
		},
	}
	store := &fakeStore{}
	sink := &recordingSink{}

	if _, err := newAgent(model, nil, store, sink).Handle(context.Background(), newTestSession(), "问题"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	if store.transientReachedStore {
		t.Error("transient 事件被送进了保存路径")
	}

	var deltaTypes []string
	for _, event := range sink.events {
		if !event.Durable() {
			deltaTypes = append(deltaTypes, string(event.Type))
			if event.Sequence != 0 {
				t.Errorf("transient 事件带了序号 %d", event.Sequence)
			}
		}
	}
	want := []string{"assistant.reasoning.delta", "assistant.delta", "assistant.delta"}
	if strings.Join(deltaTypes, ",") != strings.Join(want, ",") {
		t.Errorf("delta 事件 = %v; want %v", deltaTypes, want)
	}
}

// delta 要在承载它的 assistant.message 之前到达：它们是那条消息正在被生成的过程。
func TestDeltasArriveBeforeTheMessageTheyBelongTo(t *testing.T) {
	model := &fakeModel{
		responses: []domain.ModelResponse{{Content: "答案"}},
		deltas:    []domain.TextDelta{{Text: "答"}, {Text: "案"}},
	}
	sink := &recordingSink{}

	if _, err := newAgent(model, nil, nil, sink).Handle(context.Background(), newTestSession(), "问题"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	types := sink.types()
	lastDelta, firstMessage := -1, -1
	for index, eventType := range types {
		if eventType == "assistant.delta" {
			lastDelta = index
		}
		if eventType == "assistant.message" && firstMessage < 0 {
			firstMessage = index
		}
	}
	if lastDelta < 0 || firstMessage < 0 || lastDelta > firstMessage {
		t.Errorf("delta 与 assistant.message 的先后不对: %v", types)
	}
}

// tool.started 必须在命令执行之前就落库，重放时界面才能显示"正要执行这个命令"。
func TestToolStartedIsCommittedBeforeExecution(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
		{Content: "完成"},
	}}
	// 第二次保存就是"assistant 消息 + tool.started"那一次，让它失败。
	store := &fakeStore{failAt: 2}
	tools := &fakeTools{}

	if _, err := newAgent(model, tools, store, nil).Handle(context.Background(), newTestSession(), "看看"); err == nil {
		t.Fatal("检查点失败时本轮没有终止")
	}
	if len(tools.executed) != 0 {
		t.Errorf("检查点没落库就执行了 %d 个命令", len(tools.executed))
	}
}

// 工具输出的片段要作为 transient 事件实时推出去，并带上它属于哪次调用。
func TestToolOutputIsEmittedAsTransientEvents(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
		{Content: "完成"},
	}}
	tools := &fakeTools{outputs: []string{"第一行\n", "第二行\n"}}
	sink := &recordingSink{}

	if _, err := newAgent(model, tools, nil, sink).Handle(context.Background(), newTestSession(), "看看"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	var chunks []string
	for _, event := range sink.events {
		if event.Type != domain.EventToolOutputDelta {
			continue
		}
		var payload domain.ToolOutputDeltaPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("payload 不是合法 JSON: %v", err)
		}
		if payload.ToolCallID != "call-1" {
			t.Errorf("输出片段挂在了 %q 上", payload.ToolCallID)
		}
		chunks = append(chunks, payload.Chunk)
	}
	if strings.Join(chunks, "") != "第一行\n第二行\n" {
		t.Errorf("输出片段 = %q", chunks)
	}
}

// 本轮失败必须留下明确的终态事件，否则重放出来的历史里会有一轮永远停在执行中。
func TestFailedTurnEmitsATerminalEvent(t *testing.T) {
	sink := &recordingSink{}
	model := &fakeModel{err: errors.New("供应商 500")}

	if _, err := newAgent(model, nil, nil, sink).Handle(context.Background(), newTestSession(), "问题"); err == nil {
		t.Fatal("模型失败时本轮没有失败")
	}

	types := strings.Join(sink.types(), ",")
	if !strings.Contains(types, "turn.failed") {
		t.Errorf("事件序列里没有 turn.failed: %v", sink.types())
	}
	var payload domain.TurnFailedPayload
	for _, event := range sink.events {
		if event.Type == domain.EventTurnFailed {
			_ = json.Unmarshal(event.Payload, &payload)
		}
	}
	if !strings.Contains(payload.Reason, "供应商 500") {
		t.Errorf("失败原因 = %q; want 保留原始错误", payload.Reason)
	}
}

// 同一轮里的消息和事件必须带同一个 turn_id，前端才能把它们归成一组。
func TestEverythingInATurnSharesTheSameTurnID(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{
		{ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)}},
		{Content: "完成"},
	}}
	sink := &recordingSink{}
	session := newTestSession()

	if _, err := newAgent(model, nil, nil, sink).Handle(context.Background(), session, "看看"); err != nil {
		t.Fatalf("Handle 返回错误: %v", err)
	}

	turnIDs := make(map[domain.TurnID]struct{})
	for _, message := range session.Messages() {
		turnIDs[message.TurnID] = struct{}{}
	}
	for _, event := range sink.events {
		turnIDs[event.TurnID] = struct{}{}
	}
	if len(turnIDs) != 1 {
		t.Errorf("一轮里出现了 %d 个不同的 turn_id: %v", len(turnIDs), turnIDs)
	}
	for id := range turnIDs {
		if err := domain.SessionID(id).Validate(); err == nil {
			t.Errorf("turn_id %q 看起来像会话 ID", id)
		}
		if id == "" {
			t.Error("turn_id 为空")
		}
	}
}

// 两轮交互的 turn_id 必须不同。
func TestEachTurnGetsItsOwnTurnID(t *testing.T) {
	model := &fakeModel{responses: []domain.ModelResponse{{Content: "一"}, {Content: "二"}}}
	subject := newAgent(model, nil, nil, nil)
	session := newTestSession()

	if _, err := subject.Handle(context.Background(), session, "第一问"); err != nil {
		t.Fatalf("第一轮返回错误: %v", err)
	}
	if _, err := subject.Handle(context.Background(), session, "第二问"); err != nil {
		t.Fatalf("第二轮返回错误: %v", err)
	}

	history := session.Messages()
	if history[0].TurnID == history[2].TurnID {
		t.Error("两轮用了同一个 turn_id")
	}
}

// 中断修复产生的观察要挂在被中断的那一轮上，并以 turn.failed 收口。
func TestRestoreEmitsEventsOnTheInterruptedTurn(t *testing.T) {
	interrupted := domain.TurnID("trn_0123456789abcdef0123456789abcdef")
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "删掉临时文件", TurnID: interrupted})
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{toolCall("call-1", "bash", `{}`)},
		TurnID:    interrupted,
	})
	session.PendingToolCallID = "call-1"
	sink := &recordingSink{}

	if _, err := newAgent(&fakeModel{}, nil, nil, sink).Restore(session); err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}

	want := []string{"tool.resolved", "turn.failed", "state.changed"}
	if strings.Join(sink.types(), ",") != strings.Join(want, ",") {
		t.Errorf("修复产生的事件 = %v; want %v", sink.types(), want)
	}
	for _, event := range sink.events {
		if event.TurnID != interrupted {
			t.Errorf("事件挂在了 %q 上; want %q", event.TurnID, interrupted)
		}
	}
	for _, message := range session.Messages() {
		if message.TurnID != interrupted {
			t.Errorf("消息挂在了 %q 上; want %q", message.TurnID, interrupted)
		}
	}
}
