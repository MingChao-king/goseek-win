package httpapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"goseek/internal/domain"
)

// 归档把会话从默认列表里拿走，显式索取才出现；数据一条不动。
func TestArchiveHidesSessionFromDefaultList(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	if status, body := ts.do(t, http.MethodPatch, "/api/v1/sessions/"+id, `{"archived":true}`); status != http.StatusOK {
		t.Fatalf("归档返回 %d: %s", status, body)
	}

	if got := listedIDs(t, ts, ""); len(got) != 0 {
		t.Errorf("归档之后默认列表里还有 %v", got)
	}
	got := listedIDs(t, ts, "?include_archived=1")
	if len(got) != 1 || got[0] != id {
		t.Errorf("显式索取归档的看到 %v; want [%s]", got, id)
	}
}

// 取消归档要能做到——所以请求体里的 archived 必须区分"传了 false"和"没传"。
func TestUnarchiveWorks(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)
	if status, _ := ts.do(t, http.MethodPatch, "/api/v1/sessions/"+id, `{"archived":true}`); status != http.StatusOK {
		t.Fatal("归档失败")
	}

	if status, body := ts.do(t, http.MethodPatch, "/api/v1/sessions/"+id, `{"archived":false}`); status != http.StatusOK {
		t.Fatalf("取消归档返回 %d: %s", status, body)
	}

	if got := listedIDs(t, ts, ""); len(got) != 1 {
		t.Errorf("取消归档之后默认列表 = %v", got)
	}
}

// 重命名；空串恢复派生标题——所以 title 也必须区分"传了空串"和"没传"。
func TestRenameAndRestoreDerivedTitle(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	if status, body := ts.do(t, http.MethodPatch, "/api/v1/sessions/"+id, `{"title":"我起的名字"}`); status != http.StatusOK {
		t.Fatalf("重命名返回 %d: %s", status, body)
	}
	renamed := firstSummary(t, ts)
	if renamed.Title != "我起的名字" || !renamed.CustomTitle {
		t.Errorf("重命名之后 = %+v", renamed)
	}

	if status, _ := ts.do(t, http.MethodPatch, "/api/v1/sessions/"+id, `{"title":""}`); status != http.StatusOK {
		t.Fatal("恢复派生标题失败")
	}
	restored := firstSummary(t, ts)
	if restored.CustomTitle {
		t.Errorf("恢复之后仍然标记为自定义: %+v", restored)
	}
}

// 什么都不传是参数错误——静默成功会让人以为改了什么。
func TestEmptyUpdateIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodPatch, "/api/v1/sessions/"+id, `{}`)

	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d: %s; want 400", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "empty_update" {
		t.Errorf("code = %q", failure.Error.Code)
	}
}

// 删除要把会话从列表里彻底拿走，再查快照是 404。
func TestDeleteRemovesTheSession(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	if status, body := ts.do(t, http.MethodDelete, "/api/v1/sessions/"+id, ""); status != http.StatusOK {
		t.Fatalf("删除返回 %d: %s", status, body)
	}

	if got := listedIDs(t, ts, "?include_archived=1"); len(got) != 0 {
		t.Errorf("删除之后还列出 %v", got)
	}
	if status, _ := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id, ""); status != http.StatusNotFound {
		t.Errorf("删除之后快照返回 %d; want 404", status)
	}
}

// 删除一个正被打开（Runner 持有、锁着）的会话：必须先停掉写者，不能留下
// 一个对着不存在的会话继续写入的 goroutine。
func TestDeleteStopsTheRunnerFirst(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)
	// 提交一轮把 Runner 创建出来并跑完，这样会话确实被打开着（锁被持有）。
	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"你好"}`); status != http.StatusAccepted {
		t.Fatal("提交没有被接受")
	}
	waitForIdle(t, ts, id)

	status, body := ts.do(t, http.MethodDelete, "/api/v1/sessions/"+id, "")

	if status != http.StatusOK {
		t.Fatalf("删除返回 %d: %s", status, body)
	}
	// Runner 被摘掉之后，再提交应当当作会话不存在。
	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"再来"}`); status != http.StatusNotFound {
		t.Errorf("删除之后提交返回 %d; want 404", status)
	}
}

