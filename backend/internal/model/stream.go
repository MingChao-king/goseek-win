package model

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"goseek/internal/domain"
)

// 本文件把供应商的**流式响应**拼回一个完整的 ModelResponse。
//
// # 为什么要流式
//
// 普通的 HTTP 请求是"发出去、等着、拿到完整响应"。模型生成一段回复要几秒到几十秒，
// 这段时间里用户面前是一片空白。流式的做法是：服务端一边生成一边把片段发回来，
// 客户端一边收一边显示。
//
// # HTTP 这一层发生了什么
//
// 服务端不再一次写完响应体然后关闭连接，而是用 chunked transfer encoding 把响应体
// 分块写出，连接一直开着。因此客户端**不能用 io.ReadAll**——那会一直阻塞到服务端
// 关闭连接为止，等于把流式退化回非流式。正确的做法是拿着 resp.Body 这个 io.Reader
// 增量地读，读到多少处理多少。
//
// # SSE 这一层：响应体里是什么
//
// 响应体的内容遵循 Server-Sent Events 这个文本协议。它极其简单：
//
//   - 一行一个 "字段: 值"，目前只用到 data 这一个字段；
//   - 空行表示"一个事件到此结束"；
//   - 以冒号开头的行是注释，通常用作心跳，防止中间的代理把闲置连接掐断。
//
// 一段真实的响应体长这样（每个 data 行后面都跟一个空行）：
//
//	data: {"choices":[{"delta":{"content":"我先"}}]}
//
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"bash","arguments":""}}]}}]}
//
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"com"}}]}}]}
//
//	data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}
//
//	data: [DONE]
//
// 值得一提：M3.2 里后端推给浏览器用的是同一个协议，只不过角色对调——那时我们是
// 发送方。所以这里读懂的东西，到那边正好反过来用一遍。
//
// # chat completions 在 SSE 之上的约定
//
//   - 每个 data 行的值是一段 JSON，称为一个 chunk；
//   - 唯一的例外是最后一行 data: [DONE]，它不是 JSON，是流结束的标记；
//   - **每个 chunk 携带的是增量而不是全量**。第二个 chunk 的 content 是"要追加的
//     那几个字"，不是"到目前为止的全部文字"。因此客户端必须自己累积。
//
// # 为什么最后还要拼成一个完整响应
//
// 流式只解决"显示"。会话历史里存的必须是完整的一条 assistant 消息，下一次请求也要
// 把它原样带上；工具调用更是必须等参数拼完整才能执行。所以本文件一边把片段交给
// onDelta 用于展示，一边在内部累积，最后归一化成一个和非流式完全一样的
// ModelResponse——上层因此不需要知道这次走的是哪条路。

const (
	// streamDataPrefix 是 SSE 数据行的前缀。
	streamDataPrefix = "data: "
	// streamDoneMarker 是供应商发出的结束标记。
	streamDoneMarker = "[DONE]"
	// streamLineLimit 是单行的上限。
	//
	// bufio.Scanner 的默认上限是 64KB，而一段长回复的某一帧可能超过——超了之后
	// Scanner 会**静默停止**，表现为"回复莫名其妙被截断"。这里改用 Reader 逐行
	// 读，不受那个限制；这个上限只用来防止一个畸形的响应把内存撑爆。
	streamLineLimit = 8 << 20
)

// streamChunk 是流式响应中的一帧，只声明当前真正读取的字段。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			// Content 是正文增量。供应商在没有正文时会把它写成 null，
			// 解码成空串正好，不需要区分 null 和空串。
			Content string `json:"content"`
			// ReasoningContent 是思考过程的增量，部分模型独有。
			ReasoningContent string `json:"reasoning_content"`
			// Reasoning 是另一个常见的思考字段：AISwitch 转发 GLM 5.3 时用它，
			// 而 DeepSeek 使用上面的 reasoning_content。同一个请求只会命中其一。
			Reasoning string `json:"reasoning"`
			// ToolCalls 是工具调用的增量分片。
			ToolCalls []streamToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage 只出现在最后一帧。实测 DeepSeek 会在 finish_reason 那一帧带上它，
	// 不需要额外请求 stream_options.include_usage。
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		// PromptCacheHitTokens 与 PromptCacheMissTokens 是这次输入里命中/未命中
		// 上下文缓存的部分。
		//
		// # 为什么要取
		//
		// DeepSeek 的上下文缓存默认开启、按**从第 0 个 token 起的完整前缀**匹配，
		// 命中 $0.014/M、未命中 $0.14/M（十倍），而且对延迟影响更大——官方给的
		// 数字是 128K 输入的首 token 从 13 秒降到 500 毫秒。
		//
		// 我们不为缓存做任何设计（实测一个 1073 万 token 的会话，把命中率从 88%
		// 拉到理论上限 95% 只省九分钱），但**必须能看见它**：命中率一旦崩掉，
		// 说明有人往请求前缀里塞了会变的东西（时间戳、计数器、随机序），
		// 那是一个静默的、只体现为"变慢变贵"的回归。
		//
		// 这也是这个项目第四次遇到同一件事：权威真值一直在响应里，只是没人去取。
		PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	} `json:"usage"`
}

