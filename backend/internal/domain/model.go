package domain

import "strings"

// ModelRequest 是一次供应商无关的模型请求。
type ModelRequest struct {
	// Messages 是本次调用的完整上下文视图，由 ContextManager 生成。
	Messages []ModelMessage
	// Tools 是本次调用允许模型使用的工具定义。
	//
	// 它由 Agent 直接提供，不经过 ContextManager：视图决定的是"模型看到哪些消息"，
	// 可用工具是另一件事，两者的责任方不同。
	Tools []ToolSpec
}

// ModelResponse 是模型一次调用归一化后的完整结果。
type ModelResponse struct {
	// Content 是模型生成的文字。
	// 当 ToolCalls 非空时它只是过程说明，不是给用户的最终回复。
	Content string
	// ToolCalls 是模型提出的原生工具调用，保持模型返回的顺序。
	ToolCalls []ToolCall
	// PromptTokens 是供应商报告的**本次请求真实输入 token 数**。
	//
	// 0 表示这次响应没有带 usage（不是所有供应商都给）。它比本地估算准确，
	// 拿到之后就用它覆盖估算值，见 ContextUsage.WithProviderTokens。
	//
	// 只取 prompt_tokens 不取 completion_tokens：这里关心的是"输入占了多少
	// 上下文"，而输出量属于费用与限额的范畴，那是 M5 的事。
	PromptTokens int
	// FinishReason 是供应商在最后一帧报告的停止原因（stop / tool_calls /
	// length / content_filter / 空）。空响应诊断依赖它：finish_reason=stop
	// 且内容为空说明供应商侧正常结束但没有产出——通常是思考模型把预算
	// 全花在思考上，正文一个字没写。
	FinishReason string
	// CacheHitTokens 与 CacheMissTokens 是这次输入里命中/未命中上下文缓存的部分。
	//
	// 供应商没有报这两个字段时都是 0——**因此不能用"两者都为 0"去判断"全部未命中"**，
	// 那和"供应商没报"分不开。要判断有没有数据，看 CacheHitTokens+CacheMissTokens
	// 是否等于 PromptTokens。
	//
	// 它们不参与任何决策，只用于观测：缓存命中率崩掉意味着请求前缀里混进了会变的
	// 东西，而那是一个只体现为"变慢变贵"的静默回归。
	CacheHitTokens  int
	CacheMissTokens int
}

// IsFinal 判断这次响应能否作为本轮的最终回复。
//
// 只有"非空文字 + 没有任何工具调用"才算。响应同时带文字和工具调用时，那段文字
// 只是过程说明，本轮还没有结束：工具执行后必须再次请求模型。
func (response ModelResponse) IsFinal() bool {
	return len(response.ToolCalls) == 0 && strings.TrimSpace(response.Content) != ""
}

// IsEmpty 表示模型既没有给出文字，也没有提出工具调用。
//
// 这样的响应无法推进本轮：它不能作为回复，也不产生任何可执行的动作，
// 属于模型协议错误。
func (response ModelResponse) IsEmpty() bool {
	return len(response.ToolCalls) == 0 && strings.TrimSpace(response.Content) == ""
}

// TextDelta 说明一段增量文字属于正文还是思考过程。
type TextDelta struct {
	// Text 是这一片文字。
	Text string
	// Reasoning 为 true 表示它来自模型的思考过程，而不是给用户的正文。
	//
	// 部分模型把两者分开流式输出，且耗时几乎全在思考阶段——不展示的话，用户在
	// 最长的那段时间里看不到任何动静。但思考内容不是对话的一部分：它既不写入
	// 会话历史，也不回传给供应商。
	Reasoning bool
}

// DeltaFunc 接收模型流式输出的每一片文字。
//
// 定义在 domain 而不是 agent：Model 接口声明在调用方 agent，实现在 model 包，
// 两边的方法签名必须逐字一致才能满足接口——把回调类型放进双方都依赖的 domain，
// 才不至于退化成到处写 func(TextDelta) 的裸函数类型。
//
// 它没有返回值：展示失败不能影响模型调用的结果。
type DeltaFunc func(delta TextDelta)
