package main

import "strings"

// 终端里的对齐不能用 fmt 的 %-14s 之类做：那是按 rune 数补空格，而一个汉字在
// 等宽终端里占两列。中文标题和中文路径一多，整张表就会歪掉。
//
// 这里只需要区分"占两列"和"占一列"，因此按 East Asian Wide/Fullwidth 的主要区段
// 判断，不引入完整的字符宽度表。控制字符不出现在这些列里（标题已经折叠过空白）。

// displayWidth 返回字符串在等宽终端里占用的列数。
func displayWidth(text string) int {
	width := 0
	for _, character := range text {
		width += runeWidth(character)
	}
	return width
}

// runeWidth 返回单个字符占用的列数。
func runeWidth(character rune) int {
	switch {
	case character >= 0x1100 && character <= 0x115F, // 韩文字母
		character >= 0x2E80 && character <= 0x303E,   // CJK 部首、标点
		character >= 0x3041 && character <= 0x33FF,   // 平假名、片假名、CJK 兼容
		character >= 0x3400 && character <= 0x4DBF,   // CJK 扩展 A
		character >= 0x4E00 && character <= 0x9FFF,   // CJK 统一表意文字
		character >= 0xA000 && character <= 0xA4CF,   // 彝文
		character >= 0xAC00 && character <= 0xD7A3,   // 韩文音节
		character >= 0xF900 && character <= 0xFAFF,   // CJK 兼容表意文字
		character >= 0xFE30 && character <= 0xFE6F,   // CJK 兼容形式
		character >= 0xFF00 && character <= 0xFF60,   // 全角字符
		character >= 0xFFE0 && character <= 0xFFE6,   // 全角符号
		character >= 0x1F300 && character <= 0x1FAFF: // 表情符号
		return 2
	default:
		return 1
	}
}

// padRight 把字符串补足到指定的显示列宽。
//
// 内容本身就超宽时不截断：会话列表里宁可让一行错位，也不要把工作目录截掉一半，
// 那会让用户认错会话。
func padRight(text string, columns int) string {
	if missing := columns - displayWidth(text); missing > 0 {
		return text + strings.Repeat(" ", missing)
	}
	return text
}