// streamToolCallDelta 是一次工具调用的一个分片。
//
// 同一次调用的分片靠 Index 归拢：id 和 name 通常只在该 index 的第一片出现，
// arguments 则是逐片拼接的字符串。
type streamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// accumulatedToolCall 是一次工具调用正在被拼装的中间状态。
//
// 三个字段的到达方式不一样：id 和 name 各自只出现一次（在该调用的第一个分片里），
// arguments 则是一片一片追加的，所以用 Builder 而不是 string。
type accumulatedToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

// toolCallAccumulator 把交错到达的分片按 index 归拢成若干次完整的工具调用。
//
// # 为什么需要它
//
// 正文只要字符串追加就行，工具调用不行。模型一次可能提出多个调用，它们的分片在流里
// 是**交错**的——协议用 index 字段标明"这一片属于第几个调用"。一段真实的流可能是：
//
//	{"index":0,"id":"call_a","function":{"name":"bash","arguments":""}}
//	{"index":1,"id":"call_b","function":{"name":"bash","arguments":""}}
//	{"index":1,"function":{"arguments":"{\"command\":"}}
//	{"index":0,"function":{"arguments":"{\"command\":"}}
//	{"index":0,"function":{"arguments":"\"ls\"}"}}
//	{"index":1,"function":{"arguments":"\"pwd\"}"}}
//
// 而且 arguments 的切分完全没有规律，实测一个 {"command":"ls"} 会被拆成
// `{`、`"`、`command`、`"`、`: ` 这样的碎片——不能假设任何一片是完整的 JSON 片段。
//
// # 为什么是 map 加一个顺序切片
//
// 看起来用一个切片按 index 下标寻址更直接，但那要求 index 从 0 开始连续排列，
// 而协议只说它是个标识，没有这个承诺。map 负责归拢。
//
// order 单独记住"谁先出现"，因为 map 的遍历顺序在 Go 里是随机的，而**调用顺序是
// 有意义的**：工具要按模型给出的顺序串行执行，顺序乱了，先建目录再写文件就可能
// 变成先写文件再建目录。
type toolCallAccumulator struct {
	byIndex map[int]*accumulatedToolCall
	order   []int
}

// newToolCallAccumulator 创建一个空的累积器。
func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIndex: make(map[int]*accumulatedToolCall)}
}

// add 收下一个分片。
func (accumulator *toolCallAccumulator) add(delta streamToolCallDelta) {
	// 第一次见到这个 index，说明模型开了一个新的调用。
	call, seen := accumulator.byIndex[delta.Index]
	if !seen {
		call = &accumulatedToolCall{}
		accumulator.byIndex[delta.Index] = call
		accumulator.order = append(accumulator.order, delta.Index)
	}

	// id 和 name 只在首片带值，后续分片里是空串。用"非空才赋值"而不是直接覆盖，
	// 否则第二片的空串就会把首片带来的 id 抹掉——那样这次调用就永远配不上观察了。
	if delta.ID != "" {
		call.id = delta.ID
	}
	if delta.Function.Name != "" {
		call.name = delta.Function.Name
	}

	// 参数则相反：每一片都要追加，一片都不能漏。
	call.arguments.WriteString(delta.Function.Arguments)
}

// result 把累积结果按首次出现顺序转换成协议帧的形态。
//
// 转成 toolCallPayload 而不是直接转成 domain.ToolCall，是为了走和非流式**同一套**
// 校验（decodeToolCalls）。协议规则只有一份，不会出现"流式路径漏了某个检查"。
func (accumulator *toolCallAccumulator) result() []toolCallPayload {
	if len(accumulator.order) == 0 {
		return nil
	}
	payloads := make([]toolCallPayload, 0, len(accumulator.order))
	for _, index := range accumulator.order {
		call := accumulator.byIndex[index]
		payload := toolCallPayload{ID: call.id, Type: "function"}
		payload.Function.Name = call.name
		payload.Function.Arguments = call.arguments.String()
		payloads = append(payloads, payload)
	}
	return payloads
}

