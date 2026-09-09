package domain

import (
	"errors"
	"fmt"
	"iter"
	"strings"
)

// 这里放几个只在 domain 内部用的小工具，抽出来是为了让上面的领域逻辑读起来
// 不被字符串处理打断。

// splitLines 按行迭代一段文本。
//
// 用迭代器而不是返回切片：调用方通常只要第一个非空行，没必要为此把整段摘要
// 切成一个数组。
func splitLines(text string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, line := range strings.Split(text, "\n") {
			if !yield(line) {
				return
			}
		}
	}
}

// trimSpace 去掉首尾空白。
func trimSpace(text string) string {
	return strings.TrimSpace(text)
}

// invariantError 构造一个不变量被破坏的错误。
//
// 单独一个构造函数是为了让这类错误在日志里一眼可辨：它们不是外部输入的问题，
// 而是程序自己算错了，出现一次就说明有 bug。
func invariantError(format string, arguments ...any) error {
	return errors.New("会话记忆的不变量被破坏: " + fmt.Sprintf(format, arguments...))
}
