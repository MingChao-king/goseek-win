package httpapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// openStream 打开一条 SSE 连接，返回带缓冲的读取器。
func (ts *testServer) openStream(t *testing.T, path string, header http.Header) (*bufio.Reader, func()) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, ts.http.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for key, values := range header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	response, err := ts.http.Client().Do(request)
	if err != nil {
		t.Fatalf("打开事件流失败: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("事件流返回 %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		response.Body.Close()
		t.Fatalf("Content-Type = %q; want text/event-stream", contentType)
	}
	return bufio.NewReader(response.Body), func() { response.Body.Close() }
}

// runOneTurn 提交一条消息并等它跑完。
func (ts *testServer) runOneTurn(t *testing.T, id, content string) {
	t.Helper()
	if status, body := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns",
		`{"content":"`+content+`"}`); status != http.StatusAccepted {
		t.Fatalf("提交返回 %d: %s", status, body)
	}
	// 轮询快照，等 last_sequence 不再变化，说明这一轮收口了。
	deadline := time.Now().Add(5 * time.Second)
	previous := int64(-1)
	for time.Now().Before(deadline) {
		snapshot := ts.snapshot(t, id)
		if snapshot.LastSequence > 0 && snapshot.LastSequence == previous {
			return
		}
		previous = snapshot.LastSequence
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待一轮结束超时")
}

// snapshot 取一次会话快照。
func (ts *testServer) snapshot(t *testing.T, id string) sessionSnapshot {
	t.Helper()
	status, body := ts.do(t, http.MethodGet, "/api/v1/sessions/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("快照返回 %d: %s", status, body)
	}
	var snapshot sessionSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("快照不是合法 JSON: %v", err)
	}
	return snapshot
}

// 从 0 开始订阅时，一个已经跑完的会话的全部 durable 事件都要被重放出来。
func TestStreamReplaysHistoryFromZero(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)
	ts.runOneTurn(t, id, "你好")

	body, closeStream := ts.openStream(t, "/api/v1/sessions/"+id+"/events?from=0", nil)
	defer closeStream()

	frames := readSSE(t, body, 5)
	if len(frames) < 5 {
		t.Fatalf("只读到 %d 帧", len(frames))
	}

	var types []string
	for _, frame := range frames {
		types = append(types, frame.event(t).Type)
	}
	want := []string{"turn.started", "user.message", "state.changed", "context.usage.updated", "assistant.message"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("重放出的事件 = %v; want %v", types, want)
	}
}

