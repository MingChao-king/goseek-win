package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// historySession 造一个有 6 条消息、已经被压缩成一棵两层摘要树的会话。
//
//	P（level 1，覆盖 0..4）
//	├── L1（level 0，覆盖 0..2）
//	└── L2（level 0，覆盖 2..4）
//	原文 4..6 未压缩
func historySession(t *testing.T) *domain.Session {
	t.Helper()
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	for index := range 3 {
		turnID := domain.TurnID(fmt.Sprintf("trn_%032d", index))
		session.Append(domain.Message{
			Role: domain.RoleUser, TurnID: turnID,
			Content: fmt.Sprintf("第 %d 个问题", index),
		})
		session.Append(domain.Message{
			Role: domain.RoleAssistant, TurnID: turnID,
			Content: fmt.Sprintf("第 %d 个回答", index),
		})
	}

	created := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	leafOne := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000a", Level: 0,
		Content: "第一段摘要\n细节若干", StartMessageIndex: 0, EndMessageIndex: 2, CreatedAt: created,
	}
	leafTwo := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000b", Level: 0,
		Content: "第二段摘要", StartMessageIndex: 2, EndMessageIndex: 4, CreatedAt: created,
	}
	parent := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000c", Level: 1,
		Content: "合并摘要", StartMessageIndex: 0, EndMessageIndex: 4,
		SourceBatchIDs: []domain.MemoryBatchID{leafOne.ID, leafTwo.ID}, CreatedAt: created,
	}
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 4,
		Batches:             []domain.MemoryBatch{leafOne, leafTwo, parent},
		ActiveBatchIDs:      []domain.MemoryBatchID{parent.ID},
	}
	return session
}

// runHistory 执行一次回查并断言不返回 Go error。
func runHistory(t *testing.T, session *domain.Session, arguments string) domain.ToolResult {
	t.Helper()
	call := domain.ToolCall{ID: "call-1", Name: "conversation_history", Arguments: json.RawMessage(arguments)}
	result, err := NewHistory(session).Run(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Run 返回了 error，回查的失败应当由观察表达: %v", err)
	}
	return result
}

// inspect 只返回**直接**子节点。一次展开整棵树等于把历史重新灌回上下文，
// 压缩就白做了。
func TestInspectReturnsOnlyDirectChildren(t *testing.T) {
	session := historySession(t)

	result := runHistory(t, session, `{"action":"inspect","batch_id":"mem_0000000000000000000000000000000c"}`)

	if result.Status != domain.ToolSuccess {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{"mem_0000000000000000000000000000000a", "mem_0000000000000000000000000000000b", "层级 1"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("输出缺少 %q:\n%s", want, result.Content)
		}
	}
	// 不该把子节点的摘要正文也铺开——那是下一层 inspect 的事。
	if strings.Contains(result.Content, "细节若干") {
		t.Errorf("展开了子节点的正文:\n%s", result.Content)
	}
}

// 叶子节点要明确说"到底了"，否则模型会继续往下找一层不存在的子节点。
func TestInspectSaysWhenTheNodeIsALeaf(t *testing.T) {
	session := historySession(t)

	result := runHistory(t, session, `{"action":"inspect","batch_id":"mem_0000000000000000000000000000000a"}`)

	if !strings.Contains(result.Content, "叶子节点") {
		t.Errorf("没有说明这是叶子:\n%s", result.Content)
	}
}

// read 返回节点覆盖范围内的原始消息，并给出继续读的位置。
func TestReadReturnsCoveredMessagesWithPagination(t *testing.T) {
	session := historySession(t)

	first := runHistory(t, session,
		`{"action":"read","batch_id":"mem_0000000000000000000000000000000c","limit":2}`)
	if first.Status != domain.ToolSuccess {
		t.Fatalf("result = %+v", first)
	}
	if !strings.Contains(first.Content, "第 0 个问题") {
		t.Errorf("没有返回原文:\n%s", first.Content)
	}
	// 该节点覆盖 4 条，只读了 2 条，必须告诉模型怎么接着读。
	if !strings.Contains(first.Content, "offset=2") {
		t.Errorf("没有给出继续读的位置:\n%s", first.Content)
	}

	second := runHistory(t, session,
		`{"action":"read","batch_id":"mem_0000000000000000000000000000000c","offset":2,"limit":2}`)
	if !strings.Contains(second.Content, "读完") {
		t.Errorf("读到末尾时没有说明:\n%s", second.Content)
	}
}

// 各种非法输入都要变成模型可读的观察，而不是让本轮失败。
func TestHistoryTurnsBadInputIntoObservations(t *testing.T) {
	session := historySession(t)

	cases := []struct {
		name       string
		arguments  string
		wantInText string
	}{
		{"节点不存在", `{"action":"inspect","batch_id":"mem_0000000000000000000000000000ffff"}`, "没有找到"},
		{"batch_id 格式不对", `{"action":"inspect","batch_id":"随便写的"}`, "格式"},
		{"action 不认识", `{"action":"delete","batch_id":"mem_0000000000000000000000000000000a"}`, "search、read 或 inspect"},
		{"缺 action", `{"batch_id":"mem_0000000000000000000000000000000a"}`, "action"},
		{"缺 batch_id", `{"action":"read"}`, "batch_id"},
		{"未知字段", `{"action":"read","batch_id":"mem_0000000000000000000000000000000a","session_id":"别的会话"}`, "不符合"},
		{"offset 越界", `{"action":"read","batch_id":"mem_0000000000000000000000000000000a","offset":99}`, "超出范围"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := runHistory(t, session, testCase.arguments)
			if result.Status != domain.ToolError {
				t.Fatalf("status = %q; want error", result.Status)
			}
			if !strings.Contains(result.Content, testCase.wantInText) {
				t.Errorf("说明里没有 %q: %s", testCase.wantInText, result.Content)
			}
		})
	}
}

// 参数里没有 session_id 也没有路径，模型无法越权读别的会话或任何文件。
func TestHistorySchemaExposesNoWayToReachOtherSessions(t *testing.T) {
	var schema struct {
		Properties           map[string]any `json:"properties"`
		AdditionalProperties bool           `json:"additionalProperties"`
	}
	if err := json.Unmarshal(NewHistory(nil).Spec().Parameters, &schema); err != nil {
		t.Fatalf("Schema 不是合法 JSON: %v", err)
	}

	for _, forbidden := range []string{"session_id", "path", "file"} {
		if _, present := schema.Properties[forbidden]; present {
			t.Errorf("Schema 里出现了 %q，模型可能借它越权", forbidden)
		}
	}
	if schema.AdditionalProperties {
		t.Error("Schema 允许额外字段")
	}
}

// limit 有上限：一次读回太多，等于把刚压缩掉的东西又装回上下文。
func TestReadCapsTheNumberOfMessages(t *testing.T) {
	session := historySession(t)

	result := runHistory(t, session,
		`{"action":"read","batch_id":"mem_0000000000000000000000000000000c","limit":999}`)

	// 该节点只覆盖 4 条，全给出来也不会超过上限；这里验证的是不因为 limit 超大而出错。
	if result.Status != domain.ToolSuccess {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "读完") {
		t.Errorf("输出 = %s", result.Content)
	}
}
