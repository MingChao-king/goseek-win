package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// RunState 表示 Agent 此刻在做什么，是界面上"运行进度"的事实来源。
//
// 它不落库。会话被重新打开时状态必然是 IDLE——上一轮要么已经收口，要么被
// Restore 修复过，没有第三种可能，因此没有需要持久化的中间状态。
type RunState string

const (
	// StateIdle 表示没有正在进行的轮次，等待用户输入。
	StateIdle RunState = "IDLE"
	// StateWaitingModel 表示正在等待模型响应。
	StateWaitingModel RunState = "WAITING_MODEL"
	// StateRunningTool 表示正在执行一次工具调用。
	StateRunningTool RunState = "RUNNING_TOOL"
	// StateCompressing 表示正在压缩上下文。
	//
	// 压缩要调用模型，可能耗时数秒。没有这个状态的话，界面在这段时间里看起来
	// 就是卡住了——而它其实在做一件用户应该知道的事。
	StateCompressing RunState = "COMPRESSING"
	// StateFailed 表示本轮以失败终止。
	StateFailed RunState = "FAILED"
)

// EventType 是运行事件的类型。
//
// 取值用点号分段的小写字符串，而不是 Go 的常量名：它们会原样出现在数据库、
// SSE 帧和前端代码里，可读性比类型安全更要紧——而类型安全由 EventType 这个
// 具名类型保证。
type EventType string

const (
	// EventTurnStarted 表示一轮交互开始。
	EventTurnStarted EventType = "turn.started"
	// EventStateChanged 表示 Agent 的运行状态发生变化。
	EventStateChanged EventType = "state.changed"
	// EventUserMessage 表示用户提交了一条消息。
	EventUserMessage EventType = "user.message"
	// EventAssistantDelta 是模型正文的一个增量片段。
	EventAssistantDelta EventType = "assistant.delta"
	// EventAssistantReasoningDelta 是模型思考过程的一个增量片段。
	//
	// 部分模型（例如本项目使用的 deepseek-v4-flash）把思考与正文分开流式输出，
	// 而且耗时几乎全在思考阶段。不展示它的话，用户在最长的那段时间里看不到任何
	// 动静。它只用于展示：既不写入会话历史，也不回传给供应商——它不是对话的一
	// 部分，塞回去既浪费上下文窗口也不符合协议。
	EventAssistantReasoningDelta EventType = "assistant.reasoning.delta"
	// EventAssistantMessage 表示一条完整的 assistant 消息已经产生。
	EventAssistantMessage EventType = "assistant.message"
	// EventToolStarted 表示一次工具调用即将执行。
	EventToolStarted EventType = "tool.started"
	// EventToolOutputDelta 是工具输出的一个增量片段。
	EventToolOutputDelta EventType = "tool.output.delta"
	// EventToolResolved 表示一次工具调用得到了它唯一的观察。
	EventToolResolved EventType = "tool.resolved"
	// EventFileChanged 表示 write_file 工具写入了一个文件。
	EventFileChanged EventType = "file.changed"
	// EventTurnCompleted 表示一轮交互以最终回复正常结束。
	EventTurnCompleted EventType = "turn.completed"
	// EventTurnFailed 表示一轮交互以失败终止。
	EventTurnFailed EventType = "turn.failed"
	// EventContextCompactionStarted 表示开始压缩上下文。
	EventContextCompactionStarted EventType = "context.compaction.started"
	// EventContextCompactionCompleted 表示压缩结束。
	EventContextCompactionCompleted EventType = "context.compaction.completed"
	// EventContextUsageUpdated 表示上下文占用有了新的数字。
	//
	// 它在两个时刻产生：构建完视图、请求发出之前（估算值），以及响应回来之后
	// （供应商实测值）。durable——重放会话时要能还原"当时占用了多少"，那是判断
	// 压缩是否及时的依据；它每轮只有两三条，不会像 delta 那样撑爆事件表。
	EventContextUsageUpdated EventType = "context.usage.updated"
)

// RunEvent 是一次运行状态变化。
//
// 事件分 durable 和 transient 两类。durable 事件占据一个会话内单调递增的
// sequence 并与消息在同一个事务里落库，重放它们能完整还原"发生过什么"；
// transient 事件（各种 delta）只实时推送，不占 sequence 也不落库——它们是
// 打字动画，丢了不影响事实。这是行为底线第 11 条。
type RunEvent struct {
	// Sequence 是会话内单调递增的序号，由持久化层在提交事务时分配。
	// transient 事件恒为 0，表示它不占位。
	Sequence int64
	// TurnID 指明这个事件属于哪一轮交互。
	TurnID TurnID
	// Type 是事件类型。
	Type EventType
	// Payload 是该类型自己的内容，已经序列化成 JSON。
	//
	// 用 json.RawMessage 而不是 map[string]any：类型安全放在构造点——每种事件
	// 有自己的 payload 结构体和构造函数，序列化在那里完成一次；而存储和传输本来
	// 就要 JSON，用 map 会多一次无意义的解码再编码，还丢掉字段名的编译期检查。
	Payload json.RawMessage
	// At 是事件发生的时间。
	At time.Time
}

