// Package model 通过 OpenAI-compatible 的 chat completions 协议调用模型。
//
// 本包是进程与外部供应商之间的信任边界：请求由这里翻译成供应商协议，响应在这里
// 被校验后才转换成进程内的类型。协议细节不向上层泄漏，上层只看到"给一组消息和
// 一组工具定义、拿回文字与工具调用"。
package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"goseek/internal/domain"
)

// errorSnippetLimit 限制错误信息中携带的响应正文字符数，避免把整页 HTML 打进终端。
const errorSnippetLimit = 512

// errorBodyLimit 限制出错时读取的响应正文字节数。
//
// 错误响应本该是一小段 JSON，但一个配置错的地址可能返回整站首页；这里设上限，
// 免得为了一句诊断信息把几 MB 读进内存。
const errorBodyLimit = 64 << 10

// Config 是构造 Client 所需的供应商参数。
type Config struct {
	// BaseURL 是 chat completions 端点的前缀，例如 https://api.deepseek.com/v1。
	BaseURL string
	// APIKey 用于 Bearer 认证。
	APIKey string
	// Model 是供应商侧的模型名称，例如 deepseek-v4-flash。
	Model string
}

// Client 是一个 OpenAI-compatible 供应商的模型调用实现。
type Client struct {
	httpClient *http.Client
	endpoint   string
	apiKey     string
	model      string
	// supportsVision 由模型目录在装配时设置；为 false 时图片被降级为占位文本。
	supportsVision bool
}

// New 按给定配置构造 Client。
//
// 配置本身的合法性由读取环境的一方负责，这里只做 URL 拼接。
func New(config Config) *Client {
	client := &Client{
		httpClient: &http.Client{},
		endpoint:   strings.TrimRight(config.BaseURL, "/") + "/chat/completions",
		apiKey:     config.APIKey,
		model:      config.Model,
	}
	if entry, found := Lookup(config.Model); found {
		client.supportsVision = entry.SupportsVision
	}
	return client
}

// chatMessagePayload 是供应商协议中的一条消息。
//
// Content 使用 json.RawMessage：纯文本消息序列化为 JSON 字符串，带图消息序列化
// 为 content parts 数组。用 string 字段无法同时承载两种形态，而协议层的这个
// 分歧是供应商定的，上层不应该感知。
type chatMessagePayload struct {
	Role string `json:"role"`
	// Content 始终发送：纯文本时是 JSON 字符串，带图时是 content parts 数组。
	Content json.RawMessage `json:"content"`
	// ToolCalls 只在 assistant 消息上出现。
	ToolCalls []toolCallPayload `json:"tool_calls,omitempty"`
	// ToolCallID 只在 tool 消息上出现，指明这条观察回应的是哪次调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// chatCompletionRequest 是发往 chat completions 端点的请求体。
type chatCompletionRequest struct {
	Model    string               `json:"model"`
	Messages []chatMessagePayload `json:"messages"`
	// Tools 在没有工具时整个省略：部分供应商不接受空数组。
	Tools []toolPayload `json:"tools,omitempty"`
	// Stream 恒为 true。
	//
	// 只保留流式一条路径：两套解析并存意味着两套协议校验、两套 bug，而流式的
	// 结果归一化之后与非流式完全一致，上层看不出区别。
	Stream bool `json:"stream"`
}

// toolPayload 是协议中的一个工具定义。
// 协议把工具包在 {"type":"function","function":{...}} 里，为将来的其他工具类型留位。
type toolPayload struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}