// readStream 读完整个流式响应，边读边把文字片段交给 onDelta，最后归一化成一个
// ModelResponse。
//
// 整个函数就是一个循环："读一行 → 判断这行是什么 → 相应地累积"，读到结束标记为止。
// 之所以不用 encoding/json 的 Decoder 直接解流，是因为响应体不是一串裸 JSON，
// 而是 SSE 文本——JSON 只出现在 data 行的冒号后面，得先按行拆开。
//
// 归一化之后的结果和非流式完全一致，包括那套协议校验（id 非空且唯一、工具名非空、
// 参数是一个 JSON 对象）——调用方因此不需要关心这次走的是哪条路。
func readStream(body io.Reader, onDelta domain.DeltaFunc) (domain.ModelResponse, error) {
	// bufio.Reader 在 body 之上做缓冲：底层每次网络读可能只拿到半行，也可能一次
	// 拿到好几行，缓冲层负责把它们攒起来再按行切开。
	//64 * 1024 缓冲区大小
	reader := bufio.NewReaderSize(body, 64<<10)

	// 两个累积器，对应响应里可能同时出现的两种内容。
	//
	// 正文是纯粹的字符串追加，用 Builder 就够；工具调用要复杂得多，因为多个调用
	// 的分片是交错到达的，得按 index 归拢——细节见 toolCallAccumulator。
	var content strings.Builder
	toolCalls := newToolCallAccumulator()
	// cacheHit / cacheMiss 记录供应商报告的缓存命中情况，与 promptTokens 同来源。
	cacheHit := 0
	cacheMiss := 0
	// promptTokens 记录供应商报告的真实输入 token。它只在最后一帧出现，
	// 因此在循环里持续覆盖，循环结束时留下的就是最终值。
	promptTokens := 0
	// finishReason 记录供应商的停止原因。空响应（无文字无工具调用）诊断依赖它：
	// stop 表示供应商认为正常结束——通常是思考模型把输出预算全花在思考上。
	finishReason := ""

	// completed 记录是否见到过 [DONE]。它决定这条流是正常读完还是中途断了，
	// 循环结束后据此判断——原因见下面的注释。
	completed := false

	for {
		line, err := readStreamLine(reader)
		if errors.Is(err, io.EOF) {
			// 服务端关闭了连接。是否正常取决于此前有没有见到 [DONE]。
			break
		}
		if err != nil {
			return domain.ModelResponse{}, err
		}

		// 按 SSE 的规则给这一行分类。三种情况直接跳过：
		//   - 空行：帧分隔符，不携带内容；
		//   - 以冒号开头：注释，通常是心跳；
		//   - 行尾的 \r：有些服务端按 CRLF 换行，先剥掉。
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}

		// 剩下的只可能是 "data: ..." 数据行。SSE 还定义了 event、id、retry 等字段，
		// chat completions 不用它们；真收到了说明这不是我们认识的流，与其猜测不如
		// 报错——静默忽略一个不认识的字段，可能就丢掉了半段回复。
		payload, found := strings.CutPrefix(trimmed, streamDataPrefix)
		if !found {
			return domain.ModelResponse{}, fmt.Errorf("流式响应中出现无法识别的行: %s", snippet([]byte(trimmed)))
		}

		// [DONE] 是唯一不是 JSON 的 data 值，必须在尝试解析之前判掉。
		if payload == streamDoneMarker {
			completed = true
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return domain.ModelResponse{}, fmt.Errorf("解析流式分片失败: %w (原文: %s)",
				err, snippet([]byte(payload)))
		}
		// usage 可能出现在带 choices 的最后一帧，也可能单独成帧，
		// 因此在判断 choices 之前先取。
		if chunk.Usage.PromptTokens > 0 {
			promptTokens = chunk.Usage.PromptTokens
			// 两个缓存字段和 prompt_tokens 同帧出现，一起取。**不判 > 0**：
			// 0 命中是一个有意义的值（前缀全新），当成"没报"会把最该看见的
			// 那一次记成缺失。
			cacheHit = chunk.Usage.PromptCacheHitTokens
			cacheMiss = chunk.Usage.PromptCacheMissTokens
		}
		if len(chunk.Choices) == 0 {
			// 只带 usage 而没有 choices 的收尾帧，取完 usage 就可以跳过。
			continue
		}

		if chunk.Choices[0].FinishReason != "" {
			finishReason = chunk.Choices[0].FinishReason
		}

		// 到这里才是真正有内容的一帧。delta 里的每个字段都是"这一次新增的部分"，
		// 因此下面全是追加操作，没有任何赋值覆盖。
		delta := chunk.Choices[0].Delta

		if delta.Content != "" {
			// 正文：一边累积成完整消息，一边交给展示。两件事都要做——累积的那份
			// 进会话历史，交出去的那份让用户现在就能看见。
			content.WriteString(delta.Content)
			onDelta(domain.TextDelta{Text: delta.Content})
		}
		if delta.ReasoningContent != "" || delta.Reasoning != "" {
			reasoning := delta.ReasoningContent
			if reasoning == "" {
				reasoning = delta.Reasoning
			}
			// 思考过程只交给展示，**不进 content**：它不是对话的一部分，既不写入
			// 历史也不回传给供应商。
			onDelta(domain.TextDelta{Text: reasoning, Reasoning: true})
		}
		for _, call := range delta.ToolCalls {
			// 工具调用的分片交给累积器按 index 归拢。这里不做任何展示：参数还没
			// 拼完整，半个 JSON 拿给用户看没有意义。
			toolCalls.add(call)
		}
	}

	// 没读到 [DONE] 就走到这里，说明连接在中途断了（网络问题、服务端崩溃、超时）。
	//
	// 此时手上的内容是残缺的，而且**残缺得看不出来**：正文会少一截，更危险的是工具
	// 调用的参数可能只拼了一半——一段截断的 JSON 拿去执行，运气好是解析失败，运气
	// 不好是解析成功但语义变了。所以必须报错，不能把它当成一次正常响应交上去。
	if !completed {
		return domain.ModelResponse{}, errors.New("流式响应在结束标记之前中断，内容不完整")
	}

	// 最后一步：把累积出来的工具调用还原成协议帧的形态，走和非流式**同一套**校验。
	// 共用这套校验是有意的——协议规则只有一份，不会出现"流式路径漏了某个检查"。
	decodedCalls, err := decodeToolCalls(toolCalls.result())
	if err != nil {
		return domain.ModelResponse{}, fmt.Errorf("模型返回的工具调用不合法: %w", err)
	}
	return domain.ModelResponse{
		Content:         content.String(),
		ToolCalls:       decodedCalls,
		PromptTokens:    promptTokens,
		CacheHitTokens:  cacheHit,
		CacheMissTokens: cacheMiss,
		FinishReason:    finishReason,
	}, nil
}

