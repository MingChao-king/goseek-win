package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"goseek/internal/domain"
	"goseek/internal/store"
)

// fakeModel 是一个可控的模型：它按预设回复，并可以被要求在回复前先阻塞住，
// 用来构造"一轮正在进行中"的场景。
type fakeModel struct {
	mutex     sync.Mutex
	responses []domain.ModelResponse
	requests  []domain.ModelRequest
	calls     int
	// block 非 nil 时，第一次调用会一直等到它被关闭。
	block chan struct{}
	// started 在第一次调用真正开始时关闭，让测试知道那一轮已经跑起来了。
	started chan struct{}
	once    sync.Once
}

// Summarize 返回一段固定的假摘要。测试里的对话都很短，不会触发压缩，
// 但接口要求它存在。
func (fake *fakeModel) Summarize(context.Context, string, string) (string, error) {
	return "假摘要", nil
}

// Complete 返回下一个预设响应。
func (fake *fakeModel) Complete(
	ctx context.Context,
	request domain.ModelRequest,
	onDelta domain.DeltaFunc,
) (domain.ModelResponse, error) {
	captured := domain.ModelRequest{
		Messages: append([]domain.ModelMessage(nil), request.Messages...),
		Tools:    append([]domain.ToolSpec(nil), request.Tools...),
	}
	fake.mutex.Lock()
	fake.requests = append(fake.requests, captured)
	fake.mutex.Unlock()

	fake.once.Do(func() {
		if fake.started != nil {
			close(fake.started)
		}
	})
	if fake.block != nil {
		select {
		case <-fake.block:
		case <-ctx.Done():
			return domain.ModelResponse{}, ctx.Err()
		}
	}
	if onDelta != nil {
		onDelta(domain.TextDelta{Text: "片段"})
	}

	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.calls >= len(fake.responses) {
		return domain.ModelResponse{Content: "默认回复"}, nil
	}
	response := fake.responses[fake.calls]
	fake.calls++
	return response, nil
}

// testContextWindow 是测试里假定的上下文窗口。
const testContextWindow = 100000
const testDefaultModel = "glm-5.3"

// testServer 组装一个跑在临时目录上的服务。
type testServer struct {
	server *Server
	http   *httptest.Server
	model  *fakeModel
	// directory 是这次测试的数据目录，个别用例要直接连库预置数据。
	directory string
}

// newTestServer 启动一个测试服务。
func newTestServer(t *testing.T, fake *fakeModel) *testServer {
	t.Helper()
	// 空目录，什么都不预先创建：NewServer 必须自己把库建起来。
	// 之前这里手工 seed 过一次，正好掩盖了"服务首次启动打不开数据库"的 bug。
	directory := t.TempDir()

	if fake == nil {
		fake = &fakeModel{}
	}
	server, err := NewServer(directory, nil, func(string) ModelClient { return fake }, testDefaultModel, testContextWindow, discardLogger())
	if err != nil {
		t.Fatalf("NewServer 返回错误: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())

	t.Cleanup(func() {
		httpServer.Close()
		server.Close()
	})
	return &testServer{server: server, http: httpServer, model: fake, directory: directory}
}

// do 发一个请求并返回状态码与响应体。
func (ts *testServer) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, ts.http.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	response, err := ts.http.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer response.Body.Close()

	payload := make([]byte, 0, 4096)
	buffer := make([]byte, 4096)
	for {
		count, err := response.Body.Read(buffer)
		payload = append(payload, buffer[:count]...)
		if err != nil {
			break
		}
	}
	return response.StatusCode, payload
}

// createSession 新建一个会话并返回它的 id。
func (ts *testServer) createSession(t *testing.T) string {
	t.Helper()
	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions", `{"workspace":"/tmp"}`)
	if status != http.StatusCreated {
		t.Fatalf("新建会话返回 %d: %s", status, body)
	}
	var created sessionSummary
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	return created.ID
}

func TestCreateAndListSessions(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodGet, "/api/v1/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("列表返回 %d: %s", status, body)
	}
	var list listSessionsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != id {
		t.Fatalf("列表 = %+v", list.Sessions)
	}
	if list.Sessions[0].Workspace != "/private/tmp" {
		t.Errorf("workspace = %q", list.Sessions[0].Workspace)
	}
}

