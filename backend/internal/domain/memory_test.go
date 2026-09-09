package domain

import "testing"

// batch_id 会出现在发给模型的摘要标签里，模型再原样填回工具参数——
// 也就是说它是一条**外部输入**，必须先校验格式再拿去查仓库。
func TestMemoryBatchIDValidatesLikeOtherIDs(t *testing.T) {
	generated, err := NewMemoryBatchID()
	if err != nil {
		t.Fatalf("生成 ID 失败: %v", err)
	}
	if err := generated.Validate(); err != nil {
		t.Errorf("自己生成的 ID 没通过校验: %v", err)
	}

	bad := []MemoryBatchID{
		"",
		"mem_",
		"ses_0123456789abcdef0123456789abcdef", // 别的类型的 ID
		"mem_0123456789ABCDEF0123456789abcdef", // 大写
		"mem_0123456789abcdef0123456789abcde",  // 短一位
		"mem_../../etc/passwd",                 // 路径
	}
	for _, id := range bad {
		if err := id.Validate(); err == nil {
			t.Errorf("%q 通过了校验", id)
		}
	}
}

// Title 取首个非空行：压缩 prompt 要求模型第一行写 Topic。
func TestBatchTitleUsesTheFirstNonEmptyLine(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"正常":    {"Topic: 排查登录失败\n随后是细节", "Topic: 排查登录失败"},
		"前有空行":  {"\n\n  Topic: 改配置  \n细节", "Topic: 改配置"},
		"只有空白":  {"   \n\t\n", "（空摘要）"},
		"完全为空":  {"", "（空摘要）"},
		"单行无换行": {"就一行", "就一行"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := (MemoryBatch{Content: testCase.content}).Title(); got != testCase.want {
				t.Errorf("Title() = %q; want %q", got, testCase.want)
			}
		})
	}
}

// 覆盖范围是右开区间，相邻节点首尾相接。
func TestBatchCoversCountsTheHalfOpenRange(t *testing.T) {
	if covered := (MemoryBatch{StartMessageIndex: 4, EndMessageIndex: 10}).Covers(); covered != 6 {
		t.Errorf("Covers() = %d; want 6", covered)
	}
	if covered := (MemoryBatch{StartMessageIndex: 4, EndMessageIndex: 4}).Covers(); covered != 0 {
		t.Errorf("空区间 Covers() = %d; want 0", covered)
	}
}

// 前沿里出现找不到的 ID 时跳过而不是崩：原始消息都还在，
// 少一段背景摘要也比整个会话打不开要好。
func TestActiveBatchesSkipsMissingNodes(t *testing.T) {
	memory := ConversationMemory{
		Batches:        []MemoryBatch{{ID: "mem_a", Content: "在的"}},
		ActiveBatchIDs: []MemoryBatchID{"mem_a", "mem_gone"},
	}

	active := memory.ActiveBatches()

	if len(active) != 1 || active[0].ID != "mem_a" {
		t.Errorf("ActiveBatches() = %+v; want 只有 mem_a", active)
	}
}

// 前沿按覆盖顺序排列，返回的切片必须保持这个顺序——
// 摘要进入上下文的先后就是对话发生的先后。
func TestActiveBatchesKeepsFrontierOrder(t *testing.T) {
	memory := ConversationMemory{
		Batches: []MemoryBatch{
			{ID: "mem_second", StartMessageIndex: 10, EndMessageIndex: 20},
			{ID: "mem_first", StartMessageIndex: 0, EndMessageIndex: 10},
		},
		ActiveBatchIDs: []MemoryBatchID{"mem_first", "mem_second"},
	}

	active := memory.ActiveBatches()

	if len(active) != 2 || active[0].ID != "mem_first" || active[1].ID != "mem_second" {
		t.Errorf("顺序 = %+v; want first 在前", active)
	}
}

// 零值记忆（从没压缩过的会话）满足不变量：空前沿覆盖 [0,0)，游标也是 0。
func TestZeroMemorySatisfiesTheInvariant(t *testing.T) {
	if err := (ConversationMemory{}).CheckInvariant(); err != nil {
		t.Errorf("零值记忆不满足不变量: %v", err)
	}
}

// 无缝覆盖到游标就是合法的，哪怕前沿有好几段。
func TestSeamlessFrontierSatisfiesTheInvariant(t *testing.T) {
	memory := ConversationMemory{
		RawCompactionCursor: 30,
		Batches: []MemoryBatch{
			{ID: "mem_a", StartMessageIndex: 0, EndMessageIndex: 10},
			{ID: "mem_b", StartMessageIndex: 10, EndMessageIndex: 24},
			{ID: "mem_c", StartMessageIndex: 24, EndMessageIndex: 30},
		},
		ActiveBatchIDs: []MemoryBatchID{"mem_a", "mem_b", "mem_c"},
	}

	if err := memory.CheckInvariant(); err != nil {
		t.Errorf("合法的前沿被判为非法: %v", err)
	}
}

