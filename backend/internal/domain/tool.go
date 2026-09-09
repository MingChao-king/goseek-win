package domain

import (
	"encoding/json"
	"fmt"
)

// ToolSpec 告诉模型有哪一个工具可用、什么时候用、参数长什么样。
//
// 它随每次模型请求发送，是模型能够发起工具调用的前提。
type ToolSpec struct {
	// Name 是工具的唯一名称，模型按它发起调用。
	Name string `json:"name"`
	// Description 说明工具的用途，模型据此判断何时该用它。
	Description string `json:"description"`
	// Parameters 是参数的 JSON Schema。
	//
	// 用 json.RawMessage 而不是 map[string]any：这段 Schema 整体透传给供应商，
	// 程序自己不需要理解它的结构，解成 map 再编码回去只是两次无意义的转换。
	Parameters json.RawMessage `json:"parameters"`
}

// ToolCall 是模型提出的一次原生工具调用。
//
// 它只能来自供应商的 tool calling 协议。assistant 文本里的 JSON、XML 或 shell
// 代码块都不是 ToolCall，程序不解析它们。
type ToolCall struct {
	// ID 由供应商生成，用于和 ToolResult 配对。程序不改写它。
	ID string `json:"id"`
	// Name 是要调用的工具名称。它可能指向一个并不存在的工具，
	// 那种情况会形成一个 error 观察，而不是让本轮失败。
	Name string `json:"name"`
	// Arguments 是模型给出的参数 JSON 原文，由具体工具自己解释。
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResultStatus 是程序对一次工具调用"实际发生了什么"的判定。
type ToolResultStatus string

const (
	// ToolSuccess 表示工具正常完成。
	ToolSuccess ToolResultStatus = "success"
	// ToolError 表示调用在执行前被程序阻止，或者执行本身失败。
	ToolError ToolResultStatus = "error"
)

// ToolResult 是一次工具调用的唯一终态观察。
//
// 每个 ToolCall 最终恰好得到一个同 ID 的 ToolResult。未知工具、参数非法、命令
// 非零退出和超时都在这里表达：它们是模型可读的观察，不是程序错误，也不能被
// 伪装成给用户的最终回复。
type ToolResult struct {
	// ToolCallID 指回它所回应的那次调用，由注册表填写。
	ToolCallID string `json:"tool_call_id"`
	// Name 是产生这条观察的工具名称，由注册表填写。
	// 模型在长历史里需要知道某条观察来自哪个工具。
	Name string `json:"name"`
	// Status 是程序的判定：success 或 error。
	Status ToolResultStatus `json:"status"`
	// Content 是模型可读的观察正文。成功且为空表示命令确实没有输出。
	Content string `json:"content"`
	// ExitCode 只在命令真的跑起来并结束时存在。
	// 未知工具、参数非法、进程无法启动时为 nil。
	ExitCode *int `json:"exit_code,omitempty"`
	// Images 是这次工具产出、需要模型直接看到的图片。
	//
	// 典型场景是浏览器截图：模型必须"看到"页面才能决定下一步。图片以文件引用
	// 形式传递，Agent 负责把它们登记进会话的图片存储并在 tool 消息上绑定；
	// 工具自己只负责产出文件。没有图片时为空。
	Images []MessageImage `json:"images,omitempty"`
	// 这里曾经有一个 Truncated 字段。**它已经随截断本身一起被删除。**
	//
	// 后端不再截断任何东西，因此"你看到的不是全部"这种状态不复存在。留着一个
	// 恒为 false 的字段比删掉更糟：它会让读代码的人以为截断还可能发生，
	// 也会让模型在提示词里继续被告知一件不会发生的事。
}

// EncodeContent 把观察序列化成 tool 消息的正文。
//
// tool 消息保存的是整个 ToolResult 的 JSON，而不是裸的命令输出：只放输出会丢掉
// "成功但无输出"、"非零退出"和"根本没有执行"三者的区别，模型无法据此判断下一步。
//
// 本结构只包含字符串、布尔、整数和一个 *int，json.Marshal 对它不可能失败；万一
// 失败也不能让一次真实的观察从历史里消失，因此退回一段最小的等价 JSON，保证这次
// 调用仍然得到配对的观察。
func (result ToolResult) EncodeContent() string {
	encoded, err := encodeJSON(result)
	if err != nil {
		return fmt.Sprintf(`{"tool_call_id":%q,"name":%q,"status":%q,"content":"结果无法序列化"}`,
			result.ToolCallID, result.Name, ToolError)
	}
	return string(encoded)
}

// OutputFunc 接收工具执行过程中产生的每一片输出。
//
// 它让长命令不必等到结束才有动静。**回调可能运行在工具自己的 goroutine 上**
// （BashTool 的实现里就是 os/exec 的拷贝 goroutine），因此实现方不能假设自己
// 只被调用方所在的那一个 goroutine 调用。
//
// 同样没有返回值：展示失败不能影响命令的执行结果。
type OutputFunc func(chunk string)
