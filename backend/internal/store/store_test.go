package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// fixedTime 让测试里的时间戳可预期。
var fixedTime = time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC)

// newTestStore 在临时目录上建一个使用固定时钟的 Store，同时返回数据目录，
// 便于需要"关掉再打开"的测试重新连上同一个库。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	directory := t.TempDir()
	return openStoreAt(t, directory, fixedTime), directory
}

// openStoreAt 在指定数据目录上建一个时钟固定在给定时刻的 Store。
func openStoreAt(t *testing.T, directory string, at time.Time) *Store {
	t.Helper()
	store, err := New(directory)
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	store.now = func() time.Time { return at }
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// sampleSession 造一个含工具调用与观察的会话，覆盖需要落库的全部字段。
func sampleSession(t *testing.T, store *Store) *domain.Session {
	t.Helper()
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	session.Append(domain.Message{Role: domain.RoleUser, Content: "看看当前目录"})
	session.Append(domain.Message{
		Role:    domain.RoleAssistant,
		Content: "我看一下",
		ToolCalls: []domain.ToolCall{
			{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls && pwd"}`)},
		},
	})
	session.Append(domain.Message{
		Role:       domain.RoleTool,
		Content:    `{"status":"success","content":"a b c"}`,
		ToolCallID: "call-1",
	})
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	return session
}

// 写进去再读回来，每个字段都要一致——这是持久化的最基本要求。
func TestSaveThenLoadRoundTripsEveryField(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	original := sampleSession(t, store)
	original.PendingToolCallID = "call-2"
	if _, err := store.Save(original, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, dataDirectory, fixedTime)
	loaded, err := reopened.Load(original.ID)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}

	if loaded.ID != original.ID {
		t.Errorf("ID = %q; want %q", loaded.ID, original.ID)
	}
	if loaded.Workspace != original.Workspace {
		t.Errorf("Workspace = %q; want %q", loaded.Workspace, original.Workspace)
	}
	if loaded.PendingToolCallID != "call-2" {
		t.Errorf("PendingToolCallID = %q; want call-2", loaded.PendingToolCallID)
	}
	if !loaded.CreatedAt.Equal(fixedTime) || !loaded.UpdatedAt.Equal(fixedTime) {
		t.Errorf("时间戳 = %v / %v; want %v", loaded.CreatedAt, loaded.UpdatedAt, fixedTime)
	}

	history := loaded.Messages()
	if len(history) != 3 {
		t.Fatalf("读回 %d 条消息; want 3", len(history))
	}
	if history[0].Role != domain.RoleUser || history[0].Content != "看看当前目录" {
		t.Errorf("第一条 = %+v", history[0])
	}
	if len(history[1].ToolCalls) != 1 {
		t.Fatalf("assistant 消息丢失了工具调用: %+v", history[1])
	}
	// 参数必须逐字节一致：这些字节会原样回写进下一次请求，被重新格式化就打不中
	// 供应商的 prompt 缓存。
	call := history[1].ToolCalls[0]
	if call.ID != "call-1" || call.Name != "bash" {
		t.Errorf("工具调用 = %+v", call)
	}
	if string(call.Arguments) != `{"command":"ls && pwd"}` {
		t.Errorf("参数被改写成 %s", call.Arguments)
	}
	if history[2].Role != domain.RoleTool || history[2].ToolCallID != "call-1" {
		t.Errorf("tool 消息 = %+v", history[2])
	}
}

// 保存只写新增的消息。若每次都重写全部，第二次保存会撞上 (session_id, seq) 主键。
func TestSaveOnlyInsertsNewMessages(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("没有新增消息时再次保存失败（说明重复插入了）: %v", err)
	}

	session.Append(domain.Message{Role: domain.RoleAssistant, Content: "看完了"})
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("追加一条后保存失败: %v", err)
	}

	var count int
	if err := store.connection.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, string(session.ID)).Scan(&count); err != nil {
		t.Fatalf("统计消息失败: %v", err)
	}
	if count != 4 {
		t.Errorf("数据库里有 %d 条消息; want 4", count)
	}
}