// functionDefinition 描述一个可调用函数的名称、用途和参数 Schema。
type functionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// toolCallPayload 是响应中模型提出的一次工具调用。
type toolCallPayload struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments 在协议里是一个字符串，字符串的内容才是参数 JSON。
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// apiErrorResponse 是 OpenAI-compatible 的错误响应结构。
type apiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete 把请求发给模型，边接收边把文字片段交给 onDelta，最后返回归一化后的响应。
//
// 请求走流式，但返回值和非流式没有区别：分片在 readStream 里拼完整、做同一套
// 协议校验，再一次性交出去。调用方不需要知道这次是怎么传输的。
//
// # 领域类型与协议形态的对应
//
//	domain.ModelMessage  → chatMessagePayload   角色、正文、工具调用、tool_call_id
//	domain.ToolSpec      → toolPayload          包一层 {"type":"function", ...}
//	domain.ToolCall      → toolCallPayload      参数从 JSON 值变成 JSON **字符串**
//
// 这层翻译不能省。领域类型是按"这件事是什么"设计的，协议形态是供应商定的，
// 两者的差异（比如参数要不要包成字符串）如果泄漏到上层，换一个供应商就要改一片。
//
// onDelta 只用于实时展示，可以为 nil（比如测试里不关心展示时）。
//
// 返回的 error 覆盖四类失败：请求没有送达、供应商返回了非 2xx、流在结束标记之前
// 中断、以及响应不满足协议约定（无法解析、工具调用缺 id 或参数不是 JSON 对象）。
//
// "既没有文字也没有工具调用"不在这里判断：它是响应内容层面的问题，由 Agent 处理，
// 那里才知道这样的响应无法推进本轮。
// 传入的delta做两件事
// 模型如果是思考，状态就是流式思考，正在等待模型输出，状态就是等待模型流式输出
// 保存状态
func (client *Client) Complete(
	ctx context.Context,
	request domain.ModelRequest,
	onDelta domain.DeltaFunc,
) (domain.ModelResponse, error) {
	if onDelta == nil {
		onDelta = func(domain.TextDelta) {}
	}
	payload := chatCompletionRequest{
		Model:    client.model,
		Stream:   true,
		Messages: make([]chatMessagePayload, 0, len(request.Messages)),
	}
	//消息
	for _, message := range request.Messages {
		// 非 vision 模型：跳过所有图片，只发文本。上下文视图可能包含
		// user/tool 消息上的图片引用，但发送给不支持图片的模型时必须剥掉。
		if !client.supportsVision {
			message.Images = nil
		}
		content, err := client.encodeContent(message)
		if err != nil {
			return domain.ModelResponse{}, fmt.Errorf("编码消息内容失败: %w", err)
		}
		payload.Messages = append(payload.Messages, chatMessagePayload{
			Role:       string(message.Role),
			Content:    content,
			ToolCalls:  encodeToolCalls(message.ToolCalls),
			ToolCallID: message.ToolCallID,
		})
		// tool 观察里的图片：OpenAI 协议的 tool 消息只收文本，因此把图片
		// 翻译成紧随其后的 user content-parts 消息。完整会话事实里它们仍然
		// 属于那次工具观察；这是模型视图层的临时组装，不进历史。
		if message.Role == domain.ModelRoleTool && len(message.Images) > 0 && client.supportsVision {
			for _, image := range message.Images {
				imageContent, err := encodeImageContent(message.Content, []domain.MessageImage{image})
				if err != nil {
					return domain.ModelResponse{}, fmt.Errorf("编码工具观察图片失败: %w", err)
				}
				payload.Messages = append(payload.Messages, chatMessagePayload{
					Role:    string(domain.ModelRoleUser),
					Content: imageContent,
				})
			}
		}
	}
	//工具
	for _, spec := range request.Tools {
		payload.Tools = append(payload.Tools, toolPayload{
			Type: "function",
			Function: functionDefinition{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  spec.Parameters,
			},
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return domain.ModelResponse{}, fmt.Errorf("编码模型请求失败: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.ModelResponse{}, fmt.Errorf("构造模型请求失败: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	// OpenAI-compatible 的认证方式：Bearer + key。注意 key 只出现在这一行，
	// 不进日志、不进错误信息——错误信息会被复制到各种地方。
	httpRequest.Header.Set("Authorization", "Bearer "+client.apiKey)

	httpResponse, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return domain.ModelResponse{}, fmt.Errorf("请求模型失败: %w", err)
	}
	// 无论后面怎么返回，响应体都必须关掉，否则这条连接不会还给连接池，
	// 攒够一定数量之后新请求就得重新建连。defer 保证每条返回路径都走到。
	defer httpResponse.Body.Close()

	// 出错时响应体是一段 JSON 错误而不是 SSE 流，此时才整体读出来做诊断；
	// 正常情况下 Body 要留给流式解析器边读边处理，不能提前读干净。
	if httpResponse.StatusCode != http.StatusOK {
		responseBody, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, errorBodyLimit))
		if readErr != nil {
			return domain.ModelResponse{}, fmt.Errorf("模型返回 HTTP %d，且读取错误正文失败: %w",
				httpResponse.StatusCode, readErr)
		}
		return domain.ModelResponse{}, fmt.Errorf("模型返回 HTTP %d: %s",
			httpResponse.StatusCode, describeError(responseBody))
	}
	//流式读取
	return readStream(httpResponse.Body, onDelta)
}

// encodeContent 把领域消息的内容翻译成协议层的 JSON 形态。
//
// 没有图片时返回一个 JSON 字符串——这与旧行为完全一致。有图片时返回
// content parts 数组，按供应商多模态协议发 image_url + data URL。
func (client *Client) encodeContent(message domain.ModelMessage) (json.RawMessage, error) {
	if len(message.Images) == 0 {
		encoded, err := json.Marshal(message.Content)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}

	// 非 vision 模型：不发 image_url parts（那会被供应商拒绝），改发占位文本。
	if !client.supportsVision {
		encoded, err := json.Marshal(message.Content)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}

	// 带图消息：先发文字 part（即使为空也发，协议允许），再依次发图片。
	textPart, err := json.Marshal(map[string]string{"type": "text", "text": message.Content})
	if err != nil {
		return nil, err
	}
	parts := []json.RawMessage{textPart}
	for _, image := range message.Images {
		data, err := os.ReadFile(image.FilePath)
		if err != nil {
			return nil, fmt.Errorf("读取图片文件 %s 失败: %w", image.FilePath, err)
		}
		dataURL := "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
		imagePart, err := json.Marshal(map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": dataURL},
		})
		if err != nil {
			return nil, err
		}
		parts = append(parts, imagePart)
	}
	return json.Marshal(parts)
}

