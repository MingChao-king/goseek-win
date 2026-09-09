// Command mock-provider 是一个假的 OpenAI-compatible 供应商，用于离线冒烟测试。
//
// 用途：不消耗真实 API 额度、不需要真实中转，就能验证 GoSeek 的完整链路
// （HTTP 服务 → 会话 → 模型请求（流式 SSE）→ 工具调用 → 观察回填 → 文本回复）。
//
// 行为：
//   - 请求里没有 tool 角色的消息（第一轮）→ 返回一次 write_file 工具调用，
//     在 workspace 写入 result.txt；
//   - 请求里已有 tool 角色的消息（第二轮回合）→ 返回纯文本"测试完成"。
//
// 运行：
//
//	go run ./cmd/mock-provider            # 监听 127.0.0.1:19876
//	MOCK_PORT=20001 go run ./cmd/mock-provider
//
// 配合被测服务（另开终端）：
//
//	GOSEEK_API_KEY=test-key-not-real \
//	GOSEEK_BASE_URL=http://127.0.0.1:19876 \
//	GOSEEK_MODEL=mock-model \
//	GOSEEK_CONTEXT_WINDOW=128000 \
//	XDG_DATA_HOME=/tmp/goseek-smoke/data XDG_CONFIG_HOME=/tmp/goseek-smoke/config \
//	goseek serve --addr 127.0.0.1:18765
//
// 然后向 POST /api/v1/sessions/{id}/turns 提交两条消息，验证第二轮回复里
// 出现"测试完成"、workspace 里出现 result.txt。详见 scripts/smoke-test.sh。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
)

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

var calls atomic.Int64

func textChunk(text string) string {
	frame := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"created": 1, "model": "mock-model",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": text},
		}},
	}
	b, _ := json.Marshal(frame)
	return string(b)
}

func toolCallChunk(name, arguments string) string {
	frame := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"created": 1, "model": "mock-model",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": "call_mock_1", "type": "function",
				"function": map[string]any{"name": name, "arguments": arguments},
			}}},
		}},
	}
	b, _ := json.Marshal(frame)
	return string(b)
}

func finalChunk(finishReason string, promptTokens int) string {
	frame := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"created": 1, "model": "mock-model",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finishReason,
		}},
		"usage": map[string]any{"prompt_tokens": promptTokens},
	}
	b, _ := json.Marshal(frame)
	return string(b)
}

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func main() {
	http.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		hasToolRole := false
		var lastUser string
		for _, m := range body.Messages {
			if m.Role == "tool" {
				hasToolRole = true
			}
			if m.Role == "user" {
				var s string
				if json.Unmarshal(m.Content, &s) == nil {
					lastUser = s
				}
			}
		}
		n := calls.Add(1)
		log.Printf("completion #%d model=%s toolRole=%v lastUser=%q", n, body.Model, hasToolRole, lastUser)

		if !hasToolRole && len(body.Messages) <= 3 {
			// 第一轮：要求写文件。参数必须是合法 JSON 对象字符串。
			args, _ := json.Marshal(map[string]string{
				"path": "result.txt",
				"content": "GoSeek 冒烟测试通过。\n" +
					"本文件由被测服务在工具调用回回合中写入。\n",
			})
			sse(w, toolCallChunk("write_file", string(args)), finalChunk("tool_calls", 1234))
			return
		}
		sse(w, textChunk("测试完成：工具调用与文本回复都正常。"), finalChunk("stop", 2048))
	})

	port := os.Getenv("MOCK_PORT")
	if port == "" {
		port = "19876"
	}
	log.Printf("mock provider listening on 127.0.0.1:%s", port)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, nil))
}
