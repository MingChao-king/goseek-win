package contextmgr

import (
	"encoding/json"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// 上下文视图必须是**确定性**的：同样的会话状态，构建多少次都得到逐字节相同的结果。
//
// # 为什么这条值得单独测
//
// 供应商的上下文缓存按**从第 0 个 token 起的完整前缀**匹配（DeepSeek 官方文档：
// "Only requests with identical prefixes will be considered duplicates. Partial
// matches in the middle of the input will not trigger a cache hit."）。命中
// $0.014/M、未命中 $0.14/M，十倍；对延迟的影响更大——128K 输入的首 token 从 13 秒
// 降到 500 毫秒。
//
// 也就是说：**只要有人往请求前缀里塞进一个会变的东西，缓存就整体失效**。
// 时间戳、计数器、随机序、map 迭代顺序——任何一个都够。
//
// 而这种回归**没有任何症状**：功能全对，测试全绿，只是变慢、变贵。它不会在任何
// 现有测试里露头，只会在账单和首字延迟上慢慢显现，而那时已经没人记得是哪次改动
// 引入的。
//
// 这条测试就是为拦它而写的。它不检查缓存命中率（那要真实供应商），它检查**造成
// 命中率崩塌的唯一本地原因**：视图不确定。
func TestContextViewIsByteIdenticalAcrossBuilds(t *testing.T) {
	session := buildSession(t, 6)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 4,
		Batches: []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 0, 0, 2),
			batchAt("mem_0000000000000000000000000000000b", 0, 2, 4),
		},
		ActiveBatchIDs: []domain.MemoryBatchID{
			"mem_0000000000000000000000000000000a",
			"mem_0000000000000000000000000000000b",
		},
	}
	tools := []domain.ToolSpec{
		{Name: "bash", Description: "运行命令", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "conversation_history", Description: "回查", Parameters: json.RawMessage(`{"type":"object"}`)},
	}

	first := serializeRequest(t, session, tools)
	// 多построить几次：非确定性来源（map 迭代、时钟）往往不是每次都暴露。
	for round := 2; round <= 20; round++ {
		if got := serializeRequest(t, session, tools); got != first {
			t.Fatalf("第 %d 次构建的视图与第一次不同——请求前缀不稳定，供应商缓存会整体失效。\n"+
				"第一次 %d 字节，这一次 %d 字节\n首个差异位置: %d",
				round, len(first), len(got), firstDifference(first, got))
		}
	}
}

// 追加一条新消息之后，**旧的前缀必须逐字节不变**。
//
// 这是上一条的加强版，也是缓存真正依赖的性质：对话增长时，新视图应当是旧视图的
// 一个**前缀扩展**，而不是一份重新排布的内容。否则每追加一条消息，之前的全部
// 内容都要重新以未命中的价格读一遍。
func TestAppendingAMessageOnlyExtendsThePrefix(t *testing.T) {
	session := buildSession(t, 4)
	tools := []domain.ToolSpec{{Name: "bash", Description: "运行命令"}}

	before := serializeRequest(t, session, tools)

	session.Append(domain.Message{
		Role: domain.RoleUser, TurnID: "trn_00000000000000000000000000000099",
		Content: "再问一句",
	})
	after := serializeRequest(t, session, tools)

	if !strings.HasPrefix(after, before) {
		t.Errorf("追加一条消息之后，旧内容不再是新视图的前缀——每次追加都会让"+
			"整段上下文重新按未命中计费。\n首个差异位置: %d（旧视图共 %d 字节）",
			firstDifference(before, after), len(before))
	}
}

// 压缩**追加一个叶子**时，system 与既有摘要仍然是前缀。
//
// 这条量的是一个真实的取舍：压缩必然打断缓存，但打断多少取决于结构。
// 追加叶子只让新叶子之后的部分失效，塌缩前沿则会从 system 之后全断——
// 这给"不做预防性塌缩"多了一条独立于成本的理由。
func TestAppendingALeafKeepsTheEarlierSummariesInThePrefix(t *testing.T) {
	session := buildSession(t, 8)
	tools := []domain.ToolSpec{{Name: "bash", Description: "运行命令"}}

	before := domain.ConversationMemory{
		RawCompactionCursor: 4,
		Batches:             []domain.MemoryBatch{batchAt("mem_0000000000000000000000000000000a", 0, 0, 4)},
		ActiveBatchIDs:      []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}
	session.Memory = before
	prefixBefore := serializeHead(t, session, tools)

	// 再压一次：追加一个覆盖 [4,8) 的叶子。
	session.Memory = appendLeaf(before, batchAt("mem_0000000000000000000000000000000b", 0, 4, 8))
	prefixAfter := serializeHead(t, session, tools)

	if !strings.HasPrefix(prefixAfter, prefixBefore) {
		t.Errorf("追加叶子之后 system + 既有摘要不再是前缀，缓存会整体失效而不是部分失效\n"+
			"首个差异位置: %d", firstDifference(prefixBefore, prefixAfter))
	}
}

