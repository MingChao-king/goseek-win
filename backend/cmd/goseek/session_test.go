package main

import (
	"bufio"
	"errors"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
	"goseek/internal/store"
)

// listNow 是渲染列表时的"当前时间"，让相对时间可预期。
var listNow = time.Date(2026, 8, 19, 18, 0, 0, 0, time.UTC)

// twoSummaries 造两个会话摘要，第一个是最近活动的。
func twoSummaries() []store.Summary {
	return []store.Summary{
		{
			ID:           "ses_1111111111111111111111111111aaaa",
			Title:        "第一个会话",
			MessageCount: 12,
			UpdatedAt:    listNow.Add(-3 * time.Minute),
			Workspace:    "/tmp/first",
		},
		{
			ID:           "ses_2222222222222222222222222222bbbb",
			Title:        "第二个会话",
			MessageCount: 4,
			UpdatedAt:    listNow.Add(-30 * time.Hour),
			Workspace:    "/tmp/second",
		},
	}
}

// chooseWith 用给定输入跑一次选择。
func chooseWith(t *testing.T, input string) (domain.SessionID, string, error) {
	t.Helper()
	var output strings.Builder
	id, err := chooseSession(twoSummaries(), bufio.NewReader(strings.NewReader(input)), &output, listNow)
	return id, output.String(), err
}

func TestChooseSessionReturnsTheSelectedID(t *testing.T) {
	id, output, err := chooseWith(t, "2\n")
	if err != nil {
		t.Fatalf("chooseSession 返回错误: %v", err)
	}
	if id != "ses_2222222222222222222222222222bbbb" {
		t.Errorf("选中 %q; want 第二个会话", id)
	}

	// 列表要能让用户认出"哪个是我刚才在聊的"：标题、消息数、工作目录都得在。
	for _, want := range []string{"第一个会话", "第二个会话", "12", "3 分钟前", "/tmp/first"} {
		if !strings.Contains(output, want) {
			t.Errorf("列表里缺少 %q:\n%s", want, output)
		}
	}
}

// 回车是"我不选了"，要和出错区分开，让入口能安静退出。
func TestChooseSessionTreatsEmptyInputAsCancel(t *testing.T) {
	if _, _, err := chooseWith(t, "\n"); !errors.Is(err, errNoSelection) {
		t.Errorf("空输入的错误 = %v; want errNoSelection", err)
	}
}

func TestChooseSessionRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"不是数字", "第一个\n"},
		{"零", "0\n"},
		{"负数", "-1\n"},
		{"超出范围", "3\n"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			id, _, err := chooseWith(t, testCase.input)
			if err == nil {
				t.Fatalf("输入 %q 被接受，选中了 %q", testCase.input, id)
			}
			if errors.Is(err, errNoSelection) {
				t.Errorf("输入 %q 被当成了取消", testCase.input)
			}
		})
	}
}

// 一个会话都没有时要给出下一步该怎么做，而不是抛一个空列表。
func TestChooseSessionExplainsWhenThereAreNoSessions(t *testing.T) {
	var output strings.Builder
	_, err := chooseSession(nil, bufio.NewReader(strings.NewReader("")), &output, listNow)
	if err == nil {
		t.Fatal("空列表没有报错")
	}
	if !strings.Contains(err.Error(), "goseek") {
		t.Errorf("错误信息 = %q; want 告诉用户怎么开始一个会话", err.Error())
	}
}

func TestFormatRelativeTime(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"刚刚", listNow.Add(-30 * time.Second), "刚刚"},
		{"分钟", listNow.Add(-45 * time.Minute), "45 分钟前"},
		{"同一天", listNow.Add(-5 * time.Hour), "5 小时前"},
		{"昨天", time.Date(2026, 8, 18, 15, 3, 0, 0, time.UTC), "昨天 15:03"},
		{"同一年", time.Date(2026, 3, 2, 9, 12, 0, 0, time.UTC), "3月2日 09:12"},
		{"往年", time.Date(2025, 8, 17, 9, 12, 0, 0, time.UTC), "2025年8月17日"},
		{"零值", time.Time{}, "未知"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := formatRelativeTime(testCase.when, listNow); got != testCase.want {
				t.Errorf("formatRelativeTime = %q; want %q", got, testCase.want)
			}
		})
	}
}

// 跨过午夜但不足 24 小时，要说"昨天几点"而不是"22 小时前"——后者更难认。
func TestFormatRelativeTimeUsesCalendarDayNotElapsedHours(t *testing.T) {
	earlyMorning := time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC)
	yesterdayAfternoon := time.Date(2026, 8, 18, 15, 0, 0, 0, time.UTC)

	if got := formatRelativeTime(yesterdayAfternoon, earlyMorning); got != "昨天 15:00" {
		t.Errorf("formatRelativeTime = %q; want 昨天 15:00", got)
	}
}

func TestShortenPathReplacesHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := shortenPath(home + "/project-a"); got != "~/project-a" {
		t.Errorf("shortenPath = %q; want ~/project-a", got)
	}
	if got := shortenPath(home); got != "~" {
		t.Errorf("shortenPath = %q; want ~", got)
	}
	if got := shortenPath("/tmp/elsewhere"); got != "/tmp/elsewhere" {
		t.Errorf("shortenPath = %q; want 原样返回", got)
	}
	if got := shortenPath(""); got == "" {
		t.Error("空路径应当给出一个可读的占位说明")
	}
}

// 主目录的同名前缀不能被误当成主目录本身。
func TestShortenPathDoesNotMatchSiblingWithSharedPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sibling := home + "-backup/project"
	if got := shortenPath(sibling); got != sibling {
		t.Errorf("shortenPath = %q; want 原样返回 %q", got, sibling)
	}
}

func TestParseOptions(t *testing.T) {
	if _, err := parseOptions([]string{"--session", "--resume", "ses_x"}); err == nil {
		t.Error("--session 与 --resume 同时使用没有报错")
	}

	parsed, err := parseOptions([]string{"--session"})
	if err != nil {
		t.Fatalf("parseOptions 返回错误: %v", err)
	}
	if !parsed.listSessions || parsed.resumeID != "" {
		t.Errorf("解析 --session 得到 %+v", parsed)
	}

	parsed, err = parseOptions([]string{"--resume", "ses_abc"})
	if err != nil {
		t.Fatalf("parseOptions 返回错误: %v", err)
	}
	if parsed.listSessions || parsed.resumeID != "ses_abc" {
		t.Errorf("解析 --resume 得到 %+v", parsed)
	}

	parsed, err = parseOptions(nil)
	if err != nil {
		t.Fatalf("parseOptions 返回错误: %v", err)
	}
	if parsed.listSessions || parsed.resumeID != "" {
		t.Errorf("无参数时得到 %+v", parsed)
	}
}

// 汉字在终端里占两列，列宽必须按显示宽度算，否则会话列表会歪掉。
func TestDisplayWidthCountsWideCharactersAsTwoColumns(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"abc", 3},
		{"最后活动", 8},
		{"a中b", 4},
		{"", 0},
		{"（未知）", 8},
	}

	for _, testCase := range cases {
		if got := displayWidth(testCase.text); got != testCase.want {
			t.Errorf("displayWidth(%q) = %d; want %d", testCase.text, got, testCase.want)
		}
	}
}

func TestPadRightAlignsByDisplayWidth(t *testing.T) {
	if got := padRight("中文", 8); displayWidth(got) != 8 {
		t.Errorf("padRight(\"中文\", 8) 的显示宽度 = %d; want 8", displayWidth(got))
	}
	if got := padRight("abc", 5); got != "abc  " {
		t.Errorf("padRight = %q; want %q", got, "abc  ")
	}
	// 超宽时不截断：宁可错位，也不要把工作目录截掉一半让用户认错会话。
	if got := padRight("很长的中文路径", 4); got != "很长的中文路径" {
		t.Errorf("padRight 截断了内容: %q", got)
	}
}

// 各列真的对齐了：每一行在标题之前的部分显示宽度必须相同。
func TestSessionListColumnsLineUp(t *testing.T) {
	summaries := twoSummaries()
	summaries[0].Workspace = "/tmp/纯中文目录"
	summaries[0].Title = "中文标题"

	var output strings.Builder
	if _, err := chooseSession(summaries, bufio.NewReader(strings.NewReader("\n")), &output, listNow); !errors.Is(err, errNoSelection) {
		t.Fatalf("chooseSession 返回错误: %v", err)
	}

	var widths []int
	for _, line := range strings.Split(output.String(), "\n") {
		// 先匹配具体标题：表头的"会话"是"第二个会话"的子串，顺序反了会量错位置。
		title := ""
		switch {
		case strings.Contains(line, "中文标题"):
			title = "中文标题"
		case strings.Contains(line, "第二个会话"):
			title = "第二个会话"
		case strings.Contains(line, "最后活动"):
			title = "会话"
		default:
			continue
		}
		widths = append(widths, displayWidth(line[:strings.Index(line, title)]))
	}

	if len(widths) != 3 {
		t.Fatalf("解析出 %d 行; want 表头加两行\n%s", len(widths), output.String())
	}
	for _, width := range widths[1:] {
		if width != widths[0] {
			t.Errorf("各行标题列的起始位置不一致: %v\n%s", widths, output.String())
		}
	}
}