// 一次保存里的消息和会话状态在同一个事务里：任何一条写不进去，整批都不生效。
func TestSaveIsAtomicAcrossMessagesAndSessionState(t *testing.T) {
	store, _ := newTestStore(t)
	session, err := store.Create("/tmp/goseek-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}

	session.Append(domain.Message{Role: domain.RoleUser, Content: "第一条，本该写进去"})
	// system 不是合法的会话角色，CHECK 约束会拒绝它。
	session.Append(domain.Message{Role: domain.Role("system"), Content: "第二条，会被拒绝"})
	session.PendingToolCallID = "call-x"

	if _, err := store.Save(session, nil); err == nil {
		t.Fatal("非法角色被写进了数据库")
	}

	var messageCount int
	if err := store.connection.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, string(session.ID)).Scan(&messageCount); err != nil {
		t.Fatalf("统计消息失败: %v", err)
	}
	if messageCount != 0 {
		t.Errorf("失败的事务留下了 %d 条消息; want 0", messageCount)
	}

	var pending string
	if err := store.connection.QueryRow(
		`SELECT pending_tool_call_id FROM sessions WHERE id = ?`, string(session.ID)).Scan(&pending); err != nil {
		t.Fatalf("读取会话状态失败: %v", err)
	}
	if pending != "" {
		t.Errorf("失败的事务改动了会话状态: pending = %q", pending)
	}
}

// 保存必须针对当前打开的会话，否则会把消息写到别人的历史里。
func TestSaveRejectsForeignSession(t *testing.T) {
	store, _ := newTestStore(t)
	sampleSession(t, store)

	other := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp/other")
	if _, err := store.Save(other, nil); err == nil {
		t.Error("往未打开的会话保存没有报错")
	}
}

// 历史只能追加。条数变少说明调用方拿错了对象，此时继续写会错位。
func TestSaveRejectsShrunkHistory(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	truncated := domain.LoadSession(session.ID, session.Workspace, session.Messages()[:1])
	if _, err := store.Save(truncated, nil); err == nil {
		t.Error("历史变短时保存没有报错")
	}
}

func TestLoadReportsMissingSession(t *testing.T) {
	store, _ := newTestStore(t)
	missing, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID 返回错误: %v", err)
	}

	// 用哨兵错误而不是比对文案：HTTP 层要靠 errors.Is 把它映射成 404。
	if _, err := store.Load(missing); err == nil {
		t.Fatal("加载不存在的会话没有报错")
	} else if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("错误 = %v; want ErrSessionNotFound", err)
	}
}

// 数据库可以被 sqlite3 直接改，读出来的角色仍然是外部输入。
func TestLoadRejectsUnknownRoleWrittenBehindOurBack(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session := sampleSession(t, store)

	// 绕过 CHECK 约束的唯一办法是先关掉它；这里直接改已有行来模拟外部改动。
	if _, err := store.connection.Exec(
		`UPDATE messages SET role = 'assistant' WHERE session_id = ? AND seq = 0`,
		string(session.ID)); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	// 换个角度：让 tool_calls 变成非法 JSON，同样是外部改动。
	if _, err := store.connection.Exec(
		`UPDATE messages SET tool_calls = '{不是 JSON' WHERE session_id = ? AND seq = 1`,
		string(session.ID)); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, dataDirectory, fixedTime)
	if _, err := reopened.Load(session.ID); err == nil {
		t.Fatal("非法的工具调用被接受了")
	} else if !strings.Contains(err.Error(), "合法 JSON") {
		t.Errorf("错误信息 = %q", err.Error())
	}
}

