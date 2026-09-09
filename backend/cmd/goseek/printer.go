package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"goseek/internal/domain"
)

const (
	// maxDisplayedOutputLines 限制终端上展示的工具输出行数。
	//
	// 模型仍然收到完整的（已按字节设上限的）观察，这里只是不让一条几百行的输出
	// 把执行过程刷走。省略多少行会明确写出来。
	maxDisplayedOutputLines = 12

	// stepIndent 是步骤行的缩进，让过程与最终回复在视觉上分开。
	stepIndent = "  "
	// detailIndent 是步骤细节（命令、输出）的缩进。
	detailIndent = "    "
)

// stepPrinter 把一轮交互产生的事件渲染到终端。
//
// 它是 agent.EventSink 的实现，也是事件模型的第一个消费者。事件描述的是"发生了
// 什么"，怎么显示完全由这里决定——同一批事件将来也会推给浏览器，那边会画成完全
// 不同的样子。
//
// Emit 不返回错误：展示失败不能影响交互结果。
//
// 关于并发：工具输出的片段来自 os/exec 的拷贝 goroutine，而其余事件来自 Agent
// 所在的 goroutine。当前两者不会同时发生——执行工具期间 Agent 正阻塞在 Execute
// 里面，不会产生别的事件——因此这里不加锁。这个前提由 go test -race 守着；一旦
// M3.2 引入 Runner 使它不再成立，这里就要跟着改。
type stepPrinter struct {
	writer io.Writer
	// streamPrefix 记录当前这段流式文字用的是哪个前缀，空表示当前没有在流式输出。
	//
	// 记前缀而不是一个布尔值，是为了发现"思考切换到正文"这种情况：两段文字属于
	// 不同的东西，必须断行分开，否则思考的最后一句会和回复的第一句黏在一起。
	streamPrefix string
	// streaming 表示当前正处在一段流式文字中间。
	streaming bool
	// streamedLines 是当前这次工具调用已经打印过的输出行数，用于截断展示。
	streamedLines int
	// streamedOverflow 是因超出展示上限而没有打印的行数。
	streamedOverflow int
	// wrote 表示本渲染器已经往终端写过内容。
	wrote bool
}

// newStepPrinter 创建一个写往指定目标的事件渲染器。
func newStepPrinter(writer io.Writer) *stepPrinter {
	return &stepPrinter{writer: writer}
}

// printf 写一段内容并记下"已经写过东西"。
//
// 所有输出都走这里，wrote 的维护就只有一处，不会因为新增一种事件而漏掉。
func (printer *stepPrinter) printf(format string, arguments ...any) {
	fmt.Fprintf(printer.writer, format, arguments...)
	printer.wrote = true
}

// Emit 按事件类型渲染。
//
// 用 switch 而不是给每种事件一个方法：事件类型会随阶段增加，而终端只关心其中一部分；
// 集中在一处分派，新增类型时能一眼看出这里有没有跟进，不认识的类型也能安静忽略。
func (printer *stepPrinter) Emit(event domain.RunEvent) {
	switch event.Type {
	case domain.EventStateChanged:
		printer.onStateChanged(event)
	case domain.EventAssistantDelta:
		printer.onTextDelta(event, "")
	case domain.EventAssistantReasoningDelta:
		printer.onTextDelta(event, "思考 ")
	case domain.EventAssistantMessage:
		printer.onAssistantMessage(event)
	case domain.EventToolStarted:
		printer.onToolStarted(event)
	case domain.EventToolOutputDelta:
		printer.onToolOutputDelta(event)
	case domain.EventToolResolved:
		printer.onToolResolved(event)
	case domain.EventTurnCompleted:
		printer.onTurnCompleted()
	case domain.EventTurnFailed:
		printer.onTurnFailed(event)
	case domain.EventContextUsageUpdated:
		printer.onContextUsage(event)
	case domain.EventContextCompactionStarted:
		printer.onCompactionStarted(event)
	case domain.EventContextCompactionCompleted:
		printer.onCompactionCompleted(event)
	}
	// turn.started 和 user.message 在终端上不需要额外提示：用户刚敲完那条消息。
}

// onStateChanged 提示 Agent 切换到了什么状态。
//
// 一轮里可能请求模型多次，每次都提示，用户因此能看出 Agent 迭代了几轮。
func (printer *stepPrinter) onStateChanged(event domain.RunEvent) {
	var payload domain.StateChangedPayload
	if !decodePayload(event, &payload) {
		return
	}
	switch payload.State {
	case domain.StateWaitingModel:
		printer.step("请求模型")
	case domain.StateRunningTool:
		// 具体是哪个调用由随后的 tool.started 说明，这里不重复。
	case domain.StateCompressing:
		// 同理：紧跟着的 context.compaction.started 会说明为什么要压缩。
	}
}