// Durable 表示这个事件是否需要落库并占据一个 sequence。
//
// 判断写成"列出 transient 的类型"而不是反过来：新增事件类型时默认应当是
// durable，漏改这里只会让它多存一份，而反过来会让它悄悄从历史里消失。
func (event RunEvent) Durable() bool {
	switch event.Type {
	// 正在流式输出的话，并不落盘。其他情况，均落盘
	case EventAssistantDelta, EventAssistantReasoningDelta, EventToolOutputDelta:
		return false
	default:
		return true
	}
}

// 以下是各事件类型的 payload。字段用 JSON tag 固定线上名字：它们会出现在数据库、
// SSE 帧和前端代码里，改 Go 字段名不应该让已有数据和前端一起坏掉。

// StateChangedPayload 说明 Agent 切换到了哪个状态。
type StateChangedPayload struct {
	State RunState `json:"state"`
}

// UserMessagePayload 保存用户提交的原始文字。
type UserMessagePayload struct {
	Content string `json:"content"`
	// ImageIDs 是随消息一起发送的图片 ID 列表，前端据此渲染缩略图。
	ImageIDs []string `json:"image_ids,omitempty"`
}

// FileChangedPayload 保存一次文件变更的信息。
type FileChangedPayload struct {
	// Path 是模型传给 write_file 的原始路径（相对于 workspace 或绝对路径）。
	Path string `json:"path"`
	// TurnID 让前端把改动归到当前轮次。
	TurnID string `json:"turn_id"`
}

// TextDeltaPayload 是一段增量文字，正文和思考共用这个结构。
type TextDeltaPayload struct {
	Text string `json:"text"`
}

// AssistantMessagePayload 是一条完整的 assistant 消息。
type AssistantMessagePayload struct {
	// Content 是模型给出的文字，只提出工具调用时可以为空。
	Content string `json:"content"`
	// ToolCalls 是本次响应中提出的工具调用。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// Final 表示这条消息就是本轮给用户的最终回复。
	//
	// 它等价于"没有工具调用且文字非空"，但显式给出来，前端就不必自己重新推导
	// 一遍这条规则——推导规则散落到消费端是行为底线第 5、6 条最容易被破坏的
	// 方式。
	Final bool `json:"final"`
}

// ToolStartedPayload 描述即将执行的一次调用。
type ToolStartedPayload struct {
	Call ToolCall `json:"call"`
	// Title 是这次调用的一行人类可读标题，来自工具自己的描述。
	Title string `json:"title"`
}

// ToolOutputDeltaPayload 是某次工具调用输出的一个片段。
type ToolOutputDeltaPayload struct {
	ToolCallID string `json:"tool_call_id"`
	Chunk      string `json:"chunk"`
}

// ToolResolvedPayload 是一次调用的唯一观察。
type ToolResolvedPayload struct {
	Result ToolResult `json:"result"`
	// ImageIDs 是这次工具产出并已登记的图片 ID 列表，前端据此渲染缩略图。
	ImageIDs []string `json:"image_ids,omitempty"`
}

