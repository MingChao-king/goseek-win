package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// turnID 是测试里固定的轮次 ID。
const turnID = domain.TurnID("trn_0123456789abcdef0123456789abcdef")

// durableEvent 造一个会落库的事件。
func durableEvent(eventType domain.EventType, payload any) domain.RunEvent {
	return domain.NewEvent(turnID, eventType, payload, fixedTime)
}

// countEvents 数一数某个会话在 events 表里有多少行。
func countEvents(t *testing.T, store *Store, id domain.SessionID) int {
	t.Helper()
	var count int
	if err := store.connection.QueryRow(
		`SELECT COUNT(*) FROM events WHERE session_id = ?`, string(id)).Scan(&count); err != nil {
		t.Fatalf("统计事件失败: %v", err)
	}
	return count
}

// sequence 在会话内单调递增、不跳号，并且跨多次保存继续往下排。
func TestSaveAssignsMonotonicSequences(t *testing.T) {
	store, _ := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}

	first, err := store.Save(session, []domain.RunEvent{
		durableEvent(domain.EventTurnStarted, nil),
		durableEvent(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateWaitingModel}),
	})
	if err != nil {
		t.Fatalf("第一次 Save 返回错误: %v", err)
	}
	second, err := store.Save(session, []domain.RunEvent{
		durableEvent(domain.EventTurnCompleted, nil),
	})
	if err != nil {
		t.Fatalf("第二次 Save 返回错误: %v", err)
	}

	got := []int64{first[0].Sequence, first[1].Sequence, second[0].Sequence}
	for index, want := range []int64{1, 2, 3} {
		if got[index] != want {
			t.Errorf("第 %d 个事件的 sequence = %d; want %d", index+1, got[index], want)
		}
	}
}

// 重新打开会话之后，序号要从数据库里已有的最大值继续，不能从头再来。
func TestSequenceContinuesAfterReopen(t *testing.T) {
	first, dataDirectory := newTestStore(t)
	session, err := first.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	if _, err := first.Save(session, []domain.RunEvent{
		durableEvent(domain.EventTurnStarted, nil),
		durableEvent(domain.EventTurnCompleted, nil),
	}); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	second := openStoreAt(t, dataDirectory, fixedTime)
	reloaded, err := second.Load(session.ID)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	events, err := second.Save(reloaded, []domain.RunEvent{durableEvent(domain.EventTurnStarted, nil)})
	if err != nil {
		t.Fatalf("重开后 Save 返回错误: %v", err)
	}
	if events[0].Sequence != 3 {
		t.Errorf("重开后第一个事件的 sequence = %d; want 3", events[0].Sequence)
	}
}

// transient 事件不落库、不占序号：delta 只是打字动画，存下来会让事件表膨胀，
// 也会让"重放一遍还原发生过什么"失去意义。
func TestTransientEventsAreNeitherStoredNorNumbered(t *testing.T) {
	store, _ := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}

	persisted, err := store.Save(session, []domain.RunEvent{
		durableEvent(domain.EventTurnStarted, nil),
		durableEvent(domain.EventAssistantDelta, domain.TextDeltaPayload{Text: "一"}),
		durableEvent(domain.EventAssistantReasoningDelta, domain.TextDeltaPayload{Text: "想"}),
		durableEvent(domain.EventToolOutputDelta, domain.ToolOutputDeltaPayload{Chunk: "输出"}),
		durableEvent(domain.EventTurnCompleted, nil),
	})
	if err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	if len(persisted) != 2 {
		t.Fatalf("落库了 %d 个事件; want 2（只有两个 durable）", len(persisted))
	}
	if persisted[0].Sequence != 1 || persisted[1].Sequence != 2 {
		t.Errorf("序号 = %d、%d; want 1、2（transient 不占位）",
			persisted[0].Sequence, persisted[1].Sequence)
	}
	if count := countEvents(t, store, session.ID); count != 2 {
		t.Errorf("events 表里有 %d 行; want 2", count)
	}
}

// 消息和事件在同一个事务里：任何一半写不进去，另一半也不能留下。
func TestSaveRollsBackEventsWhenAMessageFails(t *testing.T) {
	store, _ := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}

	session.Append(domain.Message{Role: domain.RoleUser, Content: "正常的一条", TurnID: turnID})
	// system 不是合法的会话角色，CHECK 约束会拒绝它。
	session.Append(domain.Message{Role: domain.Role("system"), Content: "会被拒绝"})

	if _, err := store.Save(session, []domain.RunEvent{durableEvent(domain.EventTurnStarted, nil)}); err == nil {
		t.Fatal("非法角色被写进了数据库")
	}

	if count := countEvents(t, store, session.ID); count != 0 {
		t.Errorf("消息失败后仍留下了 %d 个事件", count)
	}
	// 序号也不能因为一次失败的保存而前进，否则后面的事件会跳号。
	events, err := store.Save(domain.LoadSession(session.ID, session.Workspace, nil),
		[]domain.RunEvent{durableEvent(domain.EventTurnStarted, nil)})
	if err != nil {
		t.Fatalf("回滚后再保存失败: %v", err)
	}
	if events[0].Sequence != 1 {
		t.Errorf("回滚后的第一个序号 = %d; want 1（失败的事务不能消耗号码）", events[0].Sequence)
	}
}