// onTextDelta 增量打印模型文字。
//
// 正文和思考走同一条路径，只是前缀不同：思考内容对理解 Agent 在想什么很有帮助，
// 但要能一眼和最终回复区分开。
func (printer *stepPrinter) onTextDelta(event domain.RunEvent, prefix string) {
	var payload domain.TextDeltaPayload
	if !decodePayload(event, &payload) {
		return
	}
	// 从思考切到正文（或反过来）时要另起一段：它们是两种不同的东西，
	// 连在一行上会让人分不清哪句是回复。
	if printer.streaming && printer.streamPrefix != prefix {
		printer.endStreaming()
	}
	if !printer.streaming {
		// 模型文字顶格，不带缩进也不带 · 标记：缩进并带标记的是"程序做了什么"，
		// 顶格的是"模型说了什么"，读的人不必去分辨哪句话是谁说的。
		//
		// 顶格还解决了多行回复的对齐问题：流式文字里的换行是模型自己写的，
		// 如果首行缩进而续行不缩进，一段带代码块的回复看起来就是歪的。
		printer.printf("%s", prefix)
		printer.streaming = true
		printer.streamPrefix = prefix
	}
	printer.printf("%s", payload.Text)
}

// onAssistantMessage 在一段流式文字结束后收尾。
//
// 正文已经随 delta 逐字打印出来了，这里只补一个换行，**不再重复打印一遍**。
// 这也是 REPL 不打印 Handle 返回值的原因：那段文字刚刚已经流过去了，再打一次
// 就是同一句话出现两遍。
func (printer *stepPrinter) onAssistantMessage(event domain.RunEvent) {
	printer.endStreaming()
}

// onTurnCompleted 在一轮正常结束后空一行，让下一个提示符不贴着回复。
//
// 只在本轮确实往终端写过东西时才空行：一个什么都没显示的事件序列不该凭空
// 产生一个空行。
func (printer *stepPrinter) onTurnCompleted() {
	printer.endStreaming()
	if printer.wrote {
		printer.printf("\n")
	}
}

// onToolStarted 展示即将执行的调用：模型自述的意图，以及真正发出的参数。
//
// 两者并列打印是有意的。标题来自模型的 purpose，属于自述；参数原文才是事实。
// 两者对不上时，用户一眼就能看出来。
func (printer *stepPrinter) onToolStarted(event domain.RunEvent) {
	var payload domain.ToolStartedPayload
	if !decodePayload(event, &payload) {
		return
	}
	printer.endStreaming()
	printer.step(payload.Title)
	printer.printf("%s%s %s\n",
		detailIndent, payload.Call.Name, compactJSON(payload.Call.Arguments))

	printer.streamedLines = 0
	printer.streamedOverflow = 0
}

// onToolOutputDelta 增量打印命令输出。
//
// 只打印前若干行，超出的部分计数、不打印——模型收到的仍然是完整（有界）的观察，
// 这里只是不让一条几百行的输出把执行过程刷走。
func (printer *stepPrinter) onToolOutputDelta(event domain.RunEvent) {
	var payload domain.ToolOutputDeltaPayload
	if !decodePayload(event, &payload) {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(payload.Chunk, "\n"), "\n") {
		if printer.streamedLines >= maxDisplayedOutputLines {
			printer.streamedOverflow++
			continue
		}
		printer.printf("%s%s\n", detailIndent, line)
		printer.streamedLines++
	}
}

// onToolResolved 展示一次调用的结局：状态、退出码，以及被省略的输出行数。
//
// 输出本身已经随 delta 打印过了，这里不重复。
func (printer *stepPrinter) onToolResolved(event domain.RunEvent) {
	var payload domain.ToolResolvedPayload
	if !decodePayload(event, &payload) {
		return
	}
	result := payload.Result

	summary := string(result.Status)
	if result.ExitCode != nil {
		summary = fmt.Sprintf("%s，exit %d", summary, *result.ExitCode)
	}
	// 终端只按行折叠显示，模型收到的始终是完整内容——这是展示层的取舍，
	// 后端不再有任何截断（见 tool/output.go）。
	if printer.streamedOverflow > 0 {
		summary += fmt.Sprintf("，另有 %d 行未显示（模型收到的是完整内容）", printer.streamedOverflow)
	}
	printer.printf("%s%s\n", detailIndent, summary)

	// 被程序阻止的调用（未知工具、参数非法）没有输出流，说明只在观察里，
	// 此时把它打出来，否则用户只看到一个光秃秃的 error。
	if result.ExitCode == nil && strings.TrimSpace(result.Content) != "" {
		for _, line := range displayLines(result.Content) {
			printer.printf("%s%s\n", detailIndent, line)
		}
	}
}

// onTurnFailed 说明本轮为什么失败。
func (printer *stepPrinter) onTurnFailed(event domain.RunEvent) {
	var payload domain.TurnFailedPayload
	if !decodePayload(event, &payload) {
		return
	}
	printer.endStreaming()
	printer.step("本轮失败：" + payload.Reason)
}

