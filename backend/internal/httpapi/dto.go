package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"goseek/internal/domain"
)

// 本文件定义 HTTP 的线上形态，以及它与领域类型之间的映射。
//
// 为什么不直接把领域类型序列化出去：前端是另一个仓库目录里的另一套代码，一旦它
// 依赖了某个字段名，那个名字就成了契约。领域类型会随阶段推进改名和拆分，绑在
// 一起意味着一次后端重构让前端跟着坏掉。显式映射多写几十行，换来两边各自演进。
//
// 这和 store 里"表结构与领域类型分开定义"是同一个道理，只是边界换成了网络。

// listSessionsResponse 是会话列表的响应体。
//
// 用一个对象包住数组而不是直接返回数组：将来要加分页游标或总数时，加字段是
// 兼容的，而把数组换成对象不是。
type listSessionsResponse struct {
	Sessions []sessionSummary `json:"sessions"`
}

// sessionSummary 是列表里的一项。
type sessionSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	MessageCount int       `json:"message_count"`
	UpdatedAt    time.Time `json:"updated_at"`
	Workspace    string    `json:"workspace"`
	// Model 是该会话使用的模型；空串表示沿用启动默认模型。
	Model string `json:"model"`
	// Archived 表示这个会话被归档了：列表默认不显示它。
	Archived bool `json:"archived"`
	// CustomTitle 表示标题是用户自己起的，不是从首条消息派生的。
	CustomTitle bool `json:"custom_title"`
}

// sessionSnapshot 是一个会话的完整快照。
type sessionSnapshot struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastSequence 是这个会话当前的最大事件序号。
	//
	// 前端拿快照渲染完整历史，再从这个序号往后订阅实时事件，中间不重不漏。
	// 没有它的话，两步之间产生的事件要么漏掉要么重复。
	LastSequence int64         `json:"last_sequence"`
	Messages     []messageView `json:"messages"`
	// Memory 是这个会话的压缩现状。
	//
	// 它必须进快照，否则刷新页面之后界面就不知道"这个会话已经压缩过了"——
	// 压缩事件是历史事件，前端只从 LastSequence 之后订阅，不会重放到它们。
	Memory memoryView `json:"memory"`
	// Usage 是这个会话**下一次请求**会占多少上下文。
	//
	// 它必须进快照，理由和 Memory 一样：面板刚打开时还没有下一次请求，事件流也
	// 只从 LastSequence 之后订阅，看不到历史上的 usage 事件。不带的话刷新页面
	// 之后仪表盘就是空的，直到用户再发一条消息。
	Usage domain.ContextUsagePayload `json:"usage"`
	// Model 是当前生效的模型名。
	Model string `json:"model"`
	// ModelInfo 是模型的窗口信息，避免前端再从模型列表里反查。
	ModelInfo modelInfoView `json:"model_info"`
}

// modelInfoView 是模型窗口信息的线上形态。
type modelInfoView struct {
	Name                   string `json:"name"`
	Display                string `json:"display_name"`
	ContextWindow          int    `json:"context_window"`
	EffectiveContextWindow int    `json:"effective_context_window"`
	CompactionTrigger      int    `json:"compaction_trigger"`
	WindowSource           string `json:"window_source"`
	MeasuredAt             string `json:"measured_at"`
}

// memoryView 是会话记忆的线上形态。
//
// 只给界面要显示的部分：当前生效的摘要前沿、原文游标、节点总数。整棵树不给——
// 它可能有上百个节点，而界面上需要的只是"哪几段被折叠了、折叠到第几条为止"。
// 要看某个节点的详情，走 conversation_history 那条路（模型用的也是它）。
type memoryView struct {
	// RawCompactionCursor 是仍以原文进入上下文的第一条消息序号，从 1 开始。
	//
	// 领域里它是右开区间的下标（[0, cursor) 已被摘要覆盖），这里 +1 换成
	// 给人看的序号："第 N 条之前的对话已经折叠"。cursor 为 0 时它是 1，
	// 意思是"一条都没折叠"，读起来仍然对。
	RawCompactionCursor int `json:"raw_compaction_cursor"`
	// ActiveBatches 是当前真正进入上下文的那一层摘要，按时间顺序。
	ActiveBatches []memoryBatchView `json:"active_batches"`
	// TotalBatches 是仓库里的节点总数，含已被合并掉的那些。
	//
	// 它和 len(ActiveBatches) 的差值就是"被合并进上层的历史节点数"，
	// 能让人一眼看出这个会话压缩了多少轮。
	TotalBatches int `json:"total_batches"`
}

