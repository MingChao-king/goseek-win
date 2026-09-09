// Package domain 定义 GoSeek 的核心数据类型。
//
// 本包不依赖项目内的任何其他包，也不感知供应商协议、传输方式和存储实现。
// 它区分两类消息：Message 是会话中已经发生的事实，ModelMessage 是为一次模型
// 调用临时组装的输入。两者字段目前相同，但生命周期和责任方不同，不能互相替代。
package domain

// Role 是会话历史中一条消息的角色。
//
// 会话历史只记录用户与助手之间实际发生的对话。稳定的 system 指令不是会话事实，
// 它由 ContextManager 组装进模型视图，因此只存在于 ModelRole 中。
type Role string

const (
	// RoleUser 表示消息来自用户。
	RoleUser Role = "user"
	// RoleAssistant 表示消息来自模型。
	RoleAssistant Role = "assistant"
	// RoleTool 表示消息保存的是一次工具调用的观察结果。
	RoleTool Role = "tool"
)

// ModelRole 是模型上下文视图中一条消息的角色。
//
// 相比 Role 多出 system：它承载稳定指令，由 ContextManager 在每次构建视图时生成，
// 不进入会话历史。
type ModelRole string

const (
	// ModelRoleSystem 表示该消息是本次调用的稳定指令。
	ModelRoleSystem ModelRole = "system"
	// ModelRoleUser 表示该消息来自用户。
	ModelRoleUser ModelRole = "user"
	// ModelRoleAssistant 表示该消息来自模型。
	ModelRoleAssistant ModelRole = "assistant"
	// ModelRoleTool 表示该消息是一次工具调用的观察结果。
	ModelRoleTool ModelRole = "tool"
)

// ModelRole 返回该会话角色在模型视图中的对应角色。
//
// user、assistant 和 tool 在两个角色集合中同名，因此这是一次直接映射；system 不会
// 经过这里，因为它不是会话历史中的角色。
func (role Role) ModelRole() ModelRole {
	return ModelRole(role)
}

// Message 是会话历史中的一条事实。
//
// 历史是 append-only 的：Message 一旦追加就不再修改或删除，即使本轮交互随后失败。
// 从消息结构上理解的话
// user消息既不会有ToolCalls，也不会有ToolCallID
// assistant消息会有ToolCalls，里面每个ToolCall会有一个供应商提供的ID，但是不会有ToolCallID
// tool消息，不会有ToolCalls，但是会有ToolCallID，代表这一次工具执行情况
type Message struct {
	// Role 说明这条消息由谁产生。
	Role Role
	// Content 是消息正文。
	//
	// user 和 assistant 消息保存原始文字，不做解析或改写；tool 消息保存的是整个
	// ToolResult 的 JSON，见 ToolResult.EncodeContent。assistant 消息在只提出
	// 工具调用、没有附带说明时，正文可以为空。
	Content string
	// ToolCalls 是这条 assistant 消息提出的工具调用，保持模型返回的顺序。
	// 其他角色的消息为空。
	ToolCalls []ToolCall
	// ToolCallID 是这条 tool 消息所回应的那次调用。其他角色的消息为空。
	ToolCallID string
	// TurnID 指明这条消息属于哪一轮交互。
	//
	// 一轮从用户消息开始，到最终回复或本轮失败为止，中间的每条消息都带同一个值。
	// 界面据此把一轮里的提问、过程说明、工具调用和观察归成一组展示。
	TurnID TurnID
	// Images 是这条消息附带的图片，按产生顺序排列。
	//
	// user 消息的图片来自用户上传；tool 消息的图片来自工具产出（如浏览器截图）。
	// assistant 消息不携带图片。Content 仍然是文本正文，图片是附属媒体，两者分开存。
	Images []MessageImage
}

// ModelMessage 是模型上下文视图中的一条消息。
//
// 它只对一次模型调用有效，不被持久化，也不代表会话事实。
type ModelMessage struct {
	// Role 说明这条消息在模型协议中的角色。
	Role ModelRole
	// Content 是消息正文。
	Content string
	// ToolCalls 是 assistant 消息提出的工具调用。其他角色为空。
	ToolCalls []ToolCall
	// ToolCallID 是 tool 消息所回应的那次调用。其他角色为空。
	//
	// 供应商协议要求每条 tool 消息都指明它回应的调用，否则无法把观察和调用对上。
	ToolCallID string
	// Images 是随这条消息发给模型的图片，按产生顺序排列。
	//
	// user 消息的图片随本条消息发 content parts；tool 消息的图片由协议层翻译成
	// 紧随其后的 user 图片消息（OpenAI 系协议 tool 消息只收文本）。纯文本消息
	// 此处为空，序列化时保持旧协议的字符串形态。
	Images []MessageImage
}

// MessageImage 描述一条消息附带的一张图片。
//
// 它保存的是文件引用而不是 base64 正文：数据库里存路径，取图走独立的 HTTP
// 端点。这样 messages.content 不会被几 MB 的字面量撑爆，压缩历史也不会
// 因为旧图而不可控地膨胀。
type MessageImage struct {
	// ID 是这张图片的唯一标识，形如 img_xxx。
	ID string `json:"id"`
	// FilePath 是图片在磁盘上的绝对路径，指向 dataDir/images/<session>/。
	FilePath string `json:"file_path,omitempty"`
	// MediaType 是图片的 MIME 类型，例如 image/png。
	MediaType string `json:"media_type,omitempty"`
	// Width 和 Height 是图片的像素尺寸，用于 token 估算。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}
