package contextmgr

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"goseek/internal/domain"
)

// 超大观察在**视图层**收边界。
//
// # 为什么是视图层，不是工具层
//
// Claude Code 在工具层截断：30000 字符进对话历史，超出的落到一个临时文件，模型
// 拿到 5KB 预览加文件路径（`BASH_MAX_OUTPUT_LENGTH` 可调）。而 Anthropic 的 bash
// tool 文档明说 API 自己不截断——"The API doesn't truncate tool results (an
// oversized request is rejected). Truncate large outputs in your application"。
//
// GoSeek 换个位置做同一件事，而且更干净：
//
//	存储     完整落库，零截断
//	视图     渲染时有界 + 标注总量 + 序号
//	read     按序号 + 行区间取回
//
// **不需要临时文件——会话历史本身就是那个持久存储**，`conversation_history` 已经是
// 取回入口。序号比文件路径可靠：文件会被清掉，历史不会。
//
// 在工具层截断是上一版 GoSeek 的做法，它的致命之处是**被挖掉的部分从此不在库里**，
// 于是模型没有任何追索路径。那才是"1173 个 man 页面"那类假事实的成因（见现状文档
// 9.21）：模型拿到一段读起来连续的文本，只能从可见的元数据硬猜。
//
// # 截断与分页的区别
//
// 光标注"这里省略了"是不够的——那只保证模型不会以为自己看到了全貌，**不保证那部分
// 还能拿回来**。要能拿回来必须有续读入口。所以这里的标注一定带着序号和取回命令：
// 模型知道漏了多少，也知道怎么补。
//
// **截断是我替它决定它看不到什么；分页是它自己决定先看哪一段。** 存储零截断加上
// 可分页的 read，这两者合起来才是"任意长的内容都能完整读完"。

const (
	// maxObservationChars 是单条工具观察在视图里渲染的字符上限。
	//
	// 取 30000 对齐 Claude Code 的 `BASH_MAX_OUTPUT_LENGTH` 默认值。上一版 GoSeek
	// 是 10000，在 1M 窗口的模型面前确实太小了。
	//
	// 只作用于**工具观察**。用户消息和模型输出不收边界：实测用户消息只占全部内容的
	// 0.4%–0.8%，而工具观察占 84%–95.6%——收边界要收在占地方的那一类上，
	// 而不是收在唯一无法重新推导的那一类上。
	maxObservationChars = 30000
	// observationHeadRatio 是头部占上限的比例，其余留给尾部。
	//
	// 头部占三分之二：命令的开头通常是要的内容，结尾主要用来兜住错误信息——
	// 失败原因往往正在 stderr 的最后几行。
	observationHeadRatio = 2.0 / 3.0
)

// renderRawMessage 把一条历史消息渲染成视图里的一条消息，超大的工具观察收边界。
//
// index 是它在完整历史里的下标（从 0 起），标注里换成从 1 起的序号——那正是
// `conversation_history read from=N` 的入参口径。
func renderRawMessage(message domain.Message, index int) domain.ModelMessage {
	rendered := domain.ModelMessage{
		Role:       message.Role.ModelRole(),
		Content:    message.Content,
		ToolCalls:  message.ToolCalls,
		ToolCallID: message.ToolCallID,
		Images:     message.Images,
	}
	// 只收工具观察的边界。理由见 maxObservationChars 的注释。
	rendered.Content = boundedContent(message, index)
	return rendered
}

// boundedContent 返回一条消息**按视图口径**的正文。
//
// 摘要器的源文本必须走这里，理由有两条：
//
//  1. **不然兜底会自己发不出去。** 终结性全塌的源文本就是保留区的原文，真机上出现过
//     136 万 token 的源文本——那个请求超过了模型的真实窗口，压缩在最需要它的时候
//     失败了（现状文档 9.22）。
//  2. **摘要应该总结"模型当时看到的东西"。** 视图给模型的是头 + 尾，如果摘要拿完整
//     原文去总结，摘要里就会提到模型从没见过的内容——那比信息少更糟，它让摘要和
//     模型的记忆对不上。
func boundedContent(message domain.Message, index int) string {
	if message.Role != domain.RoleTool || len(message.Content) <= maxObservationChars {
		return message.Content
	}
	return boundObservation(message.Content, index+1)
}

// boundObservation 把一条超大观察收成"头 + 标注 + 尾"。
//
// 标注里必须同时有三样东西，缺一样这个机制就退化成有害截断：
//
//	总量（字节数与行数）  模型才知道自己漏了多少，而不是以为看到了全貌
//	序号                 模型才知道去哪里取
//	取回命令             模型才知道怎么取——写出来比让它自己想更可靠
func boundObservation(content string, number int) string {
	totalBytes := len(content)
	// 数行数前先去掉末尾的换行：命令输出几乎都以 \n 结尾，直接 Count+1 会把那个
	// 空行也算进去，报出来的总行数就比模型翻页时看到的多一行。两处口径必须一致，
	// 否则模型翻到最后会以为还差一行没读到。
	totalLines := strings.Count(strings.TrimRight(content, "\n"), "\n") + 1

	headLimit := int(float64(maxObservationChars) * observationHeadRatio)
	tailLimit := maxObservationChars - headLimit
	head := trimPartialRuneSuffix(content[:headLimit])
	tail := trimPartialRunePrefix(content[totalBytes-tailLimit:])

	return fmt.Sprintf("%s\n\n…[这条观察共 %d 字节 / %d 行，上面是开头、下面是结尾，"+
		"中间省略了 %d 字节。**完整原文在会话历史里一个字节都没少**，"+
		"用 conversation_history 取回：read from=%d lines=\"1-200\"，"+
		"按需要改行区间往后翻]…\n\n%s",
		head, totalBytes, totalLines, totalBytes-headLimit-tailLimit, number, tail)
}

// trimPartialRuneSuffix 去掉末尾可能被切断的半个 UTF-8 字符。
//
// 按字节切必然会切在多字节字符中间。把残缺字节去掉，避免把无效 UTF-8 交给模型
// ——那会让它看到一个乱码字符，而乱码是它唯一无法判断"这是内容还是故障"的东西。
func trimPartialRuneSuffix(text string) string {
	for len(text) > 0 {
		if r, size := utf8.DecodeLastRuneInString(text); r != utf8.RuneError || size != 1 {
			break
		}
		text = text[:len(text)-1]
	}
	return text
}

// trimPartialRunePrefix 去掉开头可能被切断的半个 UTF-8 字符。
func trimPartialRunePrefix(text string) string {
	for len(text) > 0 {
		if r, size := utf8.DecodeRuneInString(text); r != utf8.RuneError || size != 1 {
			break
		}
		text = text[1:]
	}
	return text
}
