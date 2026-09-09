package domain_test

import (
	"testing"

	"goseek/internal/domain"
)

const (
	// testSessionID 是一个格式合法的固定 ID，让测试不依赖随机值。
	testSessionID = domain.SessionID("ses_0123456789abcdef0123456789abcdef")
	// testWorkspace 是测试用的工作目录，本身不会被访问。
	testWorkspace = "/tmp/goseek-test"
)

func TestSessionKeepsAppendOrder(t *testing.T) {
	session := domain.NewSession(testSessionID, testWorkspace)
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第一句"})
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "第一答"})
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第二句"})

	// Message 含有切片字段，不能直接用 == 比较，逐字段断言。
	want := []domain.Message{
		{Role: domain.RoleUser, Content: "第一句"},
		{Role: domain.RoleAssistant, Content: "第一答"},
		{Role: domain.RoleUser, Content: "第二句"},
	}
	got := session.Messages()
	if len(got) != len(want) {
		t.Fatalf("Messages() 返回 %d 条; want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].Role != want[index].Role || got[index].Content != want[index].Content {
			t.Errorf("Messages()[%d] = %+v; want %+v", index, got[index], want[index])
		}
	}
}

func TestNewSessionIsEmpty(t *testing.T) {
	if got := domain.NewSession(testSessionID, testWorkspace).Messages(); len(got) != 0 {
		t.Fatalf("新会话有 %d 条历史; want 0", len(got))
	}
}

// 快照必须是副本：调用方改写取走的切片，不能影响会话内已经发生的事实。
func TestMessagesSnapshotIsIsolatedFromCallerWrites(t *testing.T) {
	session := domain.NewSession(testSessionID, testWorkspace)
	session.Append(domain.Message{Role: domain.RoleUser, Content: "原文"})

	snapshot := session.Messages()
	snapshot[0].Content = "被调用方改写"

	if got := session.Messages()[0].Content; got != "原文" {
		t.Fatalf("会话内容被调用方改写成 %q; want %q", got, "原文")
	}
}

// 反方向同样成立：取走快照之后继续 Append，不能通过共享底层数组污染已取走的快照。
func TestMessagesSnapshotIsIsolatedFromLaterAppends(t *testing.T) {
	session := domain.NewSession(testSessionID, testWorkspace)
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第一句"})

	snapshot := session.Messages()
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "第一答"})

	if len(snapshot) != 1 {
		t.Fatalf("已取走的快照长度变成 %d; want 1", len(snapshot))
	}
	if snapshot[0].Content != "第一句" {
		t.Fatalf("已取走的快照内容变成 %q; want %q", snapshot[0].Content, "第一句")
	}
}

// 正常会话里每个工具调用都有配对的观察，不应报出悬空调用。
func TestUnresolvedToolCallsIsEmptyForConsistentHistory(t *testing.T) {
	session := domain.NewSession(testSessionID, testWorkspace)
	session.Append(domain.Message{Role: domain.RoleUser, Content: "看看目录"})
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{{ID: "call-1", Name: "bash"}, {ID: "call-2", Name: "bash"}},
	})
	session.Append(domain.Message{Role: domain.RoleTool, ToolCallID: "call-1"})
	session.Append(domain.Message{Role: domain.RoleTool, ToolCallID: "call-2"})
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "看完了"})

	if open := session.UnresolvedToolCalls(); len(open) != 0 {
		t.Errorf("自洽历史报出了 %d 个悬空调用: %+v", len(open), open)
	}
}

// 进程死在两条 tool 消息之间时，未配对的调用要被按原顺序报出来。
func TestUnresolvedToolCallsReportsUnpairedCallsInOrder(t *testing.T) {
	session := domain.NewSession(testSessionID, testWorkspace)
	session.Append(domain.Message{Role: domain.RoleUser, Content: "看看目录"})
	session.Append(domain.Message{
		Role: domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{
			{ID: "call-1", Name: "bash"},
			{ID: "call-2", Name: "bash"},
			{ID: "call-3", Name: "bash"},
		},
	})
	session.Append(domain.Message{Role: domain.RoleTool, ToolCallID: "call-1"})

	open := session.UnresolvedToolCalls()
	if len(open) != 2 {
		t.Fatalf("报出 %d 个悬空调用; want 2", len(open))
	}
	if open[0].ID != "call-2" || open[1].ID != "call-3" {
		t.Errorf("悬空调用 = %q、%q; want call-2、call-3", open[0].ID, open[1].ID)
	}
}

// 跨多轮的历史里，早先几轮已经配对的调用不能被误报。
func TestUnresolvedToolCallsOnlyLooksAtUnpairedCalls(t *testing.T) {
	session := domain.NewSession(testSessionID, testWorkspace)
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第一轮"})
	session.Append(domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "old-1"}}})
	session.Append(domain.Message{Role: domain.RoleTool, ToolCallID: "old-1"})
	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "第一轮完成"})
	session.Append(domain.Message{Role: domain.RoleUser, Content: "第二轮"})
	session.Append(domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "new-1"}}})

	open := session.UnresolvedToolCalls()
	if len(open) != 1 || open[0].ID != "new-1" {
		t.Errorf("悬空调用 = %+v; want 只有 new-1", open)
	}
}

// LoadSession 是构造入口：它拷贝传入的历史，之后外部修改那个切片不能影响会话。
func TestLoadSessionCopiesTheGivenHistory(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, Content: "原文"},
	}
	session := domain.LoadSession(testSessionID, testWorkspace, messages)

	messages[0].Content = "被外部改写"

	if got := session.Messages()[0].Content; got != "原文" {
		t.Errorf("会话内容被外部切片改写成 %q", got)
	}
	if session.ID != testSessionID || session.Workspace != testWorkspace {
		t.Errorf("身份或工作目录丢失: %+v", session)
	}
}
