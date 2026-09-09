package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"goseek/internal/domain"
)

// printerTurnID 是渲染测试里固定的轮次 ID。
const printerTurnID = domain.TurnID("trn_0123456789abcdef0123456789abcdef")

// render 把一串事件喂给渲染器，返回终端上会出现的内容。
func render(events ...domain.RunEvent) string {
	var buffer bytes.Buffer
	printer := newStepPrinter(&buffer)
	for _, event := range events {
		printer.Emit(event)
	}
	return buffer.String()
}

// event 造一个指定类型和 payload 的事件。
func event(eventType domain.EventType, payload any) domain.RunEvent {
	return domain.NewEvent(printerTurnID, eventType, payload, time.Now())
}

// 正文逐片到达时要拼在同一行上，而不是每片一行。
func TestTextDeltasAreStreamedOnOneLine(t *testing.T) {
	output := render(
		event(domain.EventAssistantDelta, domain.TextDeltaPayload{Text: "我是"}),
		event(domain.EventAssistantDelta, domain.TextDeltaPayload{Text: " GoSeek"}),
		event(domain.EventAssistantMessage, domain.AssistantMessagePayload{Content: "我是 GoSeek", Final: true}),
	)

	if !strings.Contains(output, "我是 GoSeek") {
		t.Errorf("流式文字没有拼起来: %q", output)
	}
	if strings.Count(strings.TrimRight(output, "\n"), "\n") != 0 {
		t.Errorf("流式文字被拆成了多行: %q", output)
	}
}

// 思考过程要能和正文一眼区分开——它对理解 Agent 在想什么有用，但不是回复。
func TestReasoningDeltasAreMarked(t *testing.T) {
	output := render(
		event(domain.EventAssistantReasoningDelta, domain.TextDeltaPayload{Text: "用户想看目录"}),
	)

	if !strings.Contains(output, "思考") {
		t.Errorf("思考内容没有被标记出来: %q", output)
	}
	if !strings.Contains(output, "用户想看目录") {
		t.Errorf("思考内容没有显示: %q", output)
	}
}

// 一段流式文字之后接别的步骤时，必须先换行，否则两者会挤在同一行。
func TestStreamingIsTerminatedBeforeTheNextStep(t *testing.T) {
	output := render(
		event(domain.EventAssistantDelta, domain.TextDeltaPayload{Text: "我先看看"}),
		event(domain.EventToolStarted, domain.ToolStartedPayload{
			Call:  domain.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
			Title: "查看目录",
		}),
	)

	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("输出 %d 行; want 3（流式文字、标题、参数）:\n%s", len(lines), output)
	}
	if !strings.Contains(lines[0], "我先看看") {
		t.Errorf("第一行 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "查看目录") {
		t.Errorf("第二行 = %q", lines[1])
	}
}

// 标题来自模型自述，参数原文才是事实，两者都要打印出来供对照。
func TestToolStartedPrintsTitleAndRawArguments(t *testing.T) {
	output := render(event(domain.EventToolStarted, domain.ToolStartedPayload{
		Call: domain.ToolCall{
			ID:        "call-1",
			Name:      "bash",
			Arguments: json.RawMessage("{\n  \"command\": \"ls -la\"\n}"),
		},
		Title: "查看当前目录",
	}))

	if !strings.Contains(output, "查看当前目录") {
		t.Errorf("输出缺少标题: %q", output)
	}
	if !strings.Contains(output, `{"command":"ls -la"}`) {
		t.Errorf("参数没有被压成一行: %q", output)
	}
}