// 事件按 payload 原样落库，重放时前端要能读到同样的内容。
func TestEventPayloadIsStoredVerbatim(t *testing.T) {
	store, _ := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}

	call := domain.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls && pwd"}`)}
	if _, err := store.Save(session, []domain.RunEvent{
		durableEvent(domain.EventToolStarted, domain.ToolStartedPayload{Call: call, Title: "查看目录"}),
	}); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	var eventType, payload, at string
	if err := store.connection.QueryRow(
		`SELECT type, payload, at FROM events WHERE session_id = ? AND sequence = 1`,
		string(session.ID)).Scan(&eventType, &payload, &at); err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	if eventType != string(domain.EventToolStarted) {
		t.Errorf("type = %q", eventType)
	}

	var decoded domain.ToolStartedPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if decoded.Title != "查看目录" || decoded.Call.ID != "call-1" {
		t.Errorf("payload = %+v", decoded)
	}
	// 工具参数在事件里同样要逐字节保留，前端展示的必须是模型真实发出的东西。
	if string(decoded.Call.Arguments) != `{"command":"ls && pwd"}` {
		t.Errorf("参数被改写成 %s", decoded.Call.Arguments)
	}
	if parsed, err := parseTime(at); err != nil || !parsed.Equal(fixedTime) {
		t.Errorf("at = %q（解析结果 %v，err %v）", at, parsed, err)
	}
}

// 消息要带上轮次归属，否则前端没法把一轮里的东西归成一组。
func TestMessagesCarryTurnID(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	session.Append(domain.Message{Role: domain.RoleUser, Content: "问题", TurnID: turnID})
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, dataDirectory, fixedTime)
	loaded, err := reopened.Load(session.ID)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if got := loaded.Messages()[0].TurnID; got != turnID {
		t.Errorf("turn_id = %q; want %q", got, turnID)
	}
}

// 迁移必须能加在**旧版本**的库上，而不是只对全新的库有效。
//
// 这条测试把一个已经建好的库人工回退到 v1（删掉 v2、v3 加的东西并改回版本号），
// 再重新打开，验证后续迁移被逐版应用上来。用户手上的库正是这样一步步升级的，
// 只测"空库建到最新版"会漏掉这条路径。
func TestMigrationsApplyToAnExistingOlderDatabase(t *testing.T) {
	directory := t.TempDir()

	seed := openStoreAt(t, directory, fixedTime)
	if _, err := seed.connection.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("回退版本失败: %v", err)
	}
	if _, err := seed.connection.Exec(
		`DROP TABLE memory_batches;
		 DROP TABLE events;
		 DROP TABLE message_images;
		 ALTER TABLE messages DROP COLUMN turn_id;
		 ALTER TABLE sessions DROP COLUMN last_sequence;
		 ALTER TABLE sessions DROP COLUMN raw_compaction_cursor;
		 ALTER TABLE sessions DROP COLUMN active_batch_ids;
		 ALTER TABLE sessions DROP COLUMN archived_at;
		 ALTER TABLE sessions DROP COLUMN title;
		 ALTER TABLE sessions DROP COLUMN quotes_collapsed_before;
		 ALTER TABLE sessions DROP COLUMN collapsed_quotes;
		 ALTER TABLE sessions DROP COLUMN model;`); err != nil {
		t.Fatalf("回退表结构失败: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	upgraded := openStoreAt(t, directory, fixedTime)
	var version int
	if err := upgraded.connection.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读取版本失败: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("升级后 user_version = %d; want %d", version, len(migrations))
	}

	session, err := upgraded.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("升级后 Create 失败: %v", err)
	}
	if _, err := upgraded.Save(session, []domain.RunEvent{durableEvent(domain.EventTurnStarted, nil)}); err != nil {
		t.Fatalf("升级后 Save 失败: %v", err)
	}
}

// 事件的主键同时就是重放要用的索引：按会话取序号大于 N 的事件、按序号排序。
func TestEventsCanBeReplayedInOrderFromASequence(t *testing.T) {
	store, _ := newTestStore(t)
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

	rows, err := store.connection.Query(
		`SELECT sequence, type FROM events WHERE session_id = ? AND sequence > ? ORDER BY sequence`,
		string(session.ID), 2)
	if err != nil {
		t.Fatalf("重放查询失败: %v", err)
	}
	defer rows.Close()

	var replayed []string
	for rows.Next() {
		var sequence int64
		var eventType string
		if err := rows.Scan(&sequence, &eventType); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		replayed = append(replayed, eventType)
	}
	if strings.Join(replayed, ",") != "assistant.message,turn.completed" {
		t.Errorf("从 2 号之后重放得到 %v", replayed)
	}
}

// 时间戳统一走同一套格式，事件也不例外。
func TestEventTimestampUsesTheSharedLayout(t *testing.T) {
	if _, err := time.Parse(timeLayout, formatTime(fixedTime)); err != nil {
		t.Fatalf("事件时间戳格式不可解析: %v", err)
	}
}