// 消息序号有缺口说明历史被外部改过，不能接受一段有洞的历史继续跑。
func TestLoadRejectsGapInMessageSequence(t *testing.T) {
	store, dataDirectory := newTestStore(t)
	session := sampleSession(t, store)

	if _, err := store.connection.Exec(
		`DELETE FROM messages WHERE session_id = ? AND seq = 1`, string(session.ID)); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, dataDirectory, fixedTime)
	if _, err := reopened.Load(session.ID); err == nil {
		t.Fatal("有缺口的历史被接受了")
	} else if !strings.Contains(err.Error(), "不连续") {
		t.Errorf("错误信息 = %q", err.Error())
	}
}

// List 按最后活动倒序返回，并从首条用户消息取标题。
func TestListSortsByLastActivityAndTakesTitleFromFirstUserMessage(t *testing.T) {
	directory := t.TempDir()

	older := openStoreAt(t, directory, fixedTime.Add(-2*time.Hour))
	olderSession := sampleSession(t, older)
	if err := older.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	newer := openStoreAt(t, directory, fixedTime)
	newerSession, err := newer.Create("/tmp/newer-work", "")
	if err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}
	newerSession.Append(domain.Message{Role: domain.RoleAssistant, Content: "assistant 先说话"})
	newerSession.Append(domain.Message{Role: domain.RoleUser, Content: "  第二个会话的\n第一句话  "})
	if _, err := newer.Save(newerSession, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	summaries, err := newer.List(false)
	if err != nil {
		t.Fatalf("List 返回错误: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("List 返回 %d 项; want 2", len(summaries))
	}
	if summaries[0].ID != newerSession.ID || summaries[1].ID != olderSession.ID {
		t.Errorf("顺序不是按最后活动倒序: %q、%q", summaries[0].ID, summaries[1].ID)
	}
	// 标题必须是首条 user 消息，不能被排在它前面的 assistant 消息顶替。
	if summaries[0].Title != "第二个会话的 第一句话" {
		t.Errorf("标题 = %q", summaries[0].Title)
	}
	if summaries[0].MessageCount != 2 {
		t.Errorf("消息条数 = %d; want 2", summaries[0].MessageCount)
	}
	if summaries[0].Workspace != "/tmp/newer-work" {
		t.Errorf("Workspace = %q", summaries[0].Workspace)
	}
	if !summaries[1].UpdatedAt.Equal(fixedTime.Add(-2 * time.Hour)) {
		t.Errorf("较早会话的时间 = %v", summaries[1].UpdatedAt)
	}
}

func TestListOnEmptyDatabaseReturnsNothing(t *testing.T) {
	store, _ := newTestStore(t)

	summaries, err := store.List(false)
	if err != nil {
		t.Fatalf("List 返回错误: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("空库返回了 %d 项", len(summaries))
	}
}

// 还没说话的会话也要有一个能看的标题。
func TestListDescribesSessionWithoutAnyUserMessage(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Create("/tmp/goseek-work", ""); err != nil {
		t.Fatalf("Create 返回错误: %v", err)
	}

	summaries, err := store.List(false)
	if err != nil {
		t.Fatalf("List 返回错误: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Title == "" {
		t.Fatalf("空会话的标题为空: %+v", summaries)
	}
	if summaries[0].MessageCount != 0 {
		t.Errorf("空会话的消息数 = %d", summaries[0].MessageCount)
	}
}

// 时间戳存成 UTC 的 RFC3339，字典序正好等于时间序，ORDER BY 才成立。
func TestTimestampsAreStoredAsSortableUTC(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	var createdAt string
	if err := store.connection.QueryRow(
		`SELECT created_at FROM sessions WHERE id = ?`, string(session.ID)).Scan(&createdAt); err != nil {
		t.Fatalf("读取时间戳失败: %v", err)
	}
	if !strings.HasSuffix(createdAt, "Z") {
		t.Errorf("created_at = %q; want 以 Z 结尾的 UTC 时间", createdAt)
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		t.Fatalf("时间戳无法解析: %v", err)
	}
	if !parsed.Equal(fixedTime) {
		t.Errorf("时间戳 = %v; want %v", parsed, fixedTime)
	}
}
