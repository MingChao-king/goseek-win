package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"goseek/internal/domain"
)

// 手动压缩和提交是同一类东西：投给 Runner、立刻返回 202、结果走事件流。
func TestCompactReturnsAcceptedImmediately(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/compact", "")

	if status != http.StatusAccepted {
		t.Fatalf("压缩返回 %d: %s", status, body)
	}
}

// 压缩没有参数，因此不需要请求体——带一个也不该让它失败。
func TestCompactIgnoresAnyRequestBody(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)

	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/compact", `{"随便":1}`); status != http.StatusAccepted {
		t.Fatalf("压缩返回 %d: %s", status, body)
	}
}

// 压缩改的是下一次请求要用的视图，和一轮交互并发做结果无从定义——
// 因此它和提交共用同一个 running 标记，一轮在跑时返回 409。
func TestCompactWhileATurnIsRunningReturnsConflict(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)

	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第一条"}`); status != http.StatusAccepted {
		t.Fatalf("提交返回 %d: %s", status, body)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/compact", "")

	if status != http.StatusConflict {
		t.Fatalf("轮次进行中压缩返回 %d: %s; want 409", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "turn_in_progress" {
		t.Errorf("code = %q; want turn_in_progress", failure.Error.Code)
	}

	close(block)
}

// 会话 ID 是外部输入，压缩这条路和别的路一样先校验再动手。
func TestCompactValidatesSessionID(t *testing.T) {
	ts := newTestServer(t, nil)

	status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/not-an-id/compact", "")

	if status != http.StatusBadRequest {
		t.Errorf("状态码 = %d; want 400", status)
	}
}

// 摘要树走只读路径，因此**一轮正在跑时也能查**——它不需要 Runner。
func TestMemoryTreeIsReadableWhileATurnIsRunning(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第一条"}`); status != http.StatusAccepted {
		t.Fatal("提交没有被接受")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if status, body := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/memory", ""); status != http.StatusOK {
			t.Errorf("读摘要树返回 %d: %s", status, body)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("一轮进行中读摘要树被阻塞了")
	}

	close(block)
}

// 没压缩过的会话返回一棵空树，而不是 null——前端不必在每个用到它的地方先判空。
func TestMemoryTreeOfAFreshSessionIsEmptyNotNull(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id+"/memory", "")
	if status != http.StatusOK {
		t.Fatalf("返回 %d: %s", status, body)
	}

	var tree memoryTree
	if err := json.Unmarshal(body, &tree); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if tree.Batches == nil || tree.ActiveBatchIDs == nil {
		t.Errorf("空树序列化成了 null: %s", body)
	}
	if tree.RawCompactionCursor != 1 {
		t.Errorf("游标 = %d; want 1（一条都没折叠）", tree.RawCompactionCursor)
	}
}

// 摘要树带**全部**节点和**摘要正文**——那正是它与快照里那个 memory 字段的区别。
func TestMemoryTreeCarriesEveryNodeWithContent(t *testing.T) {
	leafOne := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000a", Level: 0,
		Content: "Topic: 前半段\n细节若干", StartMessageIndex: 0, EndMessageIndex: 6,
	}
	leafTwo := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000b", Level: 0,
		Content: "Topic: 后半段", StartMessageIndex: 6, EndMessageIndex: 12,
	}
	parent := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000c", Level: 1,
		Content: "Topic: 合并", StartMessageIndex: 0, EndMessageIndex: 12,
		SourceBatchIDs: []domain.MemoryBatchID{leafOne.ID, leafTwo.ID},
	}

	tree := newMemoryTree(domain.ConversationMemory{
		RawCompactionCursor: 12,
		Batches:             []domain.MemoryBatch{leafOne, leafTwo, parent},
		// 前沿只有父节点，但树里三个都要在——被合并掉的子节点正是回查要走的路。
		ActiveBatchIDs: []domain.MemoryBatchID{parent.ID},
	})

	if len(tree.Batches) != 3 {
		t.Fatalf("树里有 %d 个节点; want 3（含已被合并的子节点）", len(tree.Batches))
	}
	if len(tree.ActiveBatchIDs) != 1 {
		t.Errorf("前沿 = %v; want 只有父节点", tree.ActiveBatchIDs)
	}
	byID := map[string]memoryNodeView{}
	for _, node := range tree.Batches {
		byID[node.ID] = node
	}
	if got := byID[string(leafOne.ID)].Content; got != leafOne.Content {
		t.Errorf("叶子的正文 = %q; want 原样带出", got)
	}
	if got := byID[string(parent.ID)].SourceBatchIDs; len(got) != 2 {
		t.Errorf("父节点的子节点 = %v; want 两个", got)
	}
	// 覆盖范围是给人看的闭区间，面板据此到快照的 messages 里取原文。
	if node := byID[string(leafTwo.ID)]; node.StartMessage != 7 || node.EndMessage != 12 {
		t.Errorf("覆盖范围 = [%d,%d]; want [7,12]", node.StartMessage, node.EndMessage)
	}
	// 叶子的 source_batch_ids 是空数组而不是 null。
	if byID[string(leafOne.ID)].SourceBatchIDs == nil {
		t.Error("叶子的子节点列表是 null")
	}
}