// onContextUsage 显示这次请求占了多少上下文。
//
// 一轮里会收到两次：请求发出前是本地估算，响应回来后是供应商实测。终端只显示
// 后者——估算那条紧接着就会被覆盖，两行数字一前一后跳动只会让人以为出了问题。
// 浏览器那边显示两次，因为它有一个持续可见的仪表，数字被校正是有意义的反馈。
func (printer *stepPrinter) onContextUsage(event domain.RunEvent) {
	var payload domain.ContextUsagePayload
	if !decodePayload(event, &payload) {
		return
	}
	if payload.Source != string(domain.ContextUsageProvider) {
		return
	}

	printer.endStreaming()
	if payload.ContextWindow <= 0 {
		// 没配置窗口就只报绝对值，不编一个比例出来。
		printer.printf("%s· 上下文 %d tokens（未配置窗口大小）\n", stepIndent, payload.InputTokens)
		return
	}
	printer.printf("%s· 上下文 %d / %d tokens（%.1f%%，剩余 %d）\n",
		stepIndent, payload.InputTokens, payload.ContextWindow,
		payload.Ratio*100, payload.Remaining)
}

// onCompactionStarted 说明为什么现在要整理上下文。
//
// 这一条不能省：压缩可能持续几十秒（每生成一个摘要节点都是一次模型调用），
// 终端上如果只是安静地停住，用户会以为程序卡死了。两个数字一起给出，
// 他才知道是"占用超过了触发线"而不是别的什么原因。
func (printer *stepPrinter) onCompactionStarted(event domain.RunEvent) {
	var payload domain.CompactionStartedPayload
	if !decodePayload(event, &payload) {
		return
	}
	printer.step(fmt.Sprintf("整理上下文（已占用 %d，超过触发线 %d）",
		payload.InputTokens, payload.Threshold))
}

// onCompactionCompleted 报告整理的结果。
//
// 三种结局都要说清楚，因为它们对用户意味着不同的事：
//   - 失败：这一轮照常继续，只是没省下 token——不能让人以为回答也出问题了；
//   - 压不到目标：受"保留最近两轮原文"的硬约束所限，不是出了故障；
//   - 成功：列出折叠了哪几段，用户据此知道模型眼前不再是全部原文。
func (printer *stepPrinter) onCompactionCompleted(event domain.RunEvent) {
	var payload domain.CompactionCompletedPayload
	if !decodePayload(event, &payload) {
		return
	}

	if payload.Failed != "" {
		printer.step(fmt.Sprintf("整理上下文未成功（%s），本轮按原上下文继续", payload.Failed))
		return
	}

	// 一个节点都没产生：什么都没压。这时说"整理完成"是误导——用户会以为上下文
	// 变小了，而那两个数字明明一样。两种情况的原因不同，分别说清楚。
	if len(payload.Batches) == 0 {
		if payload.TargetUnreachable {
			printer.step(fmt.Sprintf(
				"无需整理：能压的都压过了，剩下的是保留区的原文（当前 %d tokens）",
				payload.AfterTokens))
		} else {
			printer.step(fmt.Sprintf("无需整理：当前 %d tokens，已在目标线以下",
				payload.AfterTokens))
		}
		return
	}

	headline := fmt.Sprintf("整理完成：%d → %d tokens",
		payload.BeforeTokens, payload.AfterTokens)
	if payload.TargetUnreachable {
		headline += "（受保留最近两轮原文所限，未能压到目标）"
	}
	printer.step(headline)

	// 每段折叠单独一行，缩进到细节层级。序号是给人看的闭区间，从 1 开始。
	for _, batch := range payload.Batches {
		printer.printf("%s#%d–%d %s\n",
			detailIndent, batch.StartMessage, batch.EndMessage, batch.Title)
	}
}

// step 打印一行步骤标记。
func (printer *stepPrinter) step(text string) {
	printer.endStreaming()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	printer.printf("%s· %s\n", stepIndent, lines[0])
	// 多行说明的续行统一缩进到细节层级；只给首行加标记、其余顶格，
	// 会让它看起来像是本轮的最终回复。
	for _, line := range lines[1:] {
		printer.printf("%s%s\n", detailIndent, line)
	}
}

// endStreaming 结束当前这段流式文字，必要时补一个换行。
func (printer *stepPrinter) endStreaming() {
	if printer.streaming {
		printer.printf("\n")
		printer.streaming = false
		printer.streamPrefix = ""
	}
}

// decodePayload 解出事件的 payload，失败时安静跳过这个事件。
//
// payload 由本进程刚刚序列化，解析不可能失败；即便失败也只该少显示一行，
// 不能让展示层影响交互。
func decodePayload(event domain.RunEvent, target any) bool {
	if len(event.Payload) == 0 {
		return false
	}
	return json.Unmarshal(event.Payload, target) == nil
}

// displayLines 把一段文字裁成适合终端展示的若干行。
func displayLines(content string) []string {
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return nil
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) <= maxDisplayedOutputLines {
		return lines
	}
	kept := append([]string(nil), lines[:maxDisplayedOutputLines]...)
	return append(kept, fmt.Sprintf("…（还有 %d 行，模型收到的是完整内容）",
		len(lines)-maxDisplayedOutputLines))
}

// compactJSON 把参数压成一行，便于和标题并排展示。
//
// 参数是模型给出的原文，可能带换行和缩进；压缩失败时原样返回，因为展示不该因为
// 内容不好看而失败。
func compactJSON(raw json.RawMessage) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return string(raw)
	}
	return buffer.String()
}
