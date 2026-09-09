package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goseek/internal/domain"
	"goseek/internal/model"
)

// newTestClient 启动一个假供应商，返回指向它的 Client。
func newTestClient(t *testing.T, handler http.HandlerFunc) *model.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return model.New(model.Config{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test-model",
	})
}

// streamOf 把若干个 SSE 数据行拼成一条完整的流，末尾带结束标记。
func streamOf(frames ...string) string {
	var builder strings.Builder
	for _, frame := range frames {
		fmt.Fprintf(&builder, "data: %s\n\n", frame)
	}
	builder.WriteString("data: [DONE]\n\n")
	return builder.String()
}

// contentFrame 造一个只含正文增量的分片。
func contentFrame(text string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q,"reasoning_content":null}}]}`, text)
}

// reasoningFrame 造一个只含思考增量的分片。
func reasoningFrame(text string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":null,"reasoning_content":%q}}]}`, text)
}

// streamingClient 让假供应商回放给定的流。
func streamingClient(t *testing.T, body string) *model.Client {
	t.Helper()
	return newTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, body)
	})
}

// collectDeltas 返回一个记录所有增量的回调。
func collectDeltas(deltas *[]domain.TextDelta) domain.DeltaFunc {
	return func(delta domain.TextDelta) { *deltas = append(*deltas, delta) }
}

// 请求必须落在 /chat/completions，带 Bearer 认证，声明 stream，并按视图原顺序携带消息。
func TestCompleteSendsProtocolConformingRequest(t *testing.T) {
	var path, authorization, contentType string
	var captured struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	client := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		authorization = request.Header.Get("Authorization")
		contentType = request.Header.Get("Content-Type")
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		_, _ = io.WriteString(writer, streamOf(contentFrame("好的")))
	})

	view := []domain.ModelMessage{
		{Role: domain.ModelRoleSystem, Content: "你是 GoSeek"},
		{Role: domain.ModelRoleUser, Content: "你好"},
	}
	if _, err := client.Complete(context.Background(), domain.ModelRequest{Messages: view}, nil); err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if path != "/chat/completions" {
		t.Errorf("请求路径 = %q; want %q", path, "/chat/completions")
	}
	if authorization != "Bearer test-key" {
		t.Errorf("Authorization = %q", authorization)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if captured.Model != "test-model" {
		t.Errorf("model = %q", captured.Model)
	}
	if !captured.Stream {
		t.Error("请求没有声明 stream")
	}
	if len(captured.Messages) != 2 ||
		captured.Messages[0].Role != "system" || captured.Messages[1].Content != "你好" {
		t.Errorf("消息 = %+v", captured.Messages)
	}
}

// 正文分片要按顺序交给回调，并拼成完整的 Content。
func TestCompleteAssemblesStreamedText(t *testing.T) {
	client := streamingClient(t, streamOf(
		contentFrame("我是"), contentFrame(" Go"), contentFrame("Seek"),
	))

	var deltas []domain.TextDelta
	response, err := client.Complete(context.Background(), domain.ModelRequest{}, collectDeltas(&deltas))
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if response.Content != "我是 GoSeek" {
		t.Errorf("Content = %q; want %q", response.Content, "我是 GoSeek")
	}
	if len(deltas) != 3 {
		t.Fatalf("收到 %d 个增量; want 3", len(deltas))
	}
	for _, delta := range deltas {
		if delta.Reasoning {
			t.Errorf("正文增量被标成了思考: %+v", delta)
		}
	}
}

// 思考过程要标记出来单独展示，且**不能混进正文**——它不是对话的一部分。
func TestCompleteSeparatesReasoningFromContent(t *testing.T) {
	client := streamingClient(t, streamOf(
		reasoningFrame("用户想"), reasoningFrame("看目录"), contentFrame("好的"),
	))

	var deltas []domain.TextDelta
	response, err := client.Complete(context.Background(), domain.ModelRequest{}, collectDeltas(&deltas))
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if response.Content != "好的" {
		t.Errorf("Content = %q; want %q（思考内容不能进正文）", response.Content, "好的")
	}
	if len(deltas) != 3 {
		t.Fatalf("收到 %d 个增量; want 3", len(deltas))
	}
	if !deltas[0].Reasoning || !deltas[1].Reasoning || deltas[2].Reasoning {
		t.Errorf("增量的 Reasoning 标记不对: %+v", deltas)
	}
}

