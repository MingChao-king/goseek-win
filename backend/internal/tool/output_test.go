package tool

import (
	"strings"
	"testing"
)

// writeString 是测试里向缓冲写入文本的便捷函数。
func writeString(t *testing.T, output *commandOutput, text string) {
	t.Helper()
	written, err := output.Write([]byte(text))
	if err != nil {
		t.Fatalf("Write 返回错误: %v", err)
	}
	if written != len(text) {
		t.Fatalf("Write 报告写入 %d 字节; want %d", written, len(text))
	}
}

func TestOutputKeepsContentIntact(t *testing.T) {
	output := newCommandOutput(nil)
	writeString(t, output, "第一行\n第二行\n")

	if got := output.Result(); got != "第一行\n第二行\n" {
		t.Errorf("内容 = %q; want 原文", got)
	}
}

// **不截断**：无论多大，一个字节都不丢。
//
// 这条替代了上一版的一组"超限时保留头尾、中间标注省略多少字节"的测试。那个设计
// 被推翻了：截断损害模型的判断，而且方式很隐蔽——模型拿到一段读起来连续、实际
// 中间被挖掉的文本，于是开始从可见的元数据里推断，推断出来的东西和事实长得一样。
func TestOutputIsNeverTruncated(t *testing.T) {
	output := newCommandOutput(nil)
	// 远超旧上限（10000 字节）的量。
	huge := strings.Repeat("这一行有一些内容，用来把输出撑得很大。\n", 20000)
	writeString(t, output, huge)

	got := output.Result()
	if got != huge {
		t.Errorf("输出被改动了：写入 %d 字节，读回 %d 字节", len(huge), len(got))
	}
	// 旧实现会在中间插一条省略标记，现在绝不该出现。
	if strings.Contains(got, "省略") {
		t.Error("输出里出现了省略标记，说明还有截断逻辑残留")
	}
}

// 多段写入按顺序拼接，不因分片而错位。
func TestOutputConcatenatesWritesInOrder(t *testing.T) {
	output := newCommandOutput(nil)
	for _, part := range []string{"甲", "乙", "丙", "丁"} {
		writeString(t, output, part)
	}
	if got := output.Result(); got != "甲乙丙丁" {
		t.Errorf("内容 = %q; want 甲乙丙丁", got)
	}
}

// 多字节字符跨分片写入时不能被拆坏。
//
// os/exec 的拷贝按固定大小的缓冲切片，一个三字节的汉字完全可能横跨两次 Write。
// 上一版按字节做头尾截断，因此要专门去掉半个字符；现在不截断，只要求拼接无损。
func TestOutputHandlesRunesSplitAcrossWrites(t *testing.T) {
	output := newCommandOutput(nil)
	raw := []byte("汉字")
	// 从第二个字节切开，制造一次"半个汉字"的写入。
	writeString2(t, output, raw[:2])
	writeString2(t, output, raw[2:])

	if got := output.Result(); got != "汉字" {
		t.Errorf("跨分片的多字节字符被拆坏了: %q", got)
	}
}

// writeString2 写入原始字节，用于构造非法分片。
func writeString2(t *testing.T, output *commandOutput, raw []byte) {
	t.Helper()
	if _, err := output.Write(raw); err != nil {
		t.Fatalf("Write 返回错误: %v", err)
	}
}

// onChunk 拿到的是完整的实时流，而且和最终内容完全一致。
//
// 上一版这两者会分叉（展示看到完整流、模型看到有界内容），现在**必须相同**——
// 这正是取消截断之后的一条新不变量。
func TestOutputChunksMatchTheFinalContent(t *testing.T) {
	var streamed strings.Builder
	output := newCommandOutput(func(chunk string) { streamed.WriteString(chunk) })

	for _, part := range []string{"第一段\n", strings.Repeat("很长的内容", 5000), "\n最后一段"} {
		writeString(t, output, part)
	}

	if streamed.String() != output.Result() {
		t.Errorf("实时流与最终内容不一致：流 %d 字节，最终 %d 字节",
			len(streamed.String()), len(output.Result()))
	}
}

// onChunk 为 nil 时不崩——终端和 HTTP 之外的调用方可以不关心实时流。
func TestOutputWithoutCallbackDoesNotPanic(t *testing.T) {
	output := newCommandOutput(nil)
	writeString(t, output, "内容")
	if output.Result() != "内容" {
		t.Error("没有回调时内容不对")
	}
}