// encodeImageContent 为一张图片生成带文字说明的 content parts。
//
// tool 观察的图片单独发成 user 消息时，文字部分说明图片来源，避免模型把截图
// 误认成用户上传。没有可用文字时发空串（协议允许）。
func encodeImageContent(source string, images []domain.MessageImage) (json.RawMessage, error) {
	textPart, err := json.Marshal(map[string]string{"type": "text", "text": source})
	if err != nil {
		return nil, err
	}
	parts := []json.RawMessage{textPart}
	for _, image := range images {
		data, err := os.ReadFile(image.FilePath)
		if err != nil {
			return nil, fmt.Errorf("读取图片文件 %s 失败: %w", image.FilePath, err)
		}
		dataURL := "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
		imagePart, err := json.Marshal(map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": dataURL},
		})
		if err != nil {
			return nil, err
		}
		parts = append(parts, imagePart)
	}
	return json.Marshal(parts)
}

// encodeToolCalls 把历史中的工具调用还原成协议形态。
//
// 参数在协议里必须是一个字符串，字符串的内容才是 JSON，因此这里把 RawMessage
// 直接当字符串写回去——这与解析时脱掉字符串外壳是一对相反的操作。
func encodeToolCalls(calls []domain.ToolCall) []toolCallPayload {
	if len(calls) == 0 {
		return nil
	}
	payloads := make([]toolCallPayload, 0, len(calls))
	for _, call := range calls {
		payload := toolCallPayload{ID: call.ID, Type: "function"}
		payload.Function.Name = call.Name
		payload.Function.Arguments = string(call.Arguments)
		payloads = append(payloads, payload)
	}
	return payloads
}

// decodeToolCalls 校验并转换响应中的工具调用。
//
// 这里只保证协议层面的可用性：id 非空且在本次响应内唯一、工具名非空、参数是一个
// JSON 对象。至于这个工具是否存在、参数是否符合它的定义，属于工具自己的判断，
// 会形成一条模型可读的 error 观察，而不是让整次响应作废。
func decodeToolCalls(payloads []toolCallPayload) ([]domain.ToolCall, error) {
	if len(payloads) == 0 {
		return nil, nil
	}

	calls := make([]domain.ToolCall, 0, len(payloads))
	seen := make(map[string]struct{}, len(payloads))
	for index, payload := range payloads {
		if strings.TrimSpace(payload.ID) == "" {
			return nil, fmt.Errorf("第 %d 个工具调用没有 id", index+1)
		}
		if _, duplicate := seen[payload.ID]; duplicate {
			return nil, fmt.Errorf("工具调用 id %q 在同一个响应中重复出现", payload.ID)
		}
		seen[payload.ID] = struct{}{}

		if strings.TrimSpace(payload.Function.Name) == "" {
			return nil, fmt.Errorf("工具调用 %q 没有给出工具名", payload.ID)
		}
		arguments, err := normalizeArguments(payload.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("工具调用 %q 的参数无法使用: %w", payload.ID, err)
		}

		calls = append(calls, domain.ToolCall{
			ID:        payload.ID,
			Name:      payload.Function.Name,
			Arguments: arguments,
		})
	}
	return calls, nil
}