func TestModelsEndpoint(t *testing.T) {
	ts := newTestServer(t, nil)
	status, body := ts.do(t, http.MethodGet, "/api/v1/models", "")
	if status != http.StatusOK {
		t.Fatalf("模型列表返回 %d: %s", status, body)
	}
	var response struct{ Models []modelInfoView }
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(response.Models) != 4 {
		t.Fatalf("模型数 = %d; want 4", len(response.Models))
	}
	first := response.Models[0]
	if first.EffectiveContextWindow != 996147 || first.CompactionTrigger != 796917 {
		t.Errorf("窗口信息 = %+v", first)
	}
}

func TestSessionModelRoundTrip(t *testing.T) {
	ts := newTestServer(t, nil)
	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions",
		`{"workspace":"/tmp","model":"deepseek-v4-flash"}`)
	if status != http.StatusCreated {
		t.Fatalf("创建返回 %d: %s", status, body)
	}
	var created sessionSummary
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if created.Model != "deepseek-v4-flash" {
		t.Fatalf("创建后的模型 = %q", created.Model)
	}

	status, body = ts.do(t, http.MethodPatch, "/api/v1/sessions/"+created.ID,
		`{"model":"glm-5.3"}`)
	if status != http.StatusOK {
		t.Fatalf("切换返回 %d: %s", status, body)
	}
	status, body = ts.do(t, http.MethodGet, "/api/v1/sessions/"+created.ID, "")
	if status != http.StatusOK {
		t.Fatalf("快照返回 %d: %s", status, body)
	}
	var snapshot sessionSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if snapshot.Model != "glm-5.3" {
		t.Fatalf("切换后的模型 = %q", snapshot.Model)
	}
	if snapshot.ModelInfo.EffectiveContextWindow != 996147 {
		t.Fatalf("有效窗口 = %d", snapshot.ModelInfo.EffectiveContextWindow)
	}
}

func TestUnknownModelIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions", `{"model":"bad-model"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知模型创建返回 %d: %s", status, body)
	}
}

func TestWorkspaceValidation(t *testing.T) {
	ts := newTestServer(t, nil)
	cases := []struct {
		name string
		body string
		code string
	}{
		{"相对路径", `{"workspace":"relative/path"}`, "invalid_workspace"},
		{"不存在", `{"workspace":"/definitely/not/here"}`, "workspace_not_found"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := ts.do(t, http.MethodPost, "/api/v1/sessions", testCase.body)
			if status != http.StatusBadRequest {
				t.Fatalf("状态码 = %d: %s", status, body)
			}
			var failure errorResponse
			if err := json.Unmarshal(body, &failure); err != nil {
				t.Fatalf("响应不是合法 JSON: %v", err)
			}
			if failure.Error.Code != testCase.code {
				t.Errorf("code = %q; want %q", failure.Error.Code, testCase.code)
			}
		})
	}
}

func TestDefaultWorkspaceEndpoint(t *testing.T) {
	ts := newTestServer(t, nil)
	status, body := ts.do(t, http.MethodGet, "/api/v1/workspace/default", "")
	if status != http.StatusOK {
		t.Fatalf("默认工作区返回 %d: %s", status, body)
	}
	var response struct{ Path string }
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if response.Path == "" {
		t.Fatal("默认工作区不能为空")
	}
}

// 快照必须带 last_sequence：它是前端衔接实时事件流的锚点。
func TestSnapshotCarriesLastSequence(t *testing.T) {
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
	if snapshot.ID != id {
		t.Errorf("id = %q", snapshot.ID)
	}
	if snapshot.LastSequence != 0 {
		t.Errorf("新会话的 last_sequence = %d; want 0", snapshot.LastSequence)
	}
	if len(snapshot.Messages) != 0 {
		t.Errorf("新会话有 %d 条消息", len(snapshot.Messages))
	}
}

