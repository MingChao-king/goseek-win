package store

import (
	"encoding/json"
	"errors"
	"testing"

	"goseek/internal/domain"
)

// newTestReader 在同一个数据目录上打开一个只读句柄。
func newTestReader(t *testing.T, dataDirectory string) *Reader {
	t.Helper()
	reader, err := NewReader(dataDirectory)
	if err != nil {
		t.Fatalf("NewReader 返回错误: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

// 这是 Reader 存在的理由：会话正被写者独占持有时，读依然要能立刻返回。
// 让读也去抢会话锁的话，一轮交互期间的快照请求会全部排队。
func TestReaderWorksWhileTheSessionIsHeldByAWriter(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session := sampleSession(t, store)
	// 注意：这里**不** Close，会话仍被 store 独占持有。

	reader := newTestReader(t, dataDirectory)

	summaries, err := reader.List(false)
	if err != nil {
		t.Fatalf("List 返回错误: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != session.ID {
		t.Fatalf("List = %+v", summaries)
	}

	loaded, _, err := reader.Session(session.ID)
	if err != nil {
		t.Fatalf("Session 返回错误: %v", err)
	}
	if len(loaded.Messages()) != 3 {
		t.Errorf("读到 %d 条消息; want 3", len(loaded.Messages()))
	}
}

// 只读句柄看到的必须是已提交的内容；写者提交之后，读者立刻能看到。
func TestReaderSeesCommittedWrites(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	reader := newTestReader(t, dataDirectory)

	if _, _, err := reader.Session(session.ID); err != nil {
		t.Fatalf("新建后立刻读失败: %v", err)
	}

	session.Append(domain.Message{Role: domain.RoleUser, Content: "新消息", TurnID: turnID})
	if _, err := store.Save(session, []domain.RunEvent{durableEvent(domain.EventTurnStarted, nil)}); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	loaded, lastSequence, err := reader.Session(session.ID)
	if err != nil {
		t.Fatalf("Session 返回错误: %v", err)
	}
	if len(loaded.Messages()) != 1 {
		t.Errorf("读到 %d 条消息; want 1", len(loaded.Messages()))
	}
	// 快照带回的序号是前端衔接实时事件流的锚点。
	if lastSequence != 1 {
		t.Errorf("last_sequence = %d; want 1", lastSequence)
	}
}

// 重放查询：取序号大于 N 的事件，按序号排列。客户端一条没看过时传 0。
func TestReaderReplaysEventsAfterASequence(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	for _, eventType := range []domain.EventType{
		domain.EventTurnStarted, domain.EventUserMessage,
		domain.EventAssistantMessage, domain.EventTurnCompleted,
	} {
		if _, err := store.Save(session, []domain.RunEvent{durableEvent(eventType, nil)}); err != nil {
			t.Fatalf("Save 返回错误: %v", err)
		}
	}
	reader := newTestReader(t, dataDirectory)

	all, err := reader.EventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("EventsAfter 返回错误: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("从 0 开始重放得到 %d 个事件; want 4（序号从 1 开始）", len(all))
	}
	if all[0].Sequence != 1 || all[3].Sequence != 4 {
		t.Errorf("序号 = %d…%d", all[0].Sequence, all[3].Sequence)
	}
	if all[0].Type != domain.EventTurnStarted {
		t.Errorf("第一个事件 = %q", all[0].Type)
	}

	tail, err := reader.EventsAfter(session.ID, 2)
	if err != nil {
		t.Fatalf("EventsAfter 返回错误: %v", err)
	}
	if len(tail) != 2 || tail[0].Sequence != 3 {
		t.Errorf("从 2 开始重放得到 %+v", tail)
	}

	// 已经看完时返回空，而不是报错。
	none, err := reader.EventsAfter(session.ID, 4)
	if err != nil {
		t.Fatalf("EventsAfter 返回错误: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("从最新序号之后重放得到 %d 个事件; want 0", len(none))
	}
}

// 重放出来的事件要保留 payload 和轮次归属，前端才能渲染。
func TestReplayedEventsKeepPayloadAndTurn(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	if _, err := store.Save(session, []domain.RunEvent{
		durableEvent(domain.EventUserMessage, domain.UserMessagePayload{Content: "你好"}),
	}); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	events, err := newTestReader(t, dataDirectory).EventsAfter(session.ID, 0)
	if err != nil {
		t.Fatalf("EventsAfter 返回错误: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("重放得到 %d 个事件", len(events))
	}
	if events[0].TurnID != turnID {
		t.Errorf("turn_id = %q", events[0].TurnID)
	}
	if !events[0].At.Equal(fixedTime) {
		t.Errorf("时间 = %v", events[0].At)
	}
	var payload domain.UserMessagePayload
	if err := unmarshalPayload(events[0], &payload); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	if payload.Content != "你好" {
		t.Errorf("payload = %+v", payload)
	}
}

// 不存在的会话要返回哨兵错误，HTTP 层据此映射成 404。
func TestReaderReportsMissingSessionWithSentinel(t *testing.T) {
	_, dataDirectory := newTestStore(t)
	reader := newTestReader(t, dataDirectory)

	missing, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID 返回错误: %v", err)
	}
	if _, _, err := reader.Session(missing); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("错误 = %v; want ErrSessionNotFound", err)
	}
}

// 会话 ID 是外部输入（来自 URL），查询前必须先过格式校验。
func TestReaderValidatesSessionID(t *testing.T) {
	_, dataDirectory := newTestStore(t)
	reader := newTestReader(t, dataDirectory)

	if _, _, err := reader.Session("../../etc/passwd"); err == nil {
		t.Error("非法会话 ID 被接受了")
	}
	if _, err := reader.EventsAfter("不是 ID", 0); err == nil {
		t.Error("非法会话 ID 被接受了")
	}
}

// unmarshalPayload 是测试里解 payload 的便捷函数。
func unmarshalPayload(event domain.RunEvent, target any) error {
	return json.Unmarshal(event.Payload, target)
}