// GLM 风格的思考字段是 reasoning，而不是 DeepSeek 的 reasoning_content。
// 两个字段都要进同一条思考流，前端才不需要按模型分支。
func TestCompleteAcceptsGLMReasoningField(t *testing.T) {
	client := streamingClient(t, streamOf(
		`{"choices":[{"delta":{"content":null,"reasoning":"思考中"}}]}`,
		contentFrame("正文"),
	))

	var deltas []domain.TextDelta
	response, err := client.Complete(context.Background(), domain.ModelRequest{}, collectDeltas(&deltas))
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if response.Content != "正文" {
		t.Errorf("Content = %q; want 正文", response.Content)
	}
	if len(deltas) != 2 || !deltas[0].Reasoning || deltas[1].Reasoning {
		t.Fatalf("增量 = %+v; want 一个思考加一个正文", deltas)
	}
	if deltas[0].Text != "思考中" {
		t.Errorf("思考内容 = %q; want 思考中", deltas[0].Text)
	}
}

// 这是流式解析最容易写错的地方：工具调用按 index 分片到达，
// id 和 name 只在首片出现，arguments 要跨片拼接。
func TestCompleteAccumulatesToolCallFragments(t *testing.T) {
	client := streamingClient(t, streamOf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"bash","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"com"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"mand\":\"ls\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	))

	response, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if len(response.ToolCalls) != 1 {
		t.Fatalf("解出 %d 个工具调用; want 1", len(response.ToolCalls))
	}
	call := response.ToolCalls[0]
	if call.ID != "call-a" || call.Name != "bash" {
		t.Errorf("调用 = %+v", call)
	}
	if string(call.Arguments) != `{"command":"ls"}` {
		t.Errorf("参数 = %s; want 拼接完整的 {\"command\":\"ls\"}", call.Arguments)
	}
}

// 多个调用的分片会交错到达，按 index 归拢，顺序按各自首次出现——
// 模型给出的调用顺序是有意义的，工具要按这个顺序串行执行。
func TestCompleteKeepsInterleavedToolCallsInFirstAppearanceOrder(t *testing.T) {
	client := streamingClient(t, streamOf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-first","function":{"name":"bash","arguments":"{\"command\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call-second","function":{"name":"bash","arguments":"{\"command\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"pwd\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
	))

	response, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if len(response.ToolCalls) != 2 {
		t.Fatalf("解出 %d 个工具调用; want 2", len(response.ToolCalls))
	}
	if response.ToolCalls[0].ID != "call-first" || response.ToolCalls[1].ID != "call-second" {
		t.Errorf("顺序 = %q、%q", response.ToolCalls[0].ID, response.ToolCalls[1].ID)
	}
	if string(response.ToolCalls[0].Arguments) != `{"command":"ls"}` {
		t.Errorf("第一个调用的参数 = %s", response.ToolCalls[0].Arguments)
	}
	if string(response.ToolCalls[1].Arguments) != `{"command":"pwd"}` {
		t.Errorf("第二个调用的参数 = %s", response.ToolCalls[1].Arguments)
	}
}

// 文字和工具调用可以同时出现。
func TestCompleteHandlesTextAlongsideToolCalls(t *testing.T) {
	client := streamingClient(t, streamOf(
		contentFrame("我先看看"),
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"bash","arguments":"{}"}}]}}]}`,
	))

	response, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}
	if response.Content != "我先看看" || len(response.ToolCalls) != 1 {
		t.Errorf("响应 = %+v", response)
	}
}

// 收尾帧里可能只有 usage 而没有 choices，忽略它而不是当成错误。
func TestCompleteIgnoresFramesWithoutChoices(t *testing.T) {
	client := streamingClient(t, streamOf(
		contentFrame("好"),
		`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
	))

	response, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}
	if response.Content != "好" {
		t.Errorf("Content = %q", response.Content)
	}
}