func TestSidebarContextReachesModelWithoutPollutingConversation(t *testing.T) {
	fake := &fakeModel{responses: []domain.ModelResponse{{Content: "已经查看。"}}}
	ts := newTestServer(t, fake)
	id := ts.createSession(t)
	const original = "分析我当前打开的内容"
	body := `{
		"content":"` + original + `",
		"sidebar_items":[
			{"kind":"page","title":"Example","url":"https://example.com"},
			{"kind":"file","title":"main.go","path":"backend/cmd/goseek/main.go"}
		]
	}`

	if status, response := ts.do(
		t,
		http.MethodPost,
		"/api/v1/sessions/"+id+"/turns",
		body,
	); status != http.StatusAccepted {
		t.Fatalf("提交返回 %d: %s", status, response)
	}
	waitForIdle(t, ts, id)

	snapshot := ts.snapshot(t, id)
	if !hasUserMessage(snapshot, original) {
		t.Fatalf("快照没有用户原文: %+v", snapshot.Messages)
	}
	for _, message := range snapshot.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "example.com") {
			t.Fatalf("侧栏上下文污染了快照用户消息: %q", message.Content)
		}
	}
	if got := firstSummary(t, ts).Title; got != original {
		t.Fatalf("会话标题被侧栏上下文污染: %q", got)
	}

	fake.mutex.Lock()
	requests := append([]domain.ModelRequest(nil), fake.requests...)
	fake.mutex.Unlock()
	if len(requests) == 0 {
		t.Fatal("模型没有收到请求")
	}
	modelContent := requests[0].Messages[len(requests[0].Messages)-1].Content
	if !strings.Contains(modelContent, "https://example.com") ||
		!strings.Contains(modelContent, "backend/cmd/goseek/main.go") ||
		!strings.Contains(modelContent, original) {
		t.Fatalf("模型视图没有完整侧栏上下文和原文: %q", modelContent)
	}

	stream, closeStream := ts.openStream(
		t,
		"/api/v1/sessions/"+id+"/events?from=0",
		nil,
	)
	defer closeStream()
	for _, frame := range readSSE(t, stream, int(snapshot.LastSequence)) {
		event := frame.event(t)
		if event.Type != string(domain.EventUserMessage) {
			continue
		}
		var payload domain.UserMessagePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("user.message payload 无法解析: %v", err)
		}
		if payload.Content != original {
			t.Fatalf("user.message 事件被侧栏上下文污染: %q", payload.Content)
		}
	}
}

func TestFormatSidebarContextIsBoundedAndDeduplicated(t *testing.T) {
	items := []sidebarContextItem{
		{Kind: "page", Title: "Example", URL: "https://example.com"},
		{Kind: "page", Title: "重复", URL: "https://example.com"},
		{Kind: "file", Path: "front/src/App.tsx"},
		{Kind: "unknown", URL: "https://ignored.example"},
	}
	got := formatSidebarContext(items, []string{"https://example.com", "https://legacy.example"})

	if strings.Count(got, "https://example.com") != 1 {
		t.Fatalf("重复页面没有去重: %q", got)
	}
	if !strings.Contains(got, "front/src/App.tsx") ||
		!strings.Contains(got, "https://legacy.example") {
		t.Fatalf("页面或文件缺失: %q", got)
	}
	if strings.Contains(got, "ignored.example") {
		t.Fatalf("未知侧栏类型不应进入上下文: %q", got)
	}
}

// 压缩现状必须进快照。压缩事件是历史事件，前端只从 last_sequence 之后订阅，
// 刷新页面后不会重放到它们——不放进快照，界面就不知道这个会话已经折叠过历史。
func TestSnapshotCarriesMemory(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("快照返回 %d: %s", status, body)
	}
	// 没压缩过的会话：游标是"第 1 条起都是原文"，前沿是空数组而不是 null——
	// null 会逼前端在每个用到它的地方先判一次空。
	if !bytes.Contains(body, []byte(`"active_batches":[]`)) {
		t.Errorf("空前沿没有序列化成 []: %s", body)
	}
	var snapshot sessionSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if snapshot.Memory.RawCompactionCursor != 1 {
		t.Errorf("游标 = %d; want 1（一条都没折叠）", snapshot.Memory.RawCompactionCursor)
	}
	if snapshot.Memory.TotalBatches != 0 {
		t.Errorf("新会话有 %d 个摘要节点", snapshot.Memory.TotalBatches)
	}
}