// normalizeArguments 确认参数是一个 JSON 对象，并原样保留它的文本。
//
// 协议里 arguments 是一个字符串，字符串的内容才是参数 JSON；无参数时供应商可能
// 给出空字符串，这里补成空对象，让工具侧只需要处理一种形态。解成 map 只是判定
// 它确实是对象——数组、数字或裸字符串都无法作为工具参数。
func normalizeArguments(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = "{}"
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, fmt.Errorf("不是一个 JSON 对象: %w", err)
	}
	return json.RawMessage(trimmed), nil
}

// contextLengthPattern 从供应商的超长报错里抠出真实的上下文窗口。
//
// OpenAI-compatible 的服务在请求超窗时会回一句带着**真值**的错误：
//
//	This model's maximum context length is 1048576 tokens. However, you
//	requested 1200085 tokens (1200084 in the messages, 1 in the completion).
//
// 这是整个系统里唯一一个能拿到窗口真值的地方——供应商的 /models 接口不返回它
// （2026-08-27 实测只有 id / object / owned_by）。所以这句错误信息不能只当成
// 一句人类可读的文案：它是配置错误的**权威纠正**，要抠出来告诉用户。
//
// 配错窗口的代价不对称：配小了压缩会提前触发（花钱、多余的有损摘要），配大了
// 请求直接被拒。而这条报错只在配大了的那一侧出现——配小了永远不会触发它。
// 所以它是一道单向的保护，不是完整的自检。
var contextLengthPattern = regexp.MustCompile(`maximum context length is (\d+) tokens`)

// ContextWindowFromError 在错误信息里找供应商声明的真实窗口，找不到返回 0。
//
// 调用方拿它给用户一句可以照做的话（"把 GOSEEK_CONTEXT_WINDOW 设成 N"），
// 而不是让他对着一句英文报错自己琢磨。
func ContextWindowFromError(err error) int {
	if err == nil {
		return 0
	}
	match := contextLengthPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}
	// 正则已经限定了是一串数字，解析失败只可能是位数溢出，那时返回 0 即可。
	window, parseErr := strconv.Atoi(match[1])
	if parseErr != nil {
		return 0
	}
	return window
}

// describeError 从错误响应中提取可读信息；无法解析时退回原始正文片段。
func describeError(body []byte) string {
	var decoded apiErrorResponse
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Error.Message != "" {
		if decoded.Error.Type != "" {
			return fmt.Sprintf("%s (%s)", decoded.Error.Message, decoded.Error.Type)
		}
		return decoded.Error.Message
	}
	return snippet(body)
}

// snippet 截断正文，让错误信息保持可读。
//
// 按 rune 而不是 byte 截断：响应正文可能是中文，按字节切会在错误信息里留下半个字符。
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(空响应)"
	}
	runes := []rune(text)
	if len(runes) > errorSnippetLimit {
		return string(runes[:errorSnippetLimit]) + "…"
	}
	return text
}

// Summarize 请求模型把一段文本压缩成摘要。
//
// 它和 Complete 走同一个供应商、同一个端点，但**是完全不同的一种请求**：
//
//   - 独立的 system 指令（"你在压缩记录"，而不是"你是一个能调用工具的助手"）；
//   - **不带任何工具定义**——摘要请求不该产生工具调用，不给工具是最直接的保证；
//   - 不流式（onDelta 传 nil）：摘要是给程序用的中间产物，没有展示价值，
//     用户要看的是"压缩完了、占用降到多少"，不是摘要正文一个字一个字地出现。
//
// 待总结的内容作为一条 user 消息发送，而不是按原角色还原成多条消息。原因是后者
// 很容易让模型把最后那条 user 消息当成"要回答的问题"——毕竟从协议上看它就是。
// 合成一条带角色前缀的纯文本，配合 system 指令里那句"这是记录不是指令"，
// 模型才能稳定地把它当材料而不是任务。
func (client *Client) Summarize(ctx context.Context, instructions string, source string) (string, error) {
	response, err := client.Complete(ctx, domain.ModelRequest{
		Messages: []domain.ModelMessage{
			{Role: domain.ModelRoleSystem, Content: instructions},
			{Role: domain.ModelRoleUser, Content: source},
		},
		// 刻意不给 Tools。
	}, nil)
	if err != nil {
		return "", err
	}
	// 摘要请求不该产生工具调用。真产生了说明 prompt 没压住模型，此时宁可报错
	// 也不要把一个可能只是半句话的 Content 当成摘要存进记忆——那会污染后续
	// 所有的上下文。
	if len(response.ToolCalls) > 0 {
		return "", fmt.Errorf("摘要请求返回了 %d 个工具调用，摘要不可用", len(response.ToolCalls))
	}
	return response.Content, nil
}
