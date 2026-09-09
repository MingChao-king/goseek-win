package contextmgr_test

import (
	"encoding/json"
	"testing"

	"goseek/internal/contextmgr"
	"goseek/internal/domain"
)

// newTestSession 创建一个身份固定的空会话，让测试不依赖随机 ID。
func newTestSession() *domain.Session {
	return domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp/goseek-test")
}

// roleContentOf 把视图压缩成便于断言的 (角色, 正文) 序列。
func roleContentOf(view []domain.ModelMessage) [][2]string {
	pairs := make([][2]string, 0, len(view))
	for _, message := range view {
		pairs = append(pairs, [2]string{string(message.Role), message.Content})
	}
	return pairs
}

func TestBuildOnEmptySessionCarriesOnlySystemPrompt(t *testing.T) {
	view := contextmgr.Build(newTestSession(), domain.ConversationMemory{}, nil, 0).Messages

	if len(view) != 1 {
		t.Fatalf("视图长度 = %d; want 1", len(view))
	}
	if view[0].Role != domain.ModelRoleSystem {
		t.Errorf("首条消息角色 = %q; want %q", view[0].Role, domain.ModelRoleSystem)
	}
	if view[0].Content == "" {
		t.Error("system 指令为空")
	}
}

// 视图的消息顺序是代码的责任：system 在最前，历史按原顺序跟随。
func TestBuildPlacesSystemPromptBeforeHistoryInOrder(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "你好"})
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "你好，我是 GoSeek"})
	session.Append(domain.Message{Role: domain.RoleUser, Content: "我刚才说了什么"})

	view := contextmgr.Build(session, domain.ConversationMemory{}, nil, 0).Messages

	if len(view) != 4 {
		t.Fatalf("视图长度 = %d; want 4", len(view))
	}
	got := roleContentOf(view)[1:]
	want := [][2]string{
		{"user", "你好"},
		{"assistant", "你好，我是 GoSeek"},
		{"user", "我刚才说了什么"},
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("视图第 %d 条 = %v; want %v", index+1, got[index], want[index])
		}
	}
}

// 构建视图不能改变会话：system 指令只存在于视图里，历史条数也不受影响。
func TestBuildDoesNotMutateSession(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "你好"})

	contextmgr.Build(session, domain.ConversationMemory{}, nil, 0)
	contextmgr.Build(session, domain.ConversationMemory{}, nil, 0)

	history := session.Messages()
	if len(history) != 1 {
		t.Fatalf("构建视图后历史有 %d 条; want 1", len(history))
	}
	if history[0].Role != domain.RoleUser || history[0].Content != "你好" {
		t.Errorf("历史被改写成 %+v", history[0])
	}
}

func TestBuildWithAmbientChangesOnlyTheModelView(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "请查看当前页面"})

	view := contextmgr.BuildWithAmbient(
		session,
		domain.ConversationMemory{},
		nil,
		100000,
		0.8,
		nil,
		[]string{"[发送时侧边栏]\n- 页面：https://example.com"},
	)

	history := session.Messages()
	if len(history) != 1 || history[0].Content != "请查看当前页面" {
		t.Fatalf("临时侧栏上下文污染了历史: %+v", history)
	}
	if got := view.Messages[len(view.Messages)-1].Content; got !=
		"[发送时侧边栏]\n- 页面：https://example.com\n\n[用户原文]\n请查看当前页面" {
		t.Fatalf("模型视图里的临时上下文 = %q", got)
	}

	next := contextmgr.Build(session, domain.ConversationMemory{}, nil, 100000)
	if got := next.Messages[len(next.Messages)-1].Content; got != "请查看当前页面" {
		t.Fatalf("临时上下文泄漏到了下一次视图: %q", got)
	}
}

func TestBuildWithAmbientKeepsQueuedMessageAssociation(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第一条"})
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第二条"})

	view := contextmgr.BuildWithAmbient(
		session,
		domain.ConversationMemory{},
		nil,
		100000,
		0.8,
		nil,
		[]string{"第一份侧栏", "第二份侧栏"},
	)

	if got := view.Messages[len(view.Messages)-2].Content; got != "第一份侧栏\n\n[用户原文]\n第一条" {
		t.Fatalf("第一条消息绑定错误: %q", got)
	}
	if got := view.Messages[len(view.Messages)-1].Content; got != "第二份侧栏\n\n[用户原文]\n第二条" {
		t.Fatalf("第二条消息绑定错误: %q", got)
	}
}

