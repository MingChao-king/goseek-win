package store

import (
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// batchAt 造一个内容和范围都可辨认的摘要节点。
func batchAt(id string, level, start, end int, sources ...domain.MemoryBatchID) domain.MemoryBatch {
	return domain.MemoryBatch{
		ID:                domain.MemoryBatchID(id),
		Level:             level,
		Content:           "摘要 " + id,
		StartMessageIndex: start,
		EndMessageIndex:   end,
		SourceBatchIDs:    sources,
		// 秒级精度：落库用的时间格式只到秒，用带纳秒的时间会导致
		// 读回来跟写进去不相等，那是测试的问题不是代码的问题。
		CreatedAt: fixedTime.Add(time.Duration(level) * time.Second),
	}
}

// 摘要树连同活跃前沿和游标一起落库，读回来必须逐字段一致。
// 这是"关掉进程明天再继续"能用的前提：压缩过的会话恢复后不能退回未压缩状态，
// 否则第一次请求就会因为超窗而失败。
func TestMemoryRoundTripsThroughTheDatabase(t *testing.T) {
	store, directory := newTestStore(t)
	session := sampleSession(t, store)
	leafOne := batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)
	leafTwo := batchAt("mem_0000000000000000000000000000000b", 0, 2, 4)
	parent := batchAt("mem_0000000000000000000000000000000c", 1, 0, 4, leafOne.ID, leafTwo.ID)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 4,
		Batches:             []domain.MemoryBatch{leafOne, leafTwo, parent},
		ActiveBatchIDs:      []domain.MemoryBatchID{parent.ID},
	}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, directory, fixedTime)
	loaded, err := reopened.Load(session.ID)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}

	memory := loaded.Memory
	if memory.RawCompactionCursor != 4 {
		t.Errorf("游标 = %d; want 4", memory.RawCompactionCursor)
	}
	if len(memory.ActiveBatchIDs) != 1 || memory.ActiveBatchIDs[0] != parent.ID {
		t.Errorf("活跃前沿 = %v; want [%s]", memory.ActiveBatchIDs, parent.ID)
	}
	if len(memory.Batches) != 3 {
		t.Fatalf("读回 %d 个节点; want 3", len(memory.Batches))
	}
	for _, want := range []domain.MemoryBatch{leafOne, leafTwo, parent} {
		got, found := memory.Batch(want.ID)
		if !found {
			t.Errorf("节点 %s 丢失", want.ID)
			continue
		}
		if got.Level != want.Level || got.Content != want.Content ||
			got.StartMessageIndex != want.StartMessageIndex ||
			got.EndMessageIndex != want.EndMessageIndex ||
			!got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("节点 %s 读回 %+v; want %+v", want.ID, got, want)
		}
		if len(got.SourceBatchIDs) != len(want.SourceBatchIDs) {
			t.Errorf("节点 %s 的子节点 = %v; want %v", want.ID, got.SourceBatchIDs, want.SourceBatchIDs)
		}
	}
	// 恢复出来的记忆必须仍然满足不变量，否则下一次压缩会从一个坏状态出发。
	if err := memory.CheckInvariant(); err != nil {
		t.Errorf("恢复后不变量被破坏: %v", err)
	}
}

// 摘要节点不可变，因此保存只插入新增的那些。重复保存不能重复插入——
// 主键会直接拒绝，测试同时验证了这个保护确实生效。
func TestSaveOnlyInsertsNewMemoryBatches(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2,
		Batches:             []domain.MemoryBatch{batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)},
		ActiveBatchIDs:      []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("第一次 Save 返回错误: %v", err)
	}

	// 记忆没有任何变化时再存一次：不能报错，也不能插入第二份。
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("第二次 Save 返回错误: %v", err)
	}

	var count int
	if err := store.connection.QueryRow(
		`SELECT count(*) FROM memory_batches WHERE session_id = ?`, string(session.ID)).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Errorf("库里有 %d 个节点; want 1", count)
	}
}