// SSE 的空行是帧分隔符，冒号开头的是心跳注释，两者都不能当成数据。
func TestCompleteSkipsBlankLinesAndComments(t *testing.T) {
	body := ": keep-alive\n\n" + streamOf(contentFrame("好"))
	client := streamingClient(t, body)

	response, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}
	if response.Content != "好" {
		t.Errorf("Content = %q", response.Content)
	}
}

// 单行超过 bufio.Scanner 默认的 64KB 上限时不能被静默截断——
// 那种 bug 只在长回复时出现，而且看起来像是模型的问题。
func TestCompleteHandlesLinesLargerThanSixtyFourKilobytes(t *testing.T) {
	huge := strings.Repeat("字", 40000) // 每个 3 字节，远超 64KB
	client := streamingClient(t, streamOf(contentFrame(huge)))

	response, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}
	if response.Content != huge {
		t.Errorf("长回复被截断了：收到 %d 个字符，want %d",
			len([]rune(response.Content)), len([]rune(huge)))
	}
}

// 流在结束标记之前断掉说明内容不完整——尤其工具调用的参数可能只拼了一半，
// 拿去执行是危险的，必须报错而不是当成一次正常响应。
func TestCompleteRejectsStreamTruncatedBeforeDone(t *testing.T) {
	client := streamingClient(t,
		"data: "+contentFrame("我正要说")+"\n\ndata: "+
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"bash","arguments":"{\"command\":\"rm"}}]}}]}`+"\n\n")

	if _, err := client.Complete(context.Background(), domain.ModelRequest{}, nil); err == nil {
		t.Fatal("中断的流被当成了正常响应")
	} else if !strings.Contains(err.Error(), "中断") {
		t.Errorf("错误信息 = %q", err.Error())
	}
}

// 供应商侧的每一类异常都必须变成带有可诊断信息的 error。
func TestCompleteRejectsUnusableResponses(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantInText string
	}{
		{
			name:       "认证失败时带出供应商说明",
			status:     http.StatusUnauthorized,
			body:       `{"error":{"message":"Authentication Fails","type":"authentication_error"}}`,
			wantInText: "Authentication Fails",
		},
		{
			name:       "非 JSON 的错误页退回正文片段",
			status:     http.StatusBadGateway,
			body:       "<html>bad gateway</html>",
			wantInText: "502",
		},
		{
			name:       "分片不是合法 JSON",
			status:     http.StatusOK,
			body:       "data: 不是 JSON\n\ndata: [DONE]\n\n",
			wantInText: "解析流式分片失败",
		},
		{
			name:       "出现无法识别的行",
			status:     http.StatusOK,
			body:       "event: message\n\ndata: [DONE]\n\n",
			wantInText: "无法识别的行",
		},
		{
			name:   "工具调用缺少 id",
			status: http.StatusOK,
			body: streamOf(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{}"}}]}}]}`),
			wantInText: "没有 id",
		},
		{
			name:   "同一响应里 id 重复",
			status: http.StatusOK,
			body: streamOf(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"bash","arguments":"{}"}}]}}]}`),
			wantInText: "重复",
		},
		{
			name:   "工具调用没有工具名",
			status: http.StatusOK,
			body: streamOf(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"arguments":"{}"}}]}}]}`),
			wantInText: "没有给出工具名",
		},
		{
			name:   "参数不是 JSON 对象",
			status: http.StatusOK,
			body: streamOf(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"[1,2]"}}]}}]}`),
			wantInText: "不是一个 JSON 对象",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := newTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				_, _ = io.WriteString(writer, testCase.body)
			})

			got, err := client.Complete(context.Background(), domain.ModelRequest{}, nil)
			if err == nil {
				t.Fatalf("Complete 返回 %+v; want error", got)
			}
			if !strings.Contains(err.Error(), testCase.wantInText) {
				t.Errorf("错误信息 = %q; want 包含 %q", err.Error(), testCase.wantInText)
			}
		})
	}
}

// 工具定义必须按协议包在 {"type":"function","function":{...}} 里发出，Schema 原样透传。
func TestCompleteSendsToolDefinitions(t *testing.T) {
	var captured struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}

	client := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		_, _ = io.WriteString(writer, streamOf(contentFrame("好的")))
	})

	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
	_, err := client.Complete(context.Background(), domain.ModelRequest{
		Tools: []domain.ToolSpec{{Name: "bash", Description: "运行命令", Parameters: schema}},
	}, nil)
	if err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}

	if len(captured.Tools) != 1 {
		t.Fatalf("发出 %d 个工具定义; want 1", len(captured.Tools))
	}
	tool := captured.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "bash" {
		t.Errorf("工具定义 = %+v", tool)
	}
	var sent map[string]any
	_ = json.Unmarshal(tool.Function.Parameters, &sent)
	if sent["type"] != "object" {
		t.Errorf("参数 Schema 被改动: %s", tool.Function.Parameters)
	}
}

// 没有工具时不应发出 tools 字段：部分供应商不接受空数组。
func TestCompleteOmitsToolsFieldWhenThereAreNoTools(t *testing.T) {
	var raw map[string]any

	client := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &raw)
		_, _ = io.WriteString(writer, streamOf(contentFrame("好的")))
	})

	if _, err := client.Complete(context.Background(), domain.ModelRequest{}, nil); err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}
	if _, present := raw["tools"]; present {
		t.Errorf("没有工具时仍然发出了 tools 字段: %v", raw["tools"])
	}
}

// onDelta 为 nil 时不能崩：调用方不关心展示是合法用法。
func TestCompleteToleratesNilDeltaCallback(t *testing.T) {
	client := streamingClient(t, streamOf(contentFrame("好"), reasoningFrame("想")))

	if _, err := client.Complete(context.Background(), domain.ModelRequest{}, nil); err != nil {
		t.Fatalf("Complete 返回错误: %v", err)
	}
}

// 调用方取消 context 时，Complete 必须立即返回错误而不是等到超时。
func TestCompleteRespectsCanceledContext(t *testing.T) {
	client := streamingClient(t, streamOf(contentFrame("不该被读到")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Complete(ctx, domain.ModelRequest{}, nil); err == nil {
		t.Fatal("Complete 在 context 已取消时返回了 nil error")
	}
}

// 供应商在请求超窗时会把**真实窗口**写在错误信息里。那是整个系统里唯一能拿到
// 这个真值的地方（/models 接口不返回它），所以必须抠出来——否则用户只能对着
// 一句英文报错自己琢磨该把 GOSEEK_CONTEXT_WINDOW 设成多少。
func TestContextWindowFromError(t *testing.T) {
	// 2026-08-27 从 DeepSeek 实测抓到的原文。
	real := errors.New("模型返回 HTTP 400: This model's maximum context length is " +
		"1048576 tokens. However, you requested 1200085 tokens (1200084 in the " +
		"messages, 1 in the completion). Please reduce the length of the messages " +
		"or completion. (invalid_request_error)")

	if got := model.ContextWindowFromError(real); got != 1048576 {
		t.Errorf("ContextWindowFromError = %d; want 1048576", got)
	}

	// 别的错误里没有这个信息，不能瞎猜一个数出来。
	for _, other := range []error{
		nil,
		errors.New("模型返回 HTTP 401: Authentication Fails"),
		errors.New("请求模型失败: context deadline exceeded"),
		errors.New("maximum context length is 很多 tokens"),
	} {
		if got := model.ContextWindowFromError(other); got != 0 {
			t.Errorf("从 %v 里抠出了 %d; want 0", other, got)
		}
	}
}