// 前沿顺序错了也要被抓出来：区间本身首尾相接，但排列顺序不对，
// 拼起来就不是 [0, cursor) 了。
func TestInvariantRejectsOutOfOrderFrontier(t *testing.T) {
	memory := ConversationMemory{
		RawCompactionCursor: 20,
		Batches: []MemoryBatch{
			{ID: "mem_a", StartMessageIndex: 0, EndMessageIndex: 10},
			{ID: "mem_b", StartMessageIndex: 10, EndMessageIndex: 20},
		},
		ActiveBatchIDs: []MemoryBatchID{"mem_b", "mem_a"},
	}

	if err := memory.CheckInvariant(); err == nil {
		t.Error("顺序颠倒的前沿通过了检查")
	}
}

// —— M4.4：人工修订 ——

// 生效的正文：有修订用修订，没有用原文。Content 那一列永远是模型当初写的东西。
func TestEffectiveContentPrefersTheEdit(t *testing.T) {
	batch := MemoryBatch{Content: "模型写的"}
	if got := batch.EffectiveContent(); got != "模型写的" {
		t.Errorf("没改过时 = %q; want 原文", got)
	}
	if batch.Edited() {
		t.Error("没改过却报告 Edited")
	}

	batch.EditedContent = "人改的"
	if got := batch.EffectiveContent(); got != "人改的" {
		t.Errorf("改过之后 = %q; want 修订版", got)
	}
	if !batch.Edited() {
		t.Error("改过却没报告 Edited")
	}
	// 原文一个字都不能动——它是"模型当初生成了什么"的唯一记录。
	if batch.Content != "模型写的" {
		t.Errorf("修订污染了原文: %q", batch.Content)
	}
}

// 标题取的是**生效**正文的首行：改了摘要，界面上那行标题也要跟着变。
func TestTitleFollowsTheEdit(t *testing.T) {
	batch := MemoryBatch{Content: "Topic: 原来的主题\n细节", EditedContent: "Topic: 改过的主题\n细节"}

	if got := batch.Title(); got != "Topic: 改过的主题" {
		t.Errorf("Title() = %q; want 修订版的首行", got)
	}
}

func activeMemory() ConversationMemory {
	return ConversationMemory{
		RawCompactionCursor: 20,
		Batches: []MemoryBatch{
			{ID: "mem_child", StartMessageIndex: 0, EndMessageIndex: 10, Content: "子节点"},
			{ID: "mem_active", StartMessageIndex: 0, EndMessageIndex: 20, Content: "活跃节点",
				SourceBatchIDs: []MemoryBatchID{"mem_child"}},
		},
		ActiveBatchIDs: []MemoryBatchID{"mem_active"},
	}
}

// 只有活跃前沿上的节点可以改。
func TestOnlyActiveBatchesCanBeEdited(t *testing.T) {
	memory := activeMemory()

	updated, err := memory.EditBatch("mem_active", "改过了")
	if err != nil {
		t.Fatalf("改活跃节点失败: %v", err)
	}
	batch, _ := updated.Batch("mem_active")
	if batch.EffectiveContent() != "改过了" {
		t.Errorf("修订没生效: %q", batch.EffectiveContent())
	}

	// 已经被合并进上层的节点：改它模型看到的东西一个字都不会变，
	// 允许改等于给用户一个假的反馈。
	if _, err := memory.EditBatch("mem_child", "偷偷改"); err == nil {
		t.Error("改非活跃节点竟然成功了")
	}
	if _, err := memory.EditBatch("mem_nope", "不存在"); err == nil {
		t.Error("改不存在的节点竟然成功了")
	}
}

// 空串表示撤销修订，回到模型原始那一版。
func TestEditingWithEmptyContentRevertsToTheOriginal(t *testing.T) {
	memory, err := activeMemory().EditBatch("mem_active", "改过了")
	if err != nil {
		t.Fatalf("EditBatch 返回错误: %v", err)
	}

	reverted, err := memory.EditBatch("mem_active", "")
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	batch, _ := reverted.Batch("mem_active")
	if batch.Edited() {
		t.Error("撤销之后仍然标记为已修订")
	}
	if batch.EffectiveContent() != "活跃节点" {
		t.Errorf("撤销之后 = %q; want 模型原文", batch.EffectiveContent())
	}
}

// EditBatch 不修改接收者，也不动覆盖范围、子节点、层级和不变量。
func TestEditBatchDoesNotMutateTheReceiverOrTheStructure(t *testing.T) {
	memory := activeMemory()

	updated, err := memory.EditBatch("mem_active", "改过了")
	if err != nil {
		t.Fatalf("EditBatch 返回错误: %v", err)
	}

	before, _ := memory.Batch("mem_active")
	if before.Edited() {
		t.Error("原来那份记忆被就地改了")
	}
	after, _ := updated.Batch("mem_active")
	if after.StartMessageIndex != before.StartMessageIndex ||
		after.EndMessageIndex != before.EndMessageIndex ||
		after.Level != before.Level ||
		len(after.SourceBatchIDs) != len(before.SourceBatchIDs) {
		t.Errorf("修订动到了结构: %+v vs %+v", after, before)
	}
	// 修订不改覆盖范围，因此唯一的不变量必须原样成立。
	if err := updated.CheckInvariant(); err != nil {
		t.Errorf("修订破坏了不变量: %v", err)
	}
}
