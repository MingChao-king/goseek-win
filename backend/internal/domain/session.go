package domain

import "time"

// Session 是一次会话的全部状态：它的身份、工作目录、消息历史和恢复所需的检查点。
//
// 历史是一个 append-only 的事实序列：只能在末尾追加，已经追加的消息不再被修改或
// 删除。这个保证由类型本身维持——内部切片不对外暴露，Messages 返回副本——而不是
// 靠调用方自觉或运行时校验。
type Session struct {
	// ID 是会话的应用 ID，同时也是它在磁盘上的文件名。
	ID SessionID
	// Workspace 是工具执行命令的目录，在会话创建时固定。
	//
	// 恢复会话时命令仍在这里执行，而不是恢复时所处的目录：历史里全是关于这个
	// 目录的事实，换地方执行会让模型收到一连串无法解释的"文件不存在"。
	Workspace string
	// Model 是这个会话使用的模型。空串表示沿用启动时的默认模型。
	//
	// 模型必须是会话属性：它决定上下文窗口，而压缩历史按当时的窗口生成。
	Model string
	// PendingToolCallID 是即将执行或正在执行的工具调用。
	//
	// 它在真正执行之前就被写入并落盘，因此进程中断后可以区分两种情况：这个
	// 调用可能已经执行并产生了副作用（结果未知），还是根本没有开始执行。
	// 一次响应里的多个调用串行执行，所以同一时刻只会有一个。
	PendingToolCallID string
	// Memory 是这个会话的摘要树与当前生效的摘要前沿。
	//
	// 它和 messages 是两样东西：messages 是**事实**，永不删除；Memory 是为了让
	// 有限的上下文窗口装得下更长的对话而做的**压缩产物**。压缩只改这里。
	Memory ConversationMemory
	// CreatedAt 是会话创建时间。
	CreatedAt time.Time
	// UpdatedAt 是最近一次保存时间，也就是会话列表里的"最后活动"。
	UpdatedAt time.Time

	// messages 按发生顺序保存全部消息。
	messages []Message
}

// NewSession 创建一个指定身份和工作目录的空会话。
//
// 时间戳由持久化层填写：它们记录的是"什么时候被创建和保存"，属于存储事实。
func NewSession(id SessionID, workspace string) *Session {
	return &Session{ID: id, Workspace: workspace}
}

// Append 在历史末尾追加一条消息。
func (session *Session) Append(message Message) {
	session.messages = append(session.messages, message)
}

// Messages 返回当前历史的只读快照。
//
// 返回的是副本。Go 的切片与底层数组共享内存，直接返回内部切片会让调用方能够就地
// 改写已经发生的事实，append-only 就退化成了一个约定；同时后续 Append 也可能通过
// 共享数组影响调用方已经取走的快照。
func (session *Session) Messages() []Message {
	snapshot := make([]Message, len(session.messages))
	copy(snapshot, session.messages)
	return snapshot
}

// LoadSession 用磁盘上读到的内容重建一个会话。
//
// 它和 NewSession 一样是构造入口，而不是对历史的修改：会话建成之后，写入历史的
// 唯一途径仍然只有 Append，append-only 的保证不受影响。
//
// 时间戳和 PendingToolCallID 由持久化层在构造后直接赋值——它们是普通字段，
// 不需要经过构造函数。
func LoadSession(id SessionID, workspace string, messages []Message) *Session {
	session := NewSession(id, workspace)
	session.messages = make([]Message, len(messages))
	copy(session.messages, messages)
	return session
}

// UnresolvedToolCalls 返回历史中还没有配对观察的工具调用，保持它们被提出的顺序。
//
// 正常情况下结果为空：一轮交互总会为每个调用写下一条 tool 消息。只有进程在两者
// 之间被中断，才会留下悬空的调用——而带着悬空调用无法再请求模型，供应商要求每个
// 工具调用都有对应的观察。
func (session *Session) UnresolvedToolCalls() []ToolCall {
	resolved := make(map[string]struct{})
	for _, message := range session.messages {
		//消息的角色为工具调用而且消息的id非空，说明该id对应的工具调用已完成，加入resolved中
		if message.Role == RoleTool && message.ToolCallID != "" {
			resolved[message.ToolCallID] = struct{}{}
		}
	}

	var open []ToolCall
	//现有消息进行扫描
	for _, message := range session.messages {
		//如果某条消息是包含要求工具调用的，即模型提出了这样的消息
		for _, call := range message.ToolCalls {
			//遍历这个工具调用的ID，如果不在resolved里面，代表该调用悬空
			if _, done := resolved[call.ID]; !done {
				open = append(open, call)
			}
		}
	}
	return open
}
