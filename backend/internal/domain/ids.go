package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// 应用自有的 ID 都是"固定前缀 + 32 位小写十六进制"。前缀让 ID 在日志、命令行和
// 数据库里一眼能看出是什么东西，也让不同种类的 ID 不可能被误认；随机部分用
// crypto/rand 生成的 128 位，足以避免本机范围内的碰撞。
//
// 之所以不用自增整数或时间戳：SessionID 会成为文件名的一部分，可预测的取值在多个
// 进程同时创建会话时可能撞车；而 TurnID 会出现在事件里被前端引用，连续可猜的编号
// 也没有额外好处。
const (
	// sessionIDPrefix 标识一个会话。
	sessionIDPrefix = "ses_"
	// turnIDPrefix 标识一轮交互。
	turnIDPrefix = "trn_"
	// randomIDBytes 是随机部分的字节数，编码成 32 个十六进制字符。
	randomIDBytes = 16
)

// SessionID 是一个会话的应用 ID，形如 ses_ 加 32 个小写十六进制字符。
//
// 定义成独立类型而不是裸 string：它要出现在加载、保存等签名里，和 workspace
// 路径这类字符串参数混在一起容易传错。
type SessionID string

// TurnID 是一轮交互的应用 ID，形如 trn_ 加 32 个小写十六进制字符。
//
// 一轮从一条用户消息开始，到模型给出不含工具调用的最终回复（或本轮失败）为止。
// 这中间产生的每条消息和每个事件都带着同一个 TurnID，前端据此把它们归成一组。
type TurnID string

// NewSessionID 生成一个新的会话 ID。
func NewSessionID() (SessionID, error) {
	text, err := newRandomID(sessionIDPrefix)
	return SessionID(text), err
}

// NewTurnID 生成一个新的轮次 ID。
func NewTurnID() (TurnID, error) {
	text, err := newRandomID(turnIDPrefix)
	return TurnID(text), err
}

// Validate 校验会话 ID 的格式。
//
// 这是本项目唯一必须做的领域值校验，因为**会话 ID 会被拼进文件路径**（锁文件的
// 文件名）：形如 "ses_../../etc/passwd" 的输入必须在拼路径之前就被拒绝。十六进制
// 字符集本身不含路径分隔符和点，因此通过校验的 ID 一定是一个安全的文件名。
//
// 调用者是命令行参数解析和 Store.Load，两者拿到的都是外部输入。
//
// TurnID 没有对应的校验方法，因为它到不了任何这样的边界：它只作为一列存进数据库、
// 再随事件发给前端，取值异常最多让前端分组不对，不会越权访问任何东西。按"没有
// 调用者就不写"的规则，那个方法现在不存在。
func (id SessionID) Validate() error {
	return validateRandomID(string(id), sessionIDPrefix)
}

// Short 返回用于展示的缩写形式，例如 ses_1f3c…。
//
// 完整 ID 有 36 个字符，在启动横幅和会话列表里过长；缩写只用于展示，
// 恢复会话仍然需要完整 ID。
func (id SessionID) Short() string {
	return shortenID(string(id), sessionIDPrefix)
}

// newRandomID 生成一个带给定前缀的随机 ID。
func newRandomID(prefix string) (string, error) {
	random := make([]byte, randomIDBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("生成 ID 失败: %w", err)
	}
	return prefix + hex.EncodeToString(random), nil
}

// validateRandomID 校验 ID 由给定前缀加 32 个小写十六进制字符组成。
func validateRandomID(text, prefix string) error {
	if !strings.HasPrefix(text, prefix) {
		return fmt.Errorf("ID %q 缺少 %q 前缀", text, prefix)
	}
	random := strings.TrimPrefix(text, prefix)
	if len(random) != hex.EncodedLen(randomIDBytes) {
		return fmt.Errorf("ID %q 的随机部分应为 %d 个字符，实际 %d 个",
			text, hex.EncodedLen(randomIDBytes), len(random))
	}
	for _, character := range random {
		isDigit := character >= '0' && character <= '9'
		isLowerHex := character >= 'a' && character <= 'f'
		if !isDigit && !isLowerHex {
			return fmt.Errorf("ID %q 含有非小写十六进制字符 %q", text, character)
		}
	}
	return nil
}

// shortenID 保留前缀和随机部分的前四位，用于展示。
func shortenID(text, prefix string) string {
	keep := len(prefix) + 4
	if len(text) <= keep {
		return text
	}
	return text[:keep] + "…"
}

// memoryBatchIDPrefix 标识一个摘要节点。
const memoryBatchIDPrefix = "mem_"