// memoryBatchView 是一个摘要节点的线上形态。
//
// 字段与事件里的 domain.MemoryBatchSummary 刻意保持一致：前端用同一段代码渲染
// "快照里的前沿"和"压缩事件里新生成的节点"，两处形状不同只会逼出两份渲染逻辑。
type memoryBatchView struct {
	ID    string `json:"id"`
	Level int    `json:"level"`
	Title string `json:"title"`
	// StartMessage 与 EndMessage 是覆盖的消息序号，从 1 开始、闭区间。
	StartMessage int `json:"start_message"`
	EndMessage   int `json:"end_message"`
}

// newMemoryView 把会话记忆转换成线上形态。
func newMemoryView(memory domain.ConversationMemory) memoryView {
	view := memoryView{
		RawCompactionCursor: memory.RawCompactionCursor + 1,
		// 显式建空切片：nil 切片会序列化成 null，前端就得在每个用到它的地方
		// 先判一次空。给个 [] 让消费端可以直接 map。
		ActiveBatches: make([]memoryBatchView, 0, len(memory.ActiveBatchIDs)),
		TotalBatches:  len(memory.Batches),
	}
	for _, batch := range memory.ActiveBatches() {
		summary := domain.NewMemoryBatchSummary(batch)
		view.ActiveBatches = append(view.ActiveBatches, memoryBatchView{
			ID:           summary.ID,
			Level:        summary.Level,
			Title:        summary.Title,
			StartMessage: summary.StartMessage,
			EndMessage:   summary.EndMessage,
		})
	}
	return view
}

// messageView 是一条消息的线上形态。
type messageView struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images 是 user 消息附带的图片列表，按发送顺序。
	Images []imageView `json:"images,omitempty"`
	// ToolCalls 只在 assistant 消息上出现。
	ToolCalls []toolCallView `json:"tool_calls,omitempty"`
	// ToolCallID 只在 tool 消息上出现。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// TurnID 让前端把一轮里的东西归成一组。
	TurnID string `json:"turn_id"`
}

// toolCallView 是一次工具调用的线上形态。
type toolCallView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments 是参数原文，作为 JSON 值直接内嵌而不是字符串。
	//
	// 与存储层的选择相反：那里存字符串是为了逐字节保留（prompt 缓存按前缀哈希
	// 命中）；这里内嵌是为了前端能直接读字段，不必先 JSON.parse 一层字符串。
	// 展示不回传给供应商，因此字节层面的变化无关紧要。
	Arguments json.RawMessage `json:"arguments"`
}