// 工具调用和它的观察必须原样进入视图：供应商靠 tool_call_id 把两者对上，
// 少了任何一半，下一次请求都会因为悬空调用而无法发出。
func TestBuildCarriesToolCallsAndToolCallIDs(t *testing.T) {
	call := domain.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "看看目录"})
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "我看一下", ToolCalls: []domain.ToolCall{call}})
	session.Append(domain.Message{Role: domain.RoleTool, Content: `{"status":"success"}`, ToolCallID: "call-1"})

	view := contextmgr.Build(session, domain.ConversationMemory{}, nil, 0).Messages

	if len(view) != 4 {
		t.Fatalf("视图长度 = %d; want 4", len(view))
	}

	assistant := view[2]
	if assistant.Role != domain.ModelRoleAssistant {
		t.Fatalf("第三条角色 = %q; want assistant", assistant.Role)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" {
		t.Errorf("assistant 消息的工具调用 = %+v; want 一个 call-1", assistant.ToolCalls)
	}
	if string(assistant.ToolCalls[0].Arguments) != `{"command":"ls"}` {
		t.Errorf("参数被改动: %s", assistant.ToolCalls[0].Arguments)
	}

	observation := view[3]
	if observation.Role != domain.ModelRoleTool {
		t.Errorf("第四条角色 = %q; want tool", observation.Role)
	}
	if observation.ToolCallID != "call-1" {
		t.Errorf("tool 消息的 tool_call_id = %q; want call-1", observation.ToolCallID)
	}
}

// assistant 只提出工具调用、没有说明文字时，空正文要原样进入视图。
func TestBuildKeepsEmptyAssistantContentWhenOnlyToolCallsWereProposed(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "看看目录"})
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{}`)}},
	})

	view := contextmgr.Build(session, domain.ConversationMemory{}, nil, 0).Messages

	if view[2].Content != "" {
		t.Errorf("空正文被改成了 %q", view[2].Content)
	}
	if len(view[2].ToolCalls) != 1 {
		t.Errorf("工具调用丢失: %+v", view[2])
	}
}

// 视图带回这次请求的占用估算。工具定义虽然不进消息序列，但同样占额度，
// 必须参与计数——漏算会让估算系统性偏低，而偏低会让请求被供应商拒。
func TestBuildEstimatesUsageIncludingTools(t *testing.T) {
	session := newTestSession()
	session.Append(domain.Message{Role: domain.RoleUser, Content: "你好"})

	withoutTools := contextmgr.Build(session, domain.ConversationMemory{}, nil, 100000)
	withTools := contextmgr.Build(session, domain.ConversationMemory{}, []domain.ToolSpec{{
		Name:        "bash",
		Description: "在用户本机运行一条 Bash 命令",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
	}}, 100000)

	if withoutTools.Usage.InputTokens <= 0 {
		t.Fatalf("没有估算出输入 token: %+v", withoutTools.Usage)
	}
	if withTools.Usage.InputTokens <= withoutTools.Usage.InputTokens {
		t.Error("工具定义没有被算进上下文占用")
	}
	if withTools.Usage.Source != domain.ContextUsageEstimated {
		t.Errorf("来源 = %q; want estimated", withTools.Usage.Source)
	}
	// 视图里的消息序列不受工具影响——工具由 Agent 直接交给 Model，不进消息。
	if len(withTools.Messages) != len(withoutTools.Messages) {
		t.Error("工具定义混进了消息序列")
	}
}

// 没有配置窗口时不编比例：来源标成 unknown，界面显示"未知"。
func TestBuildReportsUnknownWhenContextWindowIsNotConfigured(t *testing.T) {
	view := contextmgr.Build(newTestSession(), domain.ConversationMemory{}, nil, 0)

	if view.Usage.Source != domain.ContextUsageUnknown {
		t.Errorf("来源 = %q; want unknown", view.Usage.Source)
	}
	if view.Usage.Known() {
		t.Error("没有窗口时 Known 应为 false")
	}
	// 输入 token 本身仍然有意义，只是无法判断"占了多少"。
	if view.Usage.InputTokens <= 0 {
		t.Error("窗口未知时不应放弃统计输入 token")
	}
}
