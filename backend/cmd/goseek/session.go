package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"goseek/internal/domain"
	"goseek/internal/store"
)

// errNoSelection 表示用户放弃了这次选择。
//
// 它是一个哨兵错误而不是空 ID：调用方需要区分"用户按了回车"和"出错了"，
// 前者应该安静退出，后者要报告原因。
var errNoSelection = errors.New("没有选择会话")

// chooseSession 展示会话列表并让用户按序号选择一个。
//
// 为什么是"列出来选序号"而不是让用户敲会话 ID：ID 是 ses_ 加 32 位十六进制，
// 没人记得住，也没法从中看出哪个是要找的那个。列表把"最后活动、消息条数、工作
// 目录、第一句话"摆出来，选择依据是这些，序号只是一个临时的指代。
//
// now 由调用方传入而不是在这里取 time.Now()：相对时间的渲染要能被测试固定住，
// 否则"刚刚"和"1 分钟前"的边界会让测试偶发失败。
func chooseSession(summaries []store.Summary, input *bufio.Reader, output io.Writer, now time.Time) (domain.SessionID, error) {
	if len(summaries) == 0 {
		return "", errors.New("还没有任何会话，直接运行 goseek 开始一个")
	}

	// 列宽用 padRight 而不是 fmt 的 %-14s：Go 的宽度动词按 **rune 数**补齐，
	// 而汉字在终端里占两列。"工作目录"四个字算 4 个 rune、占 8 列，于是每一行
	// 的补齐量都不一样，整张表就歪了。padRight 按显示列宽补（见 width.go）。
	fmt.Fprintf(output, "\n  %s %s %s  %s %s\n",
		padRight("#", 3), padRight("最后活动", 14), padRight("消息", 6),
		padRight("工作目录", 26), "会话")
	for index, summary := range summaries {
		fmt.Fprintf(output, "  %s %s %s  %s %s\n",
			padRight(strconv.Itoa(index+1), 3),
			padRight(formatRelativeTime(summary.UpdatedAt, now), 14),
			padRight(strconv.Itoa(summary.MessageCount), 6),
			padRight(shortenPath(summary.Workspace), 26),
			summary.Title)
	}
	fmt.Fprintf(output, "\n选择序号（回车取消）: ")

	// EOF 不算错误：输入被重定向自文件或管道时（测试、脚本），最后一行后面
	// 没有换行符，ReadString 会连同内容一起返回 io.EOF。先取内容再判断。
	line, err := input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	choice := strings.TrimSpace(line)
	// 直接回车表示放弃。用哨兵错误而不是空 ID 返回，调用方才能把"用户改主意了"
	// 和"出错了"区分开：前者安静退出，后者要打印原因。
	if choice == "" {
		return "", errNoSelection
	}

	// 序号是外部输入，两种错法都要给出能看懂的说明：不是数字，或者越界。
	// 这里不做"猜他是不是想输 ID"之类的补救——把错误说清楚，让他再输一次。

	number, err := strconv.Atoi(choice)
	if err != nil {
		return "", fmt.Errorf("%q 不是一个序号", choice)
	}
	if number < 1 || number > len(summaries) {
		return "", fmt.Errorf("序号 %d 超出范围 1-%d", number, len(summaries))
	}
	return summaries[number-1].ID, nil
}

// formatRelativeTime 把时间点渲染成便于扫视的相对描述。
//
// 列表要回答的是"哪个是我刚才在聊的"，所以近处用相对时间，远处才回到日期。
// 零值时间意味着这个会话的文件读不出来，不能装作它有活动时间。
func formatRelativeTime(when, now time.Time) string {
	if when.IsZero() {
		return "未知"
	}

	elapsed := now.Sub(when)
	switch {
	case elapsed < time.Minute:
		return "刚刚"
	case elapsed < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(elapsed.Minutes()))
	case isSameDay(when, now):
		return fmt.Sprintf("%d 小时前", int(elapsed.Hours()))
	case isSameDay(when, now.AddDate(0, 0, -1)):
		return when.Format("昨天 15:04")
	case when.Year() == now.Year():
		return when.Format("1月2日 15:04")
	default:
		return when.Format("2006年1月2日")
	}
}

// isSameDay 判断两个时间是否落在同一个自然日。
//
// 不能用"相差不到 24 小时"代替：凌晨一点和前一天下午三点相差不到 24 小时，
// 但说成"22 小时前"不如说"昨天 15:00"好认。
func isSameDay(first, second time.Time) bool {
	firstYear, firstMonth, firstDay := first.Date()
	secondYear, secondMonth, secondDay := second.Date()
	return firstYear == secondYear && firstMonth == secondMonth && firstDay == secondDay
}

// shortenPath 把主目录换成 ~，让工作目录一栏能放得下。
//
// 只替换前缀，不做任何路径规范化：这一栏是给人看的，而人认得出 ~/code/foo
// 是哪里。真正用来执行命令的始终是会话里存的完整路径。
func shortenPath(path string) string {
	if path == "" {
		return "（未知）"
	}
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(path, home) {
		return path
	}
	relative := strings.TrimPrefix(path, home)
	if relative == "" {
		return "~"
	}
	// 剩下的部分必须以分隔符开头才是真的子目录。否则 /home/bob 会被
	// /home/bobby 的前缀匹配命中，显示成 "~by" ——一个不存在的路径。
	if !strings.HasPrefix(relative, string(filepath.Separator)) {
		return path
	}
	return "~" + relative
}
