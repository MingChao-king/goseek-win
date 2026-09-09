package store

import (
	"encoding/json"
	"fmt"

	"goseek/internal/domain"
)

// 本文件负责数据库行与领域类型之间的映射。
//
// 为什么不直接把领域类型塞进 SQL：领域类型会随着阶段推进改名、拆分、增删字段，
// 而表结构一旦有了用户数据就只能靠迁移演进。显式映射多写几十行，换来的是两边
// 可以各自演进，一次重构不会让已有数据读不出来。

// toolCallRow 是 messages.tool_calls 列里保存的一次工具调用。
//
// 一条消息的工具调用是一个有序列表，而且没有任何地方需要按调用 ID 做 SQL 查询
// （悬空调用是在内存里扫出来的），因此存成一列 JSON 文本而不是再拆一张表：
// 拆表只会多出一次 join 和一层映射。
type toolCallRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments 是参数原文，存成字符串而不是内嵌 JSON。
	//
	// 这些字节会原样回写进下一次请求的 assistant 消息，而供应商的 prompt 缓存
	// 按前缀哈希命中——重新格式化就打不中缓存，等于每轮都为全部历史重新付费。
	// 存成字符串正好也是供应商协议本来的形态。
	Arguments string `json:"arguments"`
}

// encodeToolCalls 把一条消息上的工具调用编码成待写入的列值。
//
// 没有调用时写空串而不是 "[]"：空串一眼能看出"这条消息不涉及工具"，
// 用 sqlite3 翻表时省事。
func encodeToolCalls(calls []domain.ToolCall) (string, error) {
	if len(calls) == 0 {
		return "", nil
	}

	rows := make([]toolCallRow, 0, len(calls))
	for _, call := range calls {
		rows = append(rows, toolCallRow{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: string(call.Arguments),
		})
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("编码工具调用失败: %w", err)
	}
	return string(encoded), nil
}

// decodeToolCalls 还原一条消息上的工具调用。
func decodeToolCalls(encoded string) ([]domain.ToolCall, error) {
	if encoded == "" {
		return nil, nil
	}

	var rows []toolCallRow
	if err := json.Unmarshal([]byte(encoded), &rows); err != nil {
		return nil, fmt.Errorf("工具调用不是合法 JSON: %w", err)
	}
	calls := make([]domain.ToolCall, 0, len(rows))
	for _, row := range rows {
		calls = append(calls, domain.ToolCall{
			ID:        row.ID,
			Name:      row.Name,
			Arguments: json.RawMessage(row.Arguments),
		})
	}
	return calls, nil
}

// toMessage 把一行 messages 还原成领域消息。
//
// 角色在写入时有 CHECK 约束挡着，读取时仍然要解析：约束防止本程序写进垃圾，
// 解析防止相信别人（比如手工执行的 sqlite3）写进来的垃圾。
func toMessage(role, content, encodedCalls, toolCallID, turnID string) (domain.Message, error) {
	parsedRole, err := parseRole(role)
	if err != nil {
		return domain.Message{}, err
	}
	calls, err := decodeToolCalls(encodedCalls)
	if err != nil {
		return domain.Message{}, err
	}
	return domain.Message{
		Role:       parsedRole,
		Content:    content,
		ToolCalls:  calls,
		ToolCallID: toolCallID,
		TurnID:     domain.TurnID(turnID),
	}, nil
}

// parseRole 把数据库里的角色字符串还原成领域角色。
func parseRole(text string) (domain.Role, error) {
	switch role := domain.Role(text); role {
	case domain.RoleUser, domain.RoleAssistant, domain.RoleTool:
		return role, nil
	default:
		return "", fmt.Errorf("无法识别的角色 %q", text)
	}
}