// serializeRequest 把视图序列化成一个可比较的字符串。
//
// 比较序列化结果而不是逐字段比较：真正进入缓存匹配的就是最终字节，
// 而逐字段比较会漏掉字段顺序、空值省略这类只在序列化时才显形的差异。
//
// **逐条序列化再拼接，而不是把整个数组 Marshal 一次**：数组会带一个收尾的 `]`，
// 于是"旧视图是新视图的前缀"这个关系会被那一个字节破坏掉——而供应商的前缀匹配
// 看的是消息序列，不是我们这边的数组语法。第一版就栽在这里，测试报了一个位于
// 倒数第一字节的"差异"，实际上什么都没错。
func serializeRequest(t *testing.T, session *domain.Session, tools []domain.ToolSpec) string {
	t.Helper()
	view := BuildWith(session, session.Memory, tools, 128000, domain.DefaultSafetyFactor, nil)
	return serializeTools(t, tools) + serializeMessages(t, view.Messages)
}

// serializeHead 只序列化 system 与活跃摘要那几条，用于比较"前缀有没有被打断"。
func serializeHead(t *testing.T, session *domain.Session, tools []domain.ToolSpec) string {
	t.Helper()
	view := BuildWith(session, session.Memory, tools, 128000, domain.DefaultSafetyFactor, nil)
	return serializeTools(t, tools) +
		serializeMessages(t, view.Messages[:1+len(session.Memory.ActiveBatchIDs)])
}

// serializeMessages 把消息逐条编码再拼接，保持真正的前缀关系。
func serializeMessages(t *testing.T, messages []domain.ModelMessage) string {
	t.Helper()
	var out strings.Builder
	for _, message := range messages {
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatalf("序列化消息失败: %v", err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.String()
}

// serializeTools 编码工具定义——它们同样在缓存前缀里。
func serializeTools(t *testing.T, tools []domain.ToolSpec) string {
	t.Helper()
	var out strings.Builder
	for _, spec := range tools {
		encoded, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("序列化工具定义失败: %v", err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.String()
}

// firstDifference 返回两个字符串首个不同的位置，失败信息里指出来便于定位。
func firstDifference(left, right string) int {
	limit := min(len(left), len(right))
	for index := range limit {
		if left[index] != right[index] {
			return index
		}
	}
	if len(left) == len(right) {
		return -1
	}
	return limit
}

// 确保失败信息里的位置是可读的，顺便挡住 firstDifference 自己写错。
func TestFirstDifference(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"abc", "abc", -1},
		{"abc", "abd", 2},
		{"abc", "abcd", 3},
		{"", "a", 0},
	}
	for _, testCase := range cases {
		if got := firstDifference(testCase.left, testCase.right); got != testCase.want {
			t.Errorf("firstDifference(%q,%q) = %d; want %d",
				testCase.left, testCase.right, got, testCase.want)
		}
	}
}

// 视图里**绝不能出现两条连续的 assistant 消息**——实测确证的供应商协议约束。
//
// 摘要用 assistant 角色渲染。游标之后的第一条原文如果也是 assistant，思考模型就把它
// 当成"续写这条回复"，要求回传 reasoning_content，否则 400。用真实消息逐字重放的
// 四个形状：
//
//	[sys][摘要][assistant+tc][tool]        → 400
//	[sys][assistant+tc][tool]              → 正常
//	[sys][user][assistant+tc][tool]        → 正常
//	[sys][摘要][user][assistant+tc][tool]  → 正常
//
// 这个 bug 是真机跑出来的，而且**第一次修错了位置**：一开始在视图末尾补用户消息，
// 全塌后的第一次请求因此通过了，但下一次请求（末条是 tool）又炸——该补的是摘要与
// 原文之间那个接缝，不是末尾。
func TestViewNeverHasTwoConsecutiveAssistantMessages(t *testing.T) {
	// 构造终结性全塌之后、这一轮又跑了一条命令的状态：
	// 游标停在 3，而 history[3] 是 assistant——接缝正好落在 assistant 上。
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "跑一下这两条命令"},
		{Role: domain.RoleAssistant, TurnID: "t1",
			ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: "第一条的输出"},
		{Role: domain.RoleAssistant, TurnID: "t1",
			ToolCalls: []domain.ToolCall{{ID: "c2", Name: "bash"}}},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c2", Content: "第二条的输出"},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 3,
		Batches: []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 1, 0, 3),
		},
		ActiveBatchIDs: []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}

	view := buildMessages(session.Messages(), session.Memory, nil)

	for index := 1; index < len(view); index++ {
		if view[index-1].Role == domain.ModelRoleAssistant &&
			view[index].Role == domain.ModelRoleAssistant {
			t.Fatalf("第 %d、%d 条是连续的 assistant，这个请求会被供应商 400 拒绝。"+
				"角色序列: %s", index-1, index, rolesOfView(view))
		}
	}
	// 补的必须是当前轮的用户消息，而且落在摘要之后、原文之前。
	if rolesOfView(view) != "system,assistant,user,assistant,tool" {
		t.Errorf("角色序列 = %s; want system,assistant,user,assistant,tool", rolesOfView(view))
	}
	if view[2].Content != "跑一下这两条命令" {
		t.Errorf("补的不是当前轮的用户消息: %q", view[2].Content)
	}
}