// ContextUsagePayload 是一次上下文占用快照。
//
// 展开成扁平字段而不是内嵌 ContextUsage：剩余容量和占用比例在领域里是方法，
// 而消费端（前端）拿到的是 JSON，方法过不去。在这里算好一并发出去，前端就不必
// 重新实现一遍"窗口未知时比例算 0""剩余不能为负"这些规则——规则散到消费端，
// 正是行为底线最容易被破坏的方式。
type ContextUsagePayload struct {
	ContextWindow int     `json:"context_window"`
	InputTokens   int     `json:"input_tokens"`
	Remaining     int     `json:"remaining"`
	Ratio         float64 `json:"ratio"`
	Source        string  `json:"source"`
	// CacheHitTokens / CacheMissTokens / CacheHitRatio 只在供应商报了缓存数据时出现
	// （omitempty）。它们不参与任何决策，是给人看的观测量：命中率崩掉意味着请求
	// 前缀里混进了会变的东西，而那是一个只体现为"变慢变贵"的静默回归。
	CacheHitTokens  int     `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int     `json:"cache_miss_tokens,omitempty"`
	CacheHitRatio   float64 `json:"cache_hit_ratio,omitempty"`
}

// NewContextUsagePayload 从一份占用信息构造 payload。
func NewContextUsagePayload(usage ContextUsage) ContextUsagePayload {
	payload := ContextUsagePayload{
		ContextWindow: usage.ContextWindow,
		InputTokens:   usage.InputTokens,
		Remaining:     usage.Remaining(),
		Ratio:         usage.Ratio(),
		Source:        string(usage.Source),
	}
	if usage.CacheKnown() {
		payload.CacheHitTokens = usage.CacheHitTokens
		payload.CacheMissTokens = usage.CacheMissTokens
		payload.CacheHitRatio = usage.CacheHitRatio()
	}
	return payload
}

// CompactionStartedPayload 说明为什么要压缩。
type CompactionStartedPayload struct {
	// InputTokens 是触发压缩时的占用。
	InputTokens int `json:"input_tokens"`
	// Threshold 是触发线。两个数字一起给出，用户才知道"为什么现在压"。
	Threshold int `json:"threshold"`
}

// CompactionCompletedPayload 说明压缩做了什么。
type CompactionCompletedPayload struct {
	// BeforeTokens 与 AfterTokens 是压缩前后的占用估算。
	BeforeTokens int `json:"before_tokens"`
	AfterTokens  int `json:"after_tokens"`
	// Batches 是本次新生成的摘要节点，界面据此展示"压缩了哪几段"。
	Batches []MemoryBatchSummary `json:"batches"`
	// TargetUnreachable 表示已经低于硬边界、但没到目标线。
	//
	// 界面要把它显示出来，否则用户看到占用还很高会以为压缩失败了。它**不是失败**：
	// 这一轮的上下文贵一些，下一轮压缩会继续。
	TargetUnreachable bool `json:"target_unreachable"`
	// Reason 说明为什么没压到目标线，或者收尾诊断发现了什么。
	//
	// 走到收尾诊断（用户原话被整理、切进保留区）不是正常工作状态，而是一个信号：
	// 这个会话该结束了，或者窗口配小了。只说"没压到"等于没说。
	Reason string `json:"reason,omitempty"`
	// Failed 非空时表示压缩没有成功完成，内容是原因。
	//
	// 压缩失败不算本轮失败：只要当前视图仍在硬边界内就照常请求。因此这里用一个
	// 字段而不是不发这个事件——不发的话界面会永远停在"正在压缩"。
	Failed string `json:"failed,omitempty"`
}

// MemoryBatchSummary 是一个摘要节点在事件里的形态。
//
// 只带界面要显示的东西，不带摘要正文：正文可能上千字，而界面上它是一行标题加
// 一个可展开的详情——详情按需通过 conversation_history 或快照拿。
type MemoryBatchSummary struct {
	ID    string `json:"id"`
	Level int    `json:"level"`
	// Title 是摘要的首行概括。
	Title string `json:"title"`
	// StartMessage 与 EndMessage 是覆盖的消息序号，从 1 开始、闭区间，
	// 因为它们是给人看的。
	StartMessage int `json:"start_message"`
	EndMessage   int `json:"end_message"`
}

// NewMemoryBatchSummary 从一个节点构造它在事件里的形态。
func NewMemoryBatchSummary(batch MemoryBatch) MemoryBatchSummary {
	return MemoryBatchSummary{
		ID:           string(batch.ID),
		Level:        batch.Level,
		Title:        batch.Title(),
		StartMessage: batch.StartMessageIndex + 1,
		EndMessage:   batch.EndMessageIndex,
	}
}

// TurnFailedPayload 说明这一轮为什么失败。
type TurnFailedPayload struct {
	Reason string `json:"reason"`
}

// NewEvent 构造一个事件。各类型的构造函数都走这里，保证 payload 的序列化方式一致。
func NewEvent(turnID TurnID, eventType EventType, payload any, at time.Time) RunEvent {
	return RunEvent{
		TurnID:  turnID,
		Type:    eventType,
		Payload: encodePayload(payload),
		At:      at,
	}
}

// encodePayload 把 payload 序列化成 JSON。
//
// 这里的 payload 只由字符串、布尔和几个已知结构体组成，其中唯一的"外来"字节是
// ToolCall.Arguments——而它在模型适配器和数据库读取两处都已经被验证为合法 JSON，
// 因此 Marshal 不可能失败。
//
// 万一失败也不能让事件整个消失：那会在事件序列里留下一个洞，而重放依赖序列完整。
// 因此退回一段说明性的 payload，让问题在数据里看得见，而不是变成一次静默丢失。
func encodePayload(payload any) json.RawMessage {
	encoded, err := encodeJSON(payload)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"encode_error":%q}`, err.Error()))
	}
	return encoded
}