// readStreamLine 从缓冲流里读出一行（不含换行符），并限制单行长度。
//
// 为什么不用 bufio.Scanner：它是逐行读最顺手的工具，但单行上限默认 64KB，
// **超出后既不报错也不 panic，只是让 Scan() 返回 false**，看起来和"读完了"
// 一模一样。表现出来就是长回复被莫名截断——一个只在内容够长时才出现、
// 而且乍看像是模型问题的 bug。
//
// Reader.ReadLine 没有这个陷阱，但它有另一个约定：一行超过缓冲区时，它会分多次
// 返回，用 isPrefix=true 表示"这一行还没完"。因此这里要循环拼接，直到
// isPrefix 变成 false 才算读到完整的一行。
func readStreamLine(reader *bufio.Reader) (string, error) {
	var builder strings.Builder
	for {
		fragment, isPrefix, err := reader.ReadLine()
		if err != nil {
			if builder.Len() > 0 && errors.Is(err, io.EOF) {
				// 最后一行没有换行符收尾。已经读到的内容仍然是有效的一行，
				// 先交出去；下一次调用才会拿到干净的 EOF。
				return builder.String(), nil
			}
			return "", err
		}
		builder.Write(fragment)
		if builder.Len() > streamLineLimit {
			// 这不是正常响应会有的形态，多半是把非 SSE 的内容当成流在读。
			// 设上限只为不让内存被一路撑爆。
			return "", fmt.Errorf("流式响应中出现超过 %d 字节的行", streamLineLimit)
		}
		if !isPrefix {
			return builder.String(), nil
		}
	}
}
