package store

import (
	"errors"
	"os"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// 归档只改一列，列表默认看不到它，但数据一条不动。
func TestArchiveHidesFromListWithoutTouchingData(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	before, err := store.List(false)
	if err != nil || len(before) != 1 {
		t.Fatalf("归档前列表 = %+v, err = %v", before, err)
	}

	if err := store.SetArchived(session.ID, true); err != nil {
		t.Fatalf("SetArchived 返回错误: %v", err)
	}

	visible, err := store.List(false)
	if err != nil {
		t.Fatalf("List 返回错误: %v", err)
	}
	if len(visible) != 0 {
		t.Errorf("归档之后默认列表里还有 %d 个", len(visible))
	}

	all, err := store.List(true)
	if err != nil {
		t.Fatalf("List(true) 返回错误: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("显式索取归档的也只看到 %d 个", len(all))
	}
	if !all[0].Archived {
		t.Error("没有标记为已归档")
	}
	if all[0].ArchivedAt.IsZero() {
		t.Error("归档时间没有记下来")
	}
	// 数据一条不动：消息还在。
	if all[0].MessageCount != 3 {
		t.Errorf("归档动了消息：%d 条", all[0].MessageCount)
	}
}

// 归档是可逆的。
func TestArchiveIsReversible(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	if err := store.SetArchived(session.ID, true); err != nil {
		t.Fatalf("归档失败: %v", err)
	}

	if err := store.SetArchived(session.ID, false); err != nil {
		t.Fatalf("取消归档失败: %v", err)
	}

	visible, _ := store.List(false)
	if len(visible) != 1 {
		t.Errorf("取消归档之后列表里有 %d 个; want 1", len(visible))
	}
	if visible[0].Archived {
		t.Error("仍然标记为已归档")
	}
}

// 自定义标题覆盖派生标题；空串恢复派生。
func TestCustomTitleOverridesTheDerivedOne(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	derived, _ := store.List(false)
	if derived[0].CustomTitle {
		t.Error("派生标题被标记成了自定义")
	}

	if err := store.SetTitle(session.ID, "我起的名字"); err != nil {
		t.Fatalf("SetTitle 返回错误: %v", err)
	}
	renamed, _ := store.List(false)
	if renamed[0].Title != "我起的名字" || !renamed[0].CustomTitle {
		t.Errorf("重命名之后 = %+v", renamed[0])
	}

	if err := store.SetTitle(session.ID, ""); err != nil {
		t.Fatalf("恢复派生标题失败: %v", err)
	}
	restored, _ := store.List(false)
	if restored[0].Title != derived[0].Title || restored[0].CustomTitle {
		t.Errorf("恢复之后 = %+v; want 回到派生标题 %q", restored[0], derived[0].Title)
	}
}

// 改不存在的会话要报错。静默成功会让界面显示"已归档"而刷新之后什么都没变。
func TestUpdatingAMissingSessionFails(t *testing.T) {
	store, _ := newTestStore(t)
	missing := domain.SessionID("ses_ffffffffffffffffffffffffffffffff")

	if err := store.SetArchived(missing, true); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SetArchived 返回 %v; want ErrSessionNotFound", err)
	}
	if err := store.SetTitle(missing, "x"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SetTitle 返回 %v; want ErrSessionNotFound", err)
	}
	if err := store.Delete(missing); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Delete 返回 %v; want ErrSessionNotFound", err)
	}
}

// 删除要把消息、事件、摘要一起带走（靠外键级联），锁文件也不留。
func TestDeleteCascadesAndRemovesTheLockFile(t *testing.T) {
	store, directory := newTestStore(t)
	session := sampleSession(t, store)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2,
		Batches:             []domain.MemoryBatch{batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)},
		ActiveBatchIDs:      []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}
	if _, err := store.Save(session, []domain.RunEvent{durableEvent(domain.EventTurnStarted, nil)}); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	lockPath := store.lockPath(session.ID)
	// 关掉才能删——Delete 会拒绝删自己正持有的会话。
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("锁文件本该存在: %v", err)
	}

	reopened := openStoreAt(t, directory, fixedTime)
	if err := reopened.Delete(session.ID); err != nil {
		t.Fatalf("Delete 返回错误: %v", err)
	}

	for _, table := range []string{"sessions", "messages", "events", "memory_batches"} {
		var count int
		if err := reopened.connection.QueryRow(
			"SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("统计 %s 失败: %v", table, err)
		}
		if count != 0 {
			t.Errorf("删除之后 %s 还剩 %d 行", table, count)
		}
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("锁文件残留: %v", err)
	}
}

// 不能删自己正持有的会话——那会留下一个对着不存在的会话继续写的写者。
func TestDeleteRefusesTheSessionItHolds(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	err := store.Delete(session.ID)

	if err == nil {
		t.Fatal("删掉了自己正持有的会话")
	}
	if !strings.Contains(err.Error(), "Close") {
		t.Errorf("错误没有说明该怎么做: %v", err)
	}
}

// 会话 ID 是外部输入，三个操作都要先校验格式。
func TestLifecycleOperationsValidateTheSessionID(t *testing.T) {
	store, _ := newTestStore(t)
	bad := domain.SessionID("../../etc/passwd")

	if err := store.SetArchived(bad, true); err == nil {
		t.Error("SetArchived 没有校验 ID")
	}
	if err := store.SetTitle(bad, "x"); err == nil {
		t.Error("SetTitle 没有校验 ID")
	}
	if err := store.Delete(bad); err == nil {
		t.Error("Delete 没有校验 ID")
	}
}