// —— M4.4：人工修订摘要 ——

// seedMemory 给一个会话塞进一棵两层的摘要树，返回活跃节点与已被合并的子节点。
//
// 直接写库而不是跑真实压缩：这些测试要验的是修订这条路，压缩本身在 contextmgr
// 那边已经测过了，在这里再跑一遍只会让测试变慢且依赖假模型的行为。
func seedMemory(t *testing.T, ts *testServer, id string) (active, merged domain.MemoryBatchID) {
	t.Helper()
	// 直接连库写。走 HTTP 也能造出摘要树，但那要跑一段足够长的真实对话再触发压缩，
	// 慢且依赖假模型的具体输出；这里要验的是修订这条路，压缩本身在 contextmgr
	// 那边已经测过了。
	database, err := sql.Open("sqlite", filepath.Join(ts.directory, "goseek.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer database.Close()
	active, merged = "mem_0000000000000000000000000000000c", "mem_0000000000000000000000000000000a"
	statements := []struct {
		id      domain.MemoryBatchID
		level   int
		content string
		start   int
		end     int
		sources string
	}{
		{merged, 0, "Topic: 子节点", 0, 2, ""},
		{active, 1, "Topic: 活跃节点", 0, 2, `["` + string(merged) + `"]`},
	}
	for _, row := range statements {
		if _, err := database.Exec(
			`INSERT INTO memory_batches
			   (session_id, id, level, content, edited_content,
			    start_message_index, end_message_index, source_batch_ids, created_at)
			 VALUES (?, ?, ?, ?, '', ?, ?, ?, '2026-08-24T00:00:00Z')`,
			id, string(row.id), row.level, row.content, row.start, row.end, row.sources); err != nil {
			t.Fatalf("塞入摘要节点失败: %v", err)
		}
	}
	if _, err := database.Exec(
		`UPDATE sessions SET raw_compaction_cursor = 2, active_batch_ids = ? WHERE id = ?`,
		`["`+string(active)+`"]`, id); err != nil {
		t.Fatalf("设置活跃前沿失败: %v", err)
	}
	return active, merged
}

// 修订活跃摘要：立刻返回整棵树，原文与修订版都在。
func TestEditActiveMemoryReturnsTheUpdatedTree(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)
	active, _ := seedMemory(t, ts, id)

	status, body := ts.do(t, http.MethodPatch,
		"/api/v1/sessions/"+id+"/memory/"+string(active), `{"content":"Topic: 我改过的"}`)

	if status != http.StatusOK {
		t.Fatalf("修订返回 %d: %s", status, body)
	}
	var tree memoryTree
	if err := json.Unmarshal(body, &tree); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	var node memoryNodeView
	for _, candidate := range tree.Batches {
		if candidate.ID == string(active) {
			node = candidate
		}
	}
	if node.Content != "Topic: 我改过的" {
		t.Errorf("生效正文 = %q", node.Content)
	}
	if !node.Edited {
		t.Error("没有标记为已修订")
	}
	// 两份都要给，界面才能让人对照"我改了什么"。
	if node.OriginalContent != "Topic: 活跃节点" {
		t.Errorf("原文 = %q; want 模型当初写的那一版", node.OriginalContent)
	}
	// 标题取生效正文的首行，因此也要跟着变。
	if node.Title != "Topic: 我改过的" {
		t.Errorf("标题 = %q", node.Title)
	}
}

// 改一个已经被合并进上层的节点：模型看到的一个字都不会变，因此明确拒绝，
// 而不是让用户以为自己改了什么。
func TestEditingANonActiveBatchIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)
	_, merged := seedMemory(t, ts, id)

	status, body := ts.do(t, http.MethodPatch,
		"/api/v1/sessions/"+id+"/memory/"+string(merged), `{"content":"偷偷改"}`)

	if status != http.StatusConflict {
		t.Fatalf("状态码 = %d: %s; want 409", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "batch_not_active" {
		t.Errorf("code = %q; want batch_not_active", failure.Error.Code)
	}
}

// 空串撤销修订，回到模型原始那一版。空**不是**参数错误——它是一个有意义的取值。
func TestEditingWithEmptyContentRevertsInsteadOfFailing(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)
	active, _ := seedMemory(t, ts, id)
	path := "/api/v1/sessions/" + id + "/memory/" + string(active)

	if status, body := ts.do(t, http.MethodPatch, path, `{"content":"改过"}`); status != http.StatusOK {
		t.Fatalf("修订返回 %d: %s", status, body)
	}

	status, body := ts.do(t, http.MethodPatch, path, `{"content":""}`)

	if status != http.StatusOK {
		t.Fatalf("撤销返回 %d: %s", status, body)
	}
	var tree memoryTree
	_ = json.Unmarshal(body, &tree)
	for _, node := range tree.Batches {
		if node.ID == string(active) {
			if node.Edited {
				t.Error("撤销之后仍标记为已修订")
			}
			if node.Content != "Topic: 活跃节点" {
				t.Errorf("撤销之后 = %q; want 模型原文", node.Content)
			}
		}
	}
}