// newMessageView 把领域消息转换成线上形态。
func newMessageView(message domain.Message) messageView {
	view := messageView{
		Role:       string(message.Role),
		Content:    message.Content,
		ToolCallID: message.ToolCallID,
		TurnID:     string(message.TurnID),
	}
	for _, image := range message.Images {
		view.Images = append(view.Images, imageView{
			ID:        image.ID,
			MediaType: image.MediaType,
			Width:     image.Width,
			Height:    image.Height,
		})
	}
	for _, call := range message.ToolCalls {
		view.ToolCalls = append(view.ToolCalls, toolCallView{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}
	return view
}

// imageView 是一张消息图片的线上形态。
type imageView struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// memoryTree 是完整摘要树的线上形态，`GET …/memory` 的响应体。
//
// 与快照里的 memoryView 的区别只有两点，但都很关键：**节点是全部的**（含已被合并
// 进上层、不再直接生效的那些），而且**带摘要正文**。面板靠它逐层展开，看到的和
// 模型用 conversation_history 看到的是同一棵树。
type memoryTree struct {
	// RawCompactionCursor 是仍以原文进入上下文的第一条消息序号，从 1 开始。
	RawCompactionCursor int `json:"raw_compaction_cursor"`
	// ActiveBatchIDs 是当前生效的那一层，按覆盖时间排序。
	//
	// 只给 ID 不重复给节点：节点都在 Batches 里，面板按 ID 查即可。重复给一份
	// 就会出现"两处对同一个节点的描述可能不一致"。
	ActiveBatchIDs []string `json:"active_batch_ids"`
	// Batches 是全部节点，按创建顺序（父节点必然晚于它的子节点）。
	Batches []memoryNodeView `json:"batches"`
}

// memoryNodeView 是摘要树里的一个节点。
type memoryNodeView struct {
	ID    string `json:"id"`
	Level int    `json:"level"`
	Title string `json:"title"`
	// Content 是**当前生效**的正文：有人工修订就是修订版，没有就是模型原文。
	// 快照里的形态不带它，这里带——展开树就是为了看它。
	Content string `json:"content"`
	// OriginalContent 是模型当初生成的那一版。只在被修订过时给出。
	//
	// 两份都给，界面才能让人对照"我改了什么"。后端本来就两份都留着
	// （见 domain.MemoryBatch.EditedContent），不给出来才是浪费。
	OriginalContent string `json:"original_content,omitempty"`
	// Edited 表示这个节点被人工修订过。
	//
	// 它等价于 `original_content != ""`，但显式给出来：让消费端按一个布尔值分支，
	// 而不是自己推导一条规则——推导规则散到消费端，两处迟早不一致。
	Edited bool `json:"edited"`
	// StartMessage 与 EndMessage 是覆盖的消息序号，从 1 开始、闭区间。
	//
	// 面板拿它到快照的 messages 数组里取原文：下标区间是 [StartMessage-1, EndMessage)。
	// 因此不需要"读取节点覆盖的原始消息"这样一个额外端点。
	StartMessage int `json:"start_message"`
	EndMessage   int `json:"end_message"`
	// SourceBatchIDs 是直接子节点，空表示这是叶子。
	SourceBatchIDs []string `json:"source_batch_ids"`
}

// newMemoryTree 把会话记忆转换成线上形态。
func newMemoryTree(memory domain.ConversationMemory) memoryTree {
	tree := memoryTree{
		RawCompactionCursor: memory.RawCompactionCursor + 1,
		// 显式建空切片：nil 会序列化成 null，前端就得在每个用到它的地方先判空。
		ActiveBatchIDs: make([]string, 0, len(memory.ActiveBatchIDs)),
		Batches:        make([]memoryNodeView, 0, len(memory.Batches)),
	}
	for _, id := range memory.ActiveBatchIDs {
		tree.ActiveBatchIDs = append(tree.ActiveBatchIDs, string(id))
	}
	for _, batch := range memory.Batches {
		summary := domain.NewMemoryBatchSummary(batch)
		children := make([]string, 0, len(batch.SourceBatchIDs))
		for _, child := range batch.SourceBatchIDs {
			children = append(children, string(child))
		}
		node := memoryNodeView{
			ID:             summary.ID,
			Level:          summary.Level,
			Title:          summary.Title,
			Content:        batch.EffectiveContent(),
			Edited:         batch.Edited(),
			StartMessage:   summary.StartMessage,
			EndMessage:     summary.EndMessage,
			SourceBatchIDs: children,
		}
		if batch.Edited() {
			node.OriginalContent = batch.Content
		}
		tree.Batches = append(tree.Batches, node)
	}
	return tree
}

// eventView 是一个运行事件的线上形态。
type eventView struct {
	// Sequence 对 durable 事件是它的序号，对 transient 事件是 0。
	Sequence int64  `json:"sequence"`
	TurnID   string `json:"turn_id"`
	Type     string `json:"type"`
	// Payload 原样透传：每种事件的内容由 domain 定义，这一层不解释它。
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

// newEventView 把领域事件转换成线上形态。
func newEventView(event domain.RunEvent) eventView {
	return eventView{
		Sequence: event.Sequence,
		TurnID:   string(event.TurnID),
		Type:     string(event.Type),
		Payload:  event.Payload,
		At:       event.At,
	}
}

// errorResponse 是统一的错误响应体。
type errorResponse struct {
	Error errorBody `json:"error"`
}

// errorBody 是错误的内容。
//
// Code 是给程序判断的稳定标识，Message 是给人看的说明。前端应当按 Code 分支，
// 而不是去匹配 Message 的文案——后者会随时被改写。
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON 写出一个 JSON 响应。
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)

	encoder := json.NewEncoder(writer)
	// 与 domain 里同样的理由：命令和输出里 &、<、> 极常见，转义之后前端虽然
	// 解析无碍，但用浏览器开发者工具看响应体时满屏 &，排查起来很痛苦。
	encoder.SetEscapeHTML(false)
	// 写到一半才出错（比如客户端断开）已经来不及改状态码了，记下来就是全部能做的。
	_ = encoder.Encode(body)
}

// writeError 写出一个错误响应。
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

// defaultWorkspace 返回新建会话时的默认工作目录。
//
// 用服务进程的当前目录。取不到时退回根目录而不是报错——工作目录不对最多让命令
// 找不到文件，而让整个新建请求失败更糟。
func defaultWorkspace() string {
	if directory, err := os.Getwd(); err == nil {
		return directory
	}
	return "/"
}
