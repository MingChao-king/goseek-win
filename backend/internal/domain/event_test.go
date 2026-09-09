package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// eventTime 是测试里固定的事件时间。
var eventTime = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

const eventTurnID = domain.TurnID("trn_0123456789abcdef0123456789abcdef")

func TestNewTurnIDIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[domain.TurnID]struct{})
	for range 50 {
		id, err := domain.NewTurnID()
		if err != nil {
			t.Fatalf("NewTurnID 返回错误: %v", err)
		}
		if !strings.HasPrefix(string(id), "trn_") {
			t.Fatalf("TurnID %q 没有 trn_ 前缀", id)
		}
		if len(string(id)) != len("trn_")+32 {
			t.Fatalf("TurnID %q 长度不对", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("生成了重复的 TurnID %q", id)
		}
		seen[id] = struct{}{}
	}
}

// 会话 ID 和轮次 ID 必须互相区分得开，前缀就是为此存在的。
func TestSessionAndTurnIDsDoNotCollide(t *testing.T) {
	sessionID, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID 返回错误: %v", err)
	}
	turnID, err := domain.NewTurnID()
	if err != nil {
		t.Fatalf("NewTurnID 返回错误: %v", err)
	}

	if err := domain.SessionID(turnID).Validate(); err == nil {
		t.Error("轮次 ID 通过了会话 ID 的校验")
	}
	if err := sessionID.Validate(); err != nil {
		t.Errorf("会话 ID 没有通过自己的校验: %v", err)
	}
}

// durable 与 transient 的划分是行为底线第 11 条的落点，必须钉住。
func TestOnlyDeltasAreTransient(t *testing.T) {
	transient := []domain.EventType{
		domain.EventAssistantDelta,
		domain.EventAssistantReasoningDelta,
		domain.EventToolOutputDelta,
	}
	durable := []domain.EventType{
		domain.EventTurnStarted, domain.EventStateChanged, domain.EventUserMessage,
		domain.EventAssistantMessage, domain.EventToolStarted, domain.EventToolResolved,
		domain.EventTurnCompleted, domain.EventTurnFailed,
	}

	for _, eventType := range transient {
		if domain.NewEvent(eventTurnID, eventType, nil, eventTime).Durable() {
			t.Errorf("%s 被当成了 durable", eventType)
		}
	}
	for _, eventType := range durable {
		if !domain.NewEvent(eventTurnID, eventType, nil, eventTime).Durable() {
			t.Errorf("%s 被当成了 transient", eventType)
		}
	}
}

// 未知的新类型默认是 durable：漏改只会多存一份，而反过来会让事件悄悄消失。
func TestUnknownEventTypeDefaultsToDurable(t *testing.T) {
	unknown := domain.NewEvent(eventTurnID, domain.EventType("something.new"), nil, eventTime)
	if !unknown.Durable() {
		t.Error("未知事件类型默认成了 transient")
	}
}

func TestNewEventCarriesTurnTypeAndTime(t *testing.T) {
	event := domain.NewEvent(eventTurnID, domain.EventUserMessage,
		domain.UserMessagePayload{Content: "你好"}, eventTime)

	if event.TurnID != eventTurnID || event.Type != domain.EventUserMessage {
		t.Errorf("事件 = %+v", event)
	}
	if !event.At.Equal(eventTime) {
		t.Errorf("At = %v; want %v", event.At, eventTime)
	}
	if event.Sequence != 0 {
		t.Errorf("新建事件的 Sequence = %d; want 0（由持久化层分配）", event.Sequence)
	}

	var decoded domain.UserMessagePayload
	if err := json.Unmarshal(event.Payload, &decoded); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if decoded.Content != "你好" {
		t.Errorf("payload = %+v", decoded)
	}
}

// payload 里的 shell 操作符不能被转义：它要么给模型读，要么给人翻数据库看。
func TestPayloadDoesNotEscapeShellOperators(t *testing.T) {
	call := domain.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"a && b > c"}`)}
	event := domain.NewEvent(eventTurnID, domain.EventToolStarted,
		domain.ToolStartedPayload{Call: call, Title: "试试"}, eventTime)

	if strings.Contains(string(event.Payload), `\u0026`) {
		t.Errorf("payload 里的 & 被写成了 \\u0026: %s", event.Payload)
	}
	if !strings.Contains(string(event.Payload), "a && b > c") {
		t.Errorf("命令没有原样出现在 payload 里: %s", event.Payload)
	}
}

// 同理，工具观察是直接交给模型阅读的文本，更不能满屏转义序列。
func TestEncodeContentDoesNotEscapeShellOperators(t *testing.T) {
	result := domain.ToolResult{
		ToolCallID: "call-1", Name: "bash", Status: domain.ToolSuccess,
		Content: "$ a && b > c\n完成",
	}

	encoded := result.EncodeContent()
	if strings.Contains(encoded, `\u0026`) {
		t.Errorf("观察里的 & 被写成了 \\u0026: %s", encoded)
	}
	if !strings.Contains(encoded, "a && b > c") {
		t.Errorf("命令没有原样出现: %s", encoded)
	}
	// 转义与否不能影响往返。
	var decoded domain.ToolResult
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("观察不是合法 JSON: %v", err)
	}
	if decoded.Content != result.Content {
		t.Errorf("往返后内容变了: %q", decoded.Content)
	}
}

// assistant.message 显式带 final 标记，前端不必自己重推"无工具调用且文字非空"这条规则。
func TestAssistantMessagePayloadCarriesFinalFlag(t *testing.T) {
	final := domain.AssistantMessagePayload{Content: "答案", Final: true}
	process := domain.AssistantMessagePayload{
		Content:   "我先看看",
		ToolCalls: []domain.ToolCall{{ID: "call-1", Name: "bash"}},
		Final:     false,
	}

	for name, payload := range map[string]domain.AssistantMessagePayload{"final": final, "process": process} {
		event := domain.NewEvent(eventTurnID, domain.EventAssistantMessage, payload, eventTime)
		var decoded domain.AssistantMessagePayload
		if err := json.Unmarshal(event.Payload, &decoded); err != nil {
			t.Fatalf("%s payload 不是合法 JSON: %v", name, err)
		}
		if decoded.Final != payload.Final {
			t.Errorf("%s 的 final = %v; want %v", name, decoded.Final, payload.Final)
		}
	}
}