// 活跃前沿是可变的——每次压缩都会换一批。它存在 sessions 表的一列上，
// 是覆盖写而不是追加。
func TestActiveFrontierIsOverwrittenOnEachSave(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	leafOne := batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)
	leafTwo := batchAt("mem_0000000000000000000000000000000b", 0, 2, 4)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 4,
		Batches:             []domain.MemoryBatch{leafOne, leafTwo},
		ActiveBatchIDs:      []domain.MemoryBatchID{leafOne.ID, leafTwo.ID},
	}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	// 一次合并：两个叶子换成一个父节点，前沿从两项变一项。
	parent := batchAt("mem_0000000000000000000000000000000c", 1, 0, 4, leafOne.ID, leafTwo.ID)
	session.Memory.Batches = append(session.Memory.Batches, parent)
	session.Memory.ActiveBatchIDs = []domain.MemoryBatchID{parent.ID}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("合并后 Save 返回错误: %v", err)
	}

	var encoded string
	if err := store.connection.QueryRow(
		`SELECT active_batch_ids FROM sessions WHERE id = ?`, string(session.ID)).Scan(&encoded); err != nil {
		t.Fatalf("读取前沿失败: %v", err)
	}
	if strings.Contains(encoded, string(leafOne.ID)) {
		t.Errorf("旧前沿没有被覆盖: %s", encoded)
	}
	if !strings.Contains(encoded, string(parent.ID)) {
		t.Errorf("新前沿没有写进去: %s", encoded)
	}
	// 被换下去的叶子仍然留在库里，回查才走得下去。
	var count int
	if err := store.connection.QueryRow(
		`SELECT count(*) FROM memory_batches WHERE session_id = ?`, string(session.ID)).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 3 {
		t.Errorf("库里有 %d 个节点; want 3（父节点不覆盖子节点）", count)
	}
}

// 从没压缩过的会话读回来是零值记忆，而不是解析空串失败。
func TestSessionWithoutMemoryLoadsAsZeroValue(t *testing.T) {
	store, directory := newTestStore(t)
	session := sampleSession(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, directory, fixedTime)
	loaded, err := reopened.Load(session.ID)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}

	if loaded.Memory.RawCompactionCursor != 0 || len(loaded.Memory.Batches) != 0 ||
		len(loaded.Memory.ActiveBatchIDs) != 0 {
		t.Errorf("没压缩过的会话读出了记忆: %+v", loaded.Memory)
	}
	if err := loaded.Memory.CheckInvariant(); err != nil {
		t.Errorf("零值记忆不满足不变量: %v", err)
	}
}

// 摘要节点的外键指向会话：删会话要连带删掉它的记忆，不留孤儿行。
func TestDeletingASessionCascadesToItsMemory(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2,
		Batches:             []domain.MemoryBatch{batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)},
		ActiveBatchIDs:      []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	if _, err := store.connection.Exec(`DELETE FROM sessions WHERE id = ?`, string(session.ID)); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}

	var count int
	if err := store.connection.QueryRow(`SELECT count(*) FROM memory_batches`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 0 {
		t.Errorf("会话删掉后还剩 %d 个摘要节点", count)
	}
}

// —— M4.4：人工修订 ——

// 修订落库之后能读回，而且**原文一个字没动**。
func TestEditedContentRoundTripsAndLeavesTheOriginalAlone(t *testing.T) {
	store, directory := newTestStore(t)
	session := sampleSession(t, store)
	batch := batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2,
		Batches:             []domain.MemoryBatch{batch},
		ActiveBatchIDs:      []domain.MemoryBatchID{batch.ID},
	}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	if err := store.EditMemoryBatch(session.ID, batch.ID, "人工改过的正文"); err != nil {
		t.Fatalf("EditMemoryBatch 返回错误: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	reopened := openStoreAt(t, directory, fixedTime)
	loaded, err := reopened.Load(session.ID)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	got, found := loaded.Memory.Batch(batch.ID)
	if !found {
		t.Fatal("节点丢了")
	}
	if got.EditedContent != "人工改过的正文" {
		t.Errorf("修订 = %q; want 人工改过的正文", got.EditedContent)
	}
	// 这是整套设计的关键：content 那一列永不修改。
	if got.Content != batch.Content {
		t.Errorf("原文被改成了 %q; want %q", got.Content, batch.Content)
	}
	if got.EffectiveContent() != "人工改过的正文" {
		t.Errorf("生效正文 = %q", got.EffectiveContent())
	}
}

// 空串撤销修订。
func TestEditingWithEmptyContentClearsTheStoredEdit(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)
	batch := batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2,
		Batches:             []domain.MemoryBatch{batch},
		ActiveBatchIDs:      []domain.MemoryBatchID{batch.ID},
	}
	if _, err := store.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}
	if err := store.EditMemoryBatch(session.ID, batch.ID, "改过"); err != nil {
		t.Fatalf("EditMemoryBatch 返回错误: %v", err)
	}

	if err := store.EditMemoryBatch(session.ID, batch.ID, ""); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	var stored string
	if err := store.connection.QueryRow(
		`SELECT edited_content FROM memory_batches WHERE session_id = ? AND id = ?`,
		string(session.ID), string(batch.ID)).Scan(&stored); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if stored != "" {
		t.Errorf("撤销之后库里还留着 %q", stored)
	}
}