// 中止当前这一轮：Runner 继续活着，会话还能接着用。
func TestCancelTurnStopsOnlyThatTurn(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}, {Content: "第二轮"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)
	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"跑起来"}`); status != http.StatusAccepted {
		t.Fatal("提交没有被接受")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns/cancel", ""); status != http.StatusOK {
		t.Fatalf("中止返回 %d: %s", status, body)
	}

	// 被中止的那一轮会因为 ctx 取消而失败，Runner 随后回到空闲。
	waitForIdle(t, ts, id)
	close(block)

	// 会话仍然可用：还能再提交。
	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"接着来"}`); status != http.StatusAccepted {
		t.Errorf("中止之后再提交返回 %d: %s; want 202", status, body)
	}
}

// 没有正在跑的一轮时中止要说清楚，而不是假装成功。
func TestCancelWithNothingRunningReportsIt(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns/cancel", "")

	if status != http.StatusConflict {
		t.Fatalf("状态码 = %d: %s; want 409", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "no_turn_running" {
		t.Errorf("code = %q; want no_turn_running", failure.Error.Code)
	}
}

// 三个新端点都要先校验会话 ID——它会参与文件路径定位。
func TestLifecycleEndpointsValidateSessionID(t *testing.T) {
	ts := newTestServer(t, nil)
	cases := []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/sessions/not-an-id", `{"archived":true}`},
		{http.MethodDelete, "/api/v1/sessions/not-an-id", ""},
		{http.MethodPost, "/api/v1/sessions/not-an-id/turns/cancel", ""},
	}
	for _, testCase := range cases {
		status, _ := ts.do(t, testCase.method, testCase.path, testCase.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s %s 返回 %d; want 400", testCase.method, testCase.path, status)
		}
	}
}

// listedIDs 取会话列表里的 id。
func listedIDs(t *testing.T, ts *testServer, query string) []string {
	t.Helper()
	_, body := ts.do(t, http.MethodGet, "/api/v1/sessions"+query, "")
	var list listSessionsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
	ids := make([]string, 0, len(list.Sessions))
	for _, item := range list.Sessions {
		ids = append(ids, item.ID)
	}
	return ids
}

// firstSummary 取列表里的第一项。
func firstSummary(t *testing.T, ts *testServer) sessionSummary {
	t.Helper()
	_, body := ts.do(t, http.MethodGet, "/api/v1/sessions", "")
	var list listSessionsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
	if len(list.Sessions) == 0 {
		t.Fatal("列表是空的")
	}
	return list.Sessions[0]
}

// waitForIdle 等到这个会话不再有正在跑的一轮。
//
// 判据是"提交压缩不再返回 409"——压缩和轮次共用 running 标记，所以它能问出
// 空闲状态，而且不会真的改动什么（没有摘要器时压缩是空操作）。
func waitForIdle(t *testing.T, ts *testServer, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/compact", ""); status != http.StatusConflict {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等不到会话空闲")
}