// 全塌把整个历史吞掉时（游标到末尾），同样要补出用户消息。
func TestFullCollapseStillShowsTheUserMessage(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "跑一下"},
		{Role: domain.RoleAssistant, TurnID: "t1",
			ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: "输出"},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: len(messages),
		Batches: []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 1, 0, len(messages)),
		},
		ActiveBatchIDs: []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}

	view := buildMessages(session.Messages(), session.Memory, nil)

	if rolesOfView(view) != "system,assistant,user" {
		t.Errorf("角色序列 = %s; want system,assistant,user", rolesOfView(view))
	}
}

// 一轮里有多条用户消息时（运行中注入），全部补回去并保持顺序。
func TestAllUserMessagesOfTheTurnAreRestored(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "第一句"},
		{Role: domain.RoleAssistant, TurnID: "t1",
			ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: "输出"},
		{Role: domain.RoleUser, TurnID: "t1", Content: "补一句：跳过第 5 条"},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: len(messages),
		Batches: []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 1, 0, len(messages)),
		},
		ActiveBatchIDs: []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}

	view := buildMessages(session.Messages(), session.Memory, nil)

	var restored []string
	for _, message := range view {
		if message.Role == domain.ModelRoleUser {
			restored = append(restored, message.Content)
		}
	}
	if len(restored) != 2 || restored[0] != "第一句" || restored[1] != "补一句：跳过第 5 条" {
		t.Errorf("补回的用户消息 = %v; want [第一句, 补一句：跳过第 5 条]", restored)
	}
}

// 正常路径一个字都不补：游标落在轮次边界（用户消息）上，接缝天然是合法的。
func TestNothingIsBridgedOnTheNormalPath(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "第一轮"},
		{Role: domain.RoleAssistant, TurnID: "t1", Content: "答"},
		{Role: domain.RoleUser, TurnID: "t2", Content: "第二轮"},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)
	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 2, // 落在轮次边界上
		Batches: []domain.MemoryBatch{
			batchAt("mem_0000000000000000000000000000000a", 0, 0, 2),
		},
		ActiveBatchIDs: []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}

	view := buildMessages(session.Messages(), session.Memory, nil)

	if rolesOfView(view) != "system,assistant,user" {
		t.Errorf("角色序列 = %s; want system,assistant,user（不该补任何东西）", rolesOfView(view))
	}
}

// 没压缩过的完整对话不补——那是**快照**那条路径（面板要"下一次请求会占多少"），
// 不是真实请求，补东西会让它报的占用比实际多一条。
func TestUncompactedConversationIsNotBridged(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "问题"},
		{Role: domain.RoleAssistant, TurnID: "t1", Content: "回答"},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)

	view := buildMessages(session.Messages(), domain.ConversationMemory{}, nil)

	if rolesOfView(view) != "system,user,assistant" {
		t.Errorf("角色序列 = %s; want system,user,assistant", rolesOfView(view))
	}
}

// rolesOfView 返回视图的角色序列，失败信息里打出来便于定位。
func rolesOfView(view []domain.ModelMessage) string {
	roles := make([]string, 0, len(view))
	for _, message := range view {
		roles = append(roles, string(message.Role))
	}
	return strings.Join(roles, ",")
}