// 修订写 session.Memory，一轮在跑时 Agent 也在读写同一个对象——必须互斥。
func TestEditingWhileATurnIsRunningReturnsConflict(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)
	active, _ := seedMemory(t, ts, id)

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第一条"}`); status != http.StatusAccepted {
		t.Fatal("提交没有被接受")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	status, body := ts.do(t, http.MethodPatch,
		"/api/v1/sessions/"+id+"/memory/"+string(active), `{"content":"改"}`)

	if status != http.StatusConflict {
		t.Fatalf("状态码 = %d: %s; want 409", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "turn_in_progress" {
		t.Errorf("code = %q; want turn_in_progress", failure.Error.Code)
	}

	close(block)
}

// batch id 是外部输入，格式不对时给一句明确的说明而不是"没找到"。
func TestEditingValidatesTheBatchID(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodPatch,
		"/api/v1/sessions/"+id+"/memory/随便写的", `{"content":"改"}`)

	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d: %s; want 400", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "invalid_batch_id" {
		t.Errorf("code = %q; want invalid_batch_id", failure.Error.Code)
	}
}

// —— M4.6：快照带上上下文占用 ——

// 快照必须带占用，否则刷新页面之后仪表盘就是空的，直到用户再发一条消息——
// 那看起来像是功能没了。事件流只从 last_sequence 之后订阅，看不到历史上的
// usage 事件，所以这个数字只能由快照带过来。
func TestSnapshotCarriesContextUsage(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("快照返回 %d: %s", status, body)
	}
	var snapshot sessionSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	// 空会话也有占用：system 指令和工具定义每次请求都要发。
	if snapshot.Usage.InputTokens <= 0 {
		t.Errorf("占用 = %d; want 正数（system 指令与工具定义本身就占额度）",
			snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.ContextWindow != 1048576 {
		t.Errorf("窗口 = %d; want %d", snapshot.Usage.ContextWindow, 1048576)
	}
	if snapshot.Usage.Source != string(domain.ContextUsageEstimated) {
		t.Errorf("来源 = %q; want estimated", snapshot.Usage.Source)
	}
	// 派生值由后端算好一并发出，前端不该自己重算。
	if snapshot.Usage.Remaining != 1048576-snapshot.Usage.InputTokens {
		t.Errorf("剩余 = %d，和窗口减占用对不上", snapshot.Usage.Remaining)
	}
}

// 快照里的占用要**现算**，因此聊得越多它越大——而不是去翻最后一条 usage 事件
// （那只能告诉你"上一次请求占了多少"）。
func TestSnapshotUsageReflectsTheCurrentHistory(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)

	before := snapshotUsageOf(t, ts, id)

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns",
		`{"content":"`+strings.Repeat("这是一条很长的消息。", 50)+`"}`); status != http.StatusAccepted {
		t.Fatal("提交没有被接受")
	}
	// 等这一轮把消息写进历史。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snapshotUsageOf(t, ts, id) > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("历史变长之后快照里的占用没有变大（一直是 %d）", before)
}

// snapshotUsageOf 取一次快照里的占用 token 数。
func snapshotUsageOf(t *testing.T, ts *testServer, id string) int {
	t.Helper()
	_, body := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id, "")
	var snapshot sessionSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	return snapshot.Usage.InputTokens
}