// 摘要节点在快照里的形状，必须与它在压缩事件里的形状一致——
// 前端用同一段代码渲染两者，形状不同只会逼出两份渲染逻辑。
func TestMemoryViewMatchesTheEventShape(t *testing.T) {
	batch := domain.MemoryBatch{
		ID: "mem_0000000000000000000000000000000a", Level: 2,
		Content: "Topic: 排查登录失败\n细节若干", StartMessageIndex: 0, EndMessageIndex: 12,
	}
	memory := domain.ConversationMemory{
		RawCompactionCursor: 12,
		Batches:             []domain.MemoryBatch{batch},
		ActiveBatchIDs:      []domain.MemoryBatchID{batch.ID},
	}

	view := newMemoryView(memory)

	if len(view.ActiveBatches) != 1 {
		t.Fatalf("前沿有 %d 项; want 1", len(view.ActiveBatches))
	}
	inSnapshot, err := json.Marshal(view.ActiveBatches[0])
	if err != nil {
		t.Fatalf("序列化快照形态失败: %v", err)
	}
	inEvent, err := json.Marshal(domain.NewMemoryBatchSummary(batch))
	if err != nil {
		t.Fatalf("序列化事件形态失败: %v", err)
	}
	if string(inSnapshot) != string(inEvent) {
		t.Errorf("两处形状不一致:\n快照 %s\n事件 %s", inSnapshot, inEvent)
	}
	// 序号是给人看的：闭区间、从 1 开始。
	if view.ActiveBatches[0].StartMessage != 1 || view.ActiveBatches[0].EndMessage != 12 {
		t.Errorf("覆盖范围 = [%d,%d]; want [1,12]",
			view.ActiveBatches[0].StartMessage, view.ActiveBatches[0].EndMessage)
	}
}

func TestErrorsAreMappedToStatusCodes(t *testing.T) {
	ts := newTestServer(t, nil)
	valid := ts.createSession(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"会话 id 格式非法", http.MethodGet, "/api/v1/sessions/not-an-id", "", http.StatusBadRequest, "invalid_session_id"},
		{"提交时会话 id 格式非法", http.MethodPost, "/api/v1/sessions/not-an-id/turns", `{"content":"x"}`, http.StatusBadRequest, "invalid_session_id"},
		{"会话不存在", http.MethodGet, "/api/v1/sessions/ses_0123456789abcdef0123456789abcdef", "", http.StatusNotFound, "session_not_found"},
		{"请求体不是 JSON", http.MethodPost, "/api/v1/sessions/" + valid + "/turns", "不是 JSON", http.StatusBadRequest, "invalid_body"},
		{"消息为空", http.MethodPost, "/api/v1/sessions/" + valid + "/turns", `{"content":""}`, http.StatusBadRequest, "empty_content"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := ts.do(t, testCase.method, testCase.path, testCase.body)
			if status != testCase.status {
				t.Fatalf("状态码 = %d; want %d（响应 %s）", status, testCase.status, body)
			}
			var failure errorResponse
			if err := json.Unmarshal(body, &failure); err != nil {
				t.Fatalf("错误响应不是合法 JSON: %v", err)
			}
			// 前端按 code 分支，因此 code 必须稳定；message 是给人看的。
			if failure.Error.Code != testCase.code {
				t.Errorf("code = %q; want %q", failure.Error.Code, testCase.code)
			}
			if failure.Error.Message == "" {
				t.Error("错误响应没有给出说明")
			}
		})
	}
}

// 提交返回 202 而不是 200：这一轮才刚开始，结果要通过事件流看。
func TestSubmitReturnsAcceptedImmediately(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"你好"}`)
	if status != http.StatusAccepted {
		t.Fatalf("提交返回 %d: %s", status, body)
	}
}

// M5.2 起，一轮在跑时再提交**不再被拒**——消息进队列，在下一次请求模型之前被
// 注入到正在进行的那一轮里。这让用户能边跑边说：发现模型跑偏了补一句就能纠正。
//
// `running` 标记的含义随之收窄：从"拒绝一切"变成"拒绝并发写记忆"。
func TestSubmitWhileRunningIsQueuedInsteadOfRejected(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)

	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第一条"}`); status != http.StatusAccepted {
		t.Fatalf("第一次提交返回 %d: %s", status, body)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第二条"}`)

	if status != http.StatusAccepted {
		t.Fatalf("运行中提交返回 %d: %s; want 202（进队列）", status, body)
	}

	close(block)
}