// 工具输出逐片到达时按行打印，超出上限的部分只计数、不打印。
func TestToolOutputIsStreamedAndCappedWithACount(t *testing.T) {
	events := []domain.RunEvent{event(domain.EventToolStarted, domain.ToolStartedPayload{
		Call: domain.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{}`)}, Title: "跑一下",
	})}
	for index := range maxDisplayedOutputLines + 5 {
		events = append(events, event(domain.EventToolOutputDelta, domain.ToolOutputDeltaPayload{
			ToolCallID: "call-1",
			Chunk:      strings.Repeat("x", index%3+1) + "\n",
		}))
	}
	zero := 0
	events = append(events, event(domain.EventToolResolved, domain.ToolResolvedPayload{
		Result: domain.ToolResult{ToolCallID: "call-1", Name: "bash", Status: domain.ToolSuccess, ExitCode: &zero},
	}))

	output := render(events...)

	if !strings.Contains(output, "另有 5 行未显示") {
		t.Errorf("没有说明省略了多少行: %q", output)
	}
	if !strings.Contains(output, "完整内容") {
		t.Errorf("没有讲清模型收到的是完整内容: %q", output)
	}
	if !strings.Contains(output, "success，exit 0") {
		t.Errorf("没有显示状态和退出码: %q", output)
	}
}

// 被程序阻止的调用没有输出流，说明只在观察里，此时要把它打出来，
// 否则用户只看到一个光秃秃的 error。
func TestBlockedCallShowsItsExplanation(t *testing.T) {
	output := render(event(domain.EventToolResolved, domain.ToolResolvedPayload{
		Result: domain.ToolResult{
			ToolCallID: "call-1", Name: "python", Status: domain.ToolError,
			Content: "不存在名为 python 的工具，本次调用没有执行。",
		},
	}))

	if !strings.Contains(output, "不存在名为 python 的工具") {
		t.Errorf("没有显示阻止原因: %q", output)
	}
}

// 成功且有输出流时不重复打印观察正文——那些内容刚刚已经逐片显示过了。
func TestResolvedDoesNotReprintStreamedOutput(t *testing.T) {
	zero := 0
	output := render(event(domain.EventToolResolved, domain.ToolResolvedPayload{
		Result: domain.ToolResult{
			ToolCallID: "call-1", Name: "bash", Status: domain.ToolSuccess,
			Content: "刚才已经流式显示过的内容", ExitCode: &zero,
		},
	}))

	if strings.Contains(output, "刚才已经流式显示过的内容") {
		t.Errorf("观察正文被重复打印了: %q", output)
	}
}

// 终端不再报"输出已截断"——后端不截断了。
//
// 它仍然会报"另有 N 行未显示（模型收到的是完整内容）"，那是**展示层**的折叠，
// 括号里那句话现在是字面真实的。
func TestTerminalNeverClaimsTruncation(t *testing.T) {
	zero := 0
	output := render(event(domain.EventToolResolved, domain.ToolResolvedPayload{
		Result: domain.ToolResult{
			ToolCallID: "call-1", Status: domain.ToolSuccess,
			Content: "很长", ExitCode: &zero,
		},
	}))

	if strings.Contains(output, "输出已截断") {
		t.Errorf("输出 = %q; 后端已经不截断了，不该这么说", output)
	}
}

// 状态变化要成为明确的一行，用户才知道 Agent 迭代了几轮。
func TestWaitingModelIsAnnounced(t *testing.T) {
	output := render(event(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateWaitingModel}))

	if !strings.Contains(output, "请求模型") {
		t.Errorf("输出 = %q", output)
	}
}

// 本轮失败要说明原因。
func TestTurnFailedShowsTheReason(t *testing.T) {
	output := render(event(domain.EventTurnFailed, domain.TurnFailedPayload{Reason: "供应商 500"}))

	if !strings.Contains(output, "供应商 500") {
		t.Errorf("输出 = %q", output)
	}
}

// 不认识的事件类型要安静忽略，而不是崩掉或打出乱码——
// 事件类型会随阶段增加，终端只关心其中一部分。
func TestUnknownEventTypesAreIgnored(t *testing.T) {
	output := render(
		event(domain.EventType("something.new"), map[string]string{"a": "b"}),
		event(domain.EventTurnStarted, nil),
		event(domain.EventUserMessage, domain.UserMessagePayload{Content: "你好"}),
		event(domain.EventTurnCompleted, nil),
	)

	if output != "" {
		t.Errorf("不需要展示的事件产生了输出: %q", output)
	}
}

// payload 解不出来时只该少显示一行，不能影响别的事件。
func TestBrokenPayloadDoesNotBreakRendering(t *testing.T) {
	broken := domain.RunEvent{
		TurnID:  printerTurnID,
		Type:    domain.EventToolStarted,
		Payload: json.RawMessage(`{不是 JSON`),
	}
	output := render(broken, event(domain.EventTurnFailed, domain.TurnFailedPayload{Reason: "后续事件"}))

	if !strings.Contains(output, "后续事件") {
		t.Errorf("坏 payload 影响了后续事件: %q", output)
	}
}

// 参数不是合法 JSON 时展示不能失败，原样打印即可。
func TestCompactJSONFallsBackToRawText(t *testing.T) {
	if got := compactJSON(json.RawMessage("{不是 JSON")); got != "{不是 JSON" {
		t.Errorf("compactJSON = %q; want 原样返回", got)
	}
}

func TestDisplayLinesTruncatesLongOutputAndSaysSo(t *testing.T) {
	lines := displayLines(strings.Repeat("一行\n", maxDisplayedOutputLines+5))

	if len(lines) != maxDisplayedOutputLines+1 {
		t.Fatalf("展示 %d 行; want %d 行加一行说明", len(lines), maxDisplayedOutputLines)
	}
	if !strings.Contains(lines[len(lines)-1], "还有 5 行") {
		t.Errorf("省略说明 = %q", lines[len(lines)-1])
	}
}

func TestDisplayLinesReturnsNothingForEmptyOutput(t *testing.T) {
	if lines := displayLines("\n\n"); lines != nil {
		t.Errorf("只有换行的输出产生了 %v", lines)
	}
}

// 思考和正文是两种不同的东西，从一种切到另一种必须断行——
// 否则思考的最后一句会和回复的第一句黏在同一行上。
func TestSwitchingBetweenReasoningAndContentBreaksTheLine(t *testing.T) {
	output := render(
		event(domain.EventAssistantReasoningDelta, domain.TextDeltaPayload{Text: "先想想。"}),
		event(domain.EventAssistantDelta, domain.TextDeltaPayload{Text: "答案是 42。"}),
		event(domain.EventAssistantMessage, domain.AssistantMessagePayload{Content: "答案是 42。", Final: true}),
	)

	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("输出 %d 行; want 2（思考一行、正文一行）:\n%s", len(lines), output)
	}
	if !strings.Contains(lines[0], "先想想。") || strings.Contains(lines[0], "答案是 42。") {
		t.Errorf("思考和正文黏在了一行: %q", lines[0])
	}
	if !strings.Contains(lines[1], "答案是 42。") {
		t.Errorf("第二行 = %q", lines[1])
	}
}

// 一轮正常结束后空一行，让下一个提示符不贴着回复。
func TestCompletedTurnEndsWithABlankLine(t *testing.T) {
	output := render(
		event(domain.EventAssistantDelta, domain.TextDeltaPayload{Text: "答案"}),
		event(domain.EventAssistantMessage, domain.AssistantMessagePayload{Content: "答案", Final: true}),
		event(domain.EventTurnCompleted, nil),
	)

	if !strings.HasSuffix(output, "\n\n") {
		t.Errorf("一轮结束后没有空行: %q", output)
	}
}

// 终端只显示供应商实测的那条占用。估算那条紧接着就会被实测值覆盖，
// 两行数字一前一后跳动只会让人以为出了问题。
func TestOnlyProviderMeasuredUsageIsPrinted(t *testing.T) {
	estimated := render(event(domain.EventContextUsageUpdated, domain.NewContextUsagePayload(
		domain.ContextUsage{ContextWindow: 1000, InputTokens: 300, Source: domain.ContextUsageEstimated},
	)))
	if estimated != "" {
		t.Errorf("估算值被打印了出来: %q", estimated)
	}

	measured := render(event(domain.EventContextUsageUpdated, domain.NewContextUsagePayload(
		domain.ContextUsage{ContextWindow: 1000, InputTokens: 250, Source: domain.ContextUsageProvider},
	)))
	for _, want := range []string{"250", "1000", "25.0%", "750"} {
		if !strings.Contains(measured, want) {
			t.Errorf("输出缺少 %q: %q", want, measured)
		}
	}
}

// 没有配置窗口时只报绝对值，不编比例。
func TestUsageWithoutWindowShowsOnlyTheAbsoluteNumber(t *testing.T) {
	output := render(event(domain.EventContextUsageUpdated, domain.NewContextUsagePayload(
		domain.ContextUsage{InputTokens: 500, Source: domain.ContextUsageProvider},
	)))

	if !strings.Contains(output, "500") {
		t.Errorf("没有显示输入 token: %q", output)
	}
	if strings.Contains(output, "%") {
		t.Errorf("窗口未知时编出了比例: %q", output)
	}
}
