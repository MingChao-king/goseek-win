package domain

import (
	"bytes"
	"encoding/json"
)

// encodeJSON 序列化一个值，并且**不做 HTML 转义**。
//
// encoding/json 的默认行为会把 <、> 和 & 写成 < 这类转义序列，为的是让结果
// 能安全地内嵌进 HTML。本项目的 JSON 有两个去处，两个都不需要这层转义，反而都
// 被它伤害：
//
//   - 工具观察（ToolResult.EncodeContent）会作为 tool 消息的正文交给模型阅读。
//     shell 命令里 &&、>、< 极其常见，让模型读到 "ls && pwd" 而不是
//     "ls && pwd" 是纯粹的噪声，还可能让它误判命令的真实内容。
//   - 事件 payload 会落进数据库并推给前端。前端 JSON.parse 之后拿到的内容是对的，
//     但用 sqlite3 直接翻事件表时满屏转义序列，可读性正是选文本存储的理由之一。
//
// 转义与否不改变解析结果，因此这个选择不影响任何往返正确性。
//
// json.Encoder 会在结果末尾补一个换行，这里去掉——调用方要的是一个 JSON 值，
// 不是一行输出。
func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