// 改一个不存在的节点必须报错。静默成功会让界面显示"已保存"，
// 而刷新之后修订不见了——那种 bug 最难查。
func TestEditingAMissingBatchFails(t *testing.T) {
	store, _ := newTestStore(t)
	session := sampleSession(t, store)

	err := store.EditMemoryBatch(session.ID, "mem_0000000000000000000000000000ffff", "改")

	if err == nil {
		t.Error("改不存在的节点竟然成功了")
	}
}

// 修订不能跨会话：只能改当前打开的那个会话的节点。
func TestEditingRejectsAForeignSession(t *testing.T) {
	store, _ := newTestStore(t)
	sampleSession(t, store)

	err := store.EditMemoryBatch("ses_ffffffffffffffffffffffffffffffff",
		"mem_0000000000000000000000000000000a", "改")

	if err == nil {
		t.Error("改别的会话的节点竟然成功了")
	}
}

// v4 能加在一个已有数据的 v3 库上：老会话的摘要读回来就是"没改过"。
//
// 单独测 v3→v4 而不是只靠"从 v1 一路升上来"那条：真实用户的库是 v3 且**有数据**，
// 而那条测试是从空库回退出来的。加列时若写错默认值，空库看不出问题，有数据的库
// 会直接读失败。
func TestMigrationV4AppliesToAV3DatabaseWithData(t *testing.T) {
	directory := t.TempDir()
	seed := openStoreAt(t, directory, fixedTime)
	session := sampleSession(t, seed)
	batch := batchAt("mem_0000000000000000000000000000000a", 0, 0, 2)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2,
		Batches:             []domain.MemoryBatch{batch},
		ActiveBatchIDs:      []domain.MemoryBatchID{batch.ID},
	}
	if _, err := seed.Save(session, nil); err != nil {
		t.Fatalf("Save 返回错误: %v", err)
	}

	// 退回 v3：把 v4 之后每一版加的列都删掉，版本号也退回去。
	//
	// 每加一版迁移，这里都要跟着加一行。看着啰嗦，但它正是这条测试的价值所在：
	// 它模拟的是**真实用户那份有数据的老库**，而不是从空库一路升上来的干净库。
	if _, err := seed.connection.Exec(
		`ALTER TABLE memory_batches DROP COLUMN edited_content;
		 ALTER TABLE sessions DROP COLUMN archived_at;
		 ALTER TABLE sessions DROP COLUMN title;
		 ALTER TABLE sessions DROP COLUMN quotes_collapsed_before;
		 ALTER TABLE sessions DROP COLUMN collapsed_quotes;
		 ALTER TABLE sessions DROP COLUMN model;
		 DROP TABLE message_images;
		 PRAGMA user_version = 3;`); err != nil {
		t.Fatalf("回退到 v3 失败: %v", err)
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

	loaded, err := upgraded.Load(session.ID)
	if err != nil {
		t.Fatalf("升级后 Load 失败: %v", err)
	}
	got, found := loaded.Memory.Batch(batch.ID)
	if !found {
		t.Fatal("升级之后节点丢了")
	}
	if got.Edited() {
		t.Errorf("老数据被读成了已修订: %q", got.EditedContent)
	}
	if got.EffectiveContent() != batch.Content {
		t.Errorf("生效正文 = %q; want %q", got.EffectiveContent(), batch.Content)
	}
	// 升级完还能正常写。
	if err := upgraded.EditMemoryBatch(session.ID, batch.ID, "升级后改的"); err != nil {
		t.Errorf("升级后修订失败: %v", err)
	}
}