// 压缩和摘要修订仍然被拒——它们并发改写 session.Memory，和注入不是一类事。
func TestCompactWhileRunningStillReturnsConflict(t *testing.T) {
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

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/compact", "")
	if status != http.StatusConflict {
		t.Fatalf("运行中压缩返回 %d: %s; want 409", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "turn_in_progress" {
		t.Errorf("code = %q", failure.Error.Code)
	}

	close(block)
}

// 会话被别的进程（比如终端里的 goseek）占着时，返回 409 而不是 500。
func TestBusySessionReturnsConflict(t *testing.T) {
	ts := newTestServer(t, nil)
	id := ts.createSession(t)

	// 模拟另一个进程持有这个会话。
	other, err := store.New(ts.server.dataDirectory)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer other.Close()
	if _, err := other.Load(domain.SessionID(id)); err != nil {
		t.Fatalf("占用会话失败: %v", err)
	}

	status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"你好"}`)
	if status != http.StatusConflict {
		t.Fatalf("状态码 = %d: %s; want 409", status, body)
	}
	var failure errorResponse
	_ = json.Unmarshal(body, &failure)
	if failure.Error.Code != "session_busy" {
		t.Errorf("code = %q", failure.Error.Code)
	}
}

// 读路径不经过 Runner：一轮正在跑的时候，列表和快照仍要立刻返回。
func TestReadsAreNotBlockedByARunningTurn(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	ts := newTestServer(t, &fakeModel{
		responses: []domain.ModelResponse{{Content: "好的"}},
		block:     block,
		started:   started,
	})
	id := ts.createSession(t)

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"你好"}`); status != http.StatusAccepted {
		t.Fatal("提交失败")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("第一轮没有开始")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if status, _ := ts.do(t, http.MethodGet, "/api/v1/sessions", ""); status != http.StatusOK {
			t.Errorf("一轮进行中时列表请求失败")
		}
		if status, _ := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id, ""); status != http.StatusOK {
			t.Errorf("一轮进行中时快照请求失败")
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("读请求被正在进行的一轮挡住了")
	}
	close(block)
}

// discardLogger 返回一个丢弃全部输出的 logger，避免测试刷屏。
func discardLogger() *slogLogger {
	return newDiscardLogger()
}

// readSSE 从一条 SSE 响应里读出若干帧，直到读满 count 个或超时。
func readSSE(t *testing.T, body *bufio.Reader, count int) []sseFrame {
	t.Helper()
	var frames []sseFrame
	var current sseFrame
	for len(frames) < count {
		line, err := body.ReadString('\n')
		if err != nil {
			return frames
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if current.data != "" {
				frames = append(frames, current)
			}
			current = sseFrame{}
		case strings.HasPrefix(line, "id: "):
			current.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			current.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return frames
}

// sseFrame 是解析出来的一帧。
type sseFrame struct {
	id   string
	data string
}

// event 解出这一帧承载的事件。
func (frame sseFrame) event(t *testing.T) eventView {
	t.Helper()
	var view eventView
	if err := json.Unmarshal([]byte(frame.data), &view); err != nil {
		t.Fatalf("帧不是合法 JSON: %v", err)
	}
	return view
}

// URL 里的路径穿越在到达处理器之前就被 Go 的路由规范化掉了（../.. 会被折叠），
// 因此它根本匹配不上任何路由。这条测试钉住这个事实——不能因为"反正 SessionID
// 会校验"就假设穿越请求一定走到校验那一步。
func TestPathTraversalNeverReachesAHandler(t *testing.T) {
	ts := newTestServer(t, nil)
	ts.createSession(t)

	status, _ := ts.do(t, http.MethodGet, "/api/v1/sessions/../../../etc/passwd", "")
	if status == http.StatusOK {
		t.Fatal("路径穿越请求返回了 200")
	}
}