// 一轮在注入生效之前就结束时，队列里的消息不能被丢掉——它是用户明确说过的话。
//
// 这个缝隙是真实存在的：用户在模型吐最后一个字的同时按下回车，消息进了队列，
// 可下一次"请求模型之前"永远不会到来，因为这一轮已经收口了。此时 Runner 必须
// 把队列排空、自己开一轮新的。
func TestQueuedMessageStartsANewTurnWhenTheRunningTurnEndsFirst(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "第一轮的回复"}, {Content: "第二轮的回复"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第一条"}`); status != http.StatusAccepted {
		t.Fatal("第一次提交没有被接受")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	// 这一轮卡在模型调用上，注入点（下一次请求模型之前）永远不会到来：
	// fakeModel 一被放行就直接给出最终回复，本轮随即收口。
	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第二条"}`); status != http.StatusAccepted {
		t.Fatalf("运行中提交返回 %d: %s", status, body)
	}
	close(block)

	// 等到两条用户消息都在历史里。轮询而不是固定 sleep：新一轮是异步开起来的，
	// 睡多久都是猜，而轮询在快的机器上立刻返回、在慢的机器上也不会假失败。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := ts.snapshot(t, id)
		if countUserMessages(snapshot) >= 2 {
			// 第二条确实成了会话历史的一部分，而不是停在某个内存队列里。
			if !hasUserMessage(snapshot, "第二条") {
				t.Fatalf("历史里没有第二条: %+v", snapshot.Messages)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("排队的消息没有开启新的一轮")
}

// 中止之后，历史里不能留下没有观察配对的工具调用。
//
// 悬空调用是硬伤而不是瑕疵：OpenAI 兼容接口要求每个 tool_call 后面必须跟一条同
// id 的 tool 消息，缺一条，**之后这个会话的每一次请求都会被供应商 400 拒绝**——
// 也就是说会话从此报废。
func TestCancelLeavesNoDanglingToolCall(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"跑起来"}`); status != http.StatusAccepted {
		t.Fatal("提交没有被接受")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("这一轮没有开始")
	}

	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns/cancel", ""); status != http.StatusOK {
		t.Fatalf("中止返回 %d: %s", status, body)
	}
	waitForIdle(t, ts, id)
	close(block)

	snapshot := ts.snapshot(t, id)

	// 逐条检查：每个 assistant 消息带的工具调用，都要能在后面找到同 id 的观察。
	observed := make(map[string]bool)
	for _, message := range snapshot.Messages {
		if message.ToolCallID != "" {
			observed[message.ToolCallID] = true
		}
	}
	for _, message := range snapshot.Messages {
		for _, call := range message.ToolCalls {
			if !observed[call.ID] {
				t.Errorf("工具调用 %q 没有对应的观察，这个会话之后的每次请求都会被供应商拒绝", call.ID)
			}
		}
	}

	// 而且这一轮的失败要落库：只中止不记录的话，刷新页面之后那一轮看起来像是
	// 凭空断在半截，用户无从判断是自己停的还是系统崩了。
	//
	// 从序号 0 重新订阅一次事件流，读到的就是**库里**的那份——比在内存里问 Runner
	// 更接近用户刷新页面时看到的东西。
	body, closeStream := ts.openStream(t, "/api/v1/sessions/"+id+"/events?from=0", nil)
	defer closeStream()
	types := replayedTypes(t, body, int(snapshot.LastSequence))
	if !contains(types, "turn.failed") {
		t.Errorf("没有 turn.failed 事件，事件序列是 %v", types)
	}
}

// replayedTypes 从事件流里读 count 帧，返回它们的类型序列。
//
// count 取自快照的 last_sequence——那正是这个会话已落库的 durable 事件条数，
// 因此既不会少读，也不会读到一半阻塞去等一个永远不来的帧（重放完之后流会一直开着）。
func replayedTypes(t *testing.T, body *bufio.Reader, count int) []string {
	t.Helper()
	frames := readSSE(t, body, count)
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frame.event(t).Type)
	}
	return types
}

// contains 判断字符串切片里有没有某个值。
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// countUserMessages 数快照里的用户消息条数。
func countUserMessages(snapshot sessionSnapshot) int {
	count := 0
	for _, message := range snapshot.Messages {
		if message.Role == "user" {
			count++
		}
	}
	return count
}

// hasUserMessage 判断快照里有没有某条用户消息。
func hasUserMessage(snapshot sessionSnapshot, content string) bool {
	for _, message := range snapshot.Messages {
		if message.Role == "user" && message.Content == content {
			return true
		}
	}
	return false
}