// 只有 durable 事件带 id：它是浏览器重连时的续传位置。
// transient 事件没有序号，给它们写 id 会把客户端的位置带偏。
func TestOnlyDurableFramesCarryAnID(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)

	body, closeStream := ts.openStream(t, "/api/v1/sessions/"+id+"/events?from=0", nil)
	defer closeStream()

	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"你好"}`); status != http.StatusAccepted {
		t.Fatal("提交失败")
	}

	frames := readSSE(t, body, 6)
	sawTransient := false
	for _, frame := range frames {
		event := frame.event(t)
		if event.Sequence == 0 {
			sawTransient = true
			if frame.id != "" {
				t.Errorf("transient 事件 %q 带了 id %q", event.Type, frame.id)
			}
			continue
		}
		if frame.id == "" {
			t.Errorf("durable 事件 %q 没有 id", event.Type)
		}
	}
	if !sawTransient {
		t.Error("没有收到任何 transient 事件（delta），流式没有推出来")
	}
}

// 断线重连：带上 from 之后只补断开期间的事件，前面看过的不再重复。
func TestStreamResumesFromASequence(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)
	ts.runOneTurn(t, id, "你好")
	last := ts.snapshot(t, id).LastSequence

	// 从倒数第二个序号开始，应当只拿到最后一个 durable 事件。
	body, closeStream := ts.openStream(t,
		"/api/v1/sessions/"+id+"/events?from="+itoa(last-1), nil)
	defer closeStream()

	frames := readSSE(t, body, 1)
	if len(frames) != 1 {
		t.Fatalf("读到 %d 帧; want 1", len(frames))
	}
	if got := frames[0].event(t).Sequence; got != last {
		t.Errorf("补上的事件序号 = %d; want %d", got, last)
	}
}

// 浏览器自动重连时带的是 Last-Event-ID 头，它比查询参数更准——那是浏览器实际
// 收到的最后一条。
func TestLastEventIDHeaderTakesPrecedenceOverQuery(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "好的"}}})
	id := ts.createSession(t)
	ts.runOneTurn(t, id, "你好")
	last := ts.snapshot(t, id).LastSequence

	header := http.Header{}
	header.Set("Last-Event-ID", itoa(last-1))
	// 查询参数说"从头开始"，但头部说"我已经看到倒数第二个了"，应当听头部的。
	body, closeStream := ts.openStream(t, "/api/v1/sessions/"+id+"/events?from=0", header)
	defer closeStream()

	frames := readSSE(t, body, 1)
	if len(frames) != 1 {
		t.Fatalf("读到 %d 帧", len(frames))
	}
	if got := frames[0].event(t).Sequence; got != last {
		t.Errorf("补上的事件序号 = %d; want %d（应当听 Last-Event-ID）", got, last)
	}
}

// 这是本节最容易写错的地方：**先订阅、后重放**。
//
// 反过来做的话，"读完历史"和"开始订阅"之间产生的事件会全部丢失——而那正是断线
// 重连要解决的问题。这条测试在连接已经建立、重放已经完成之后再触发一轮，验证
// 新事件确实接得上，且序号连续没有缺口。
func TestReplayAndLiveHandoffHasNoGap(t *testing.T) {
	ts := newTestServer(t, &fakeModel{responses: []domain.ModelResponse{{Content: "第一轮"}, {Content: "第二轮"}}})
	id := ts.createSession(t)
	ts.runOneTurn(t, id, "第一句")
	afterFirst := ts.snapshot(t, id).LastSequence

	body, closeStream := ts.openStream(t, "/api/v1/sessions/"+id+"/events?from=0", nil)
	defer closeStream()

	// 先把历史读完。
	history := readSSE(t, body, int(afterFirst))
	if len(history) != int(afterFirst) {
		t.Fatalf("重放到 %d 帧; want %d", len(history), afterFirst)
	}

	// 连接已经建立，现在触发第二轮。
	if status, _ := ts.do(t, http.MethodPost, "/api/v1/sessions/"+id+"/turns", `{"content":"第二句"}`); status != http.StatusAccepted {
		t.Fatal("提交失败")
	}

	// 收集接下来的 durable 事件，验证序号从 afterFirst+1 开始且连续。
	expected := afterFirst
	deadline := time.Now().Add(5 * time.Second)
	for expected < afterFirst+4 && time.Now().Before(deadline) {
		frames := readSSE(t, body, 1)
		if len(frames) == 0 {
			break
		}
		event := frames[0].event(t)
		if event.Sequence == 0 {
			continue // transient，不参与序号检查
		}
		expected++
		if event.Sequence != expected {
			t.Fatalf("序号出现缺口：收到 %d，期望 %d", event.Sequence, expected)
		}
	}
	if expected <= afterFirst {
		t.Fatal("连接建立之后产生的事件一条都没收到——先订阅后重放的顺序可能反了")
	}
}

// 会话 id 非法时不该建立流，而是返回一个普通的 JSON 错误。
func TestStreamRejectsInvalidSessionID(t *testing.T) {
	ts := newTestServer(t, nil)

	status, body := ts.do(t, http.MethodGet, "/api/v1/sessions/not-an-id/events", "")
	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d: %s; want 400", status, body)
	}
}

// itoa 是测试里拼接查询参数用的。
func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
