package tool

import (
	"bytes"

	"goseek/internal/domain"
)

// commandOutput 收集一条命令的全部输出。
//
// # 它不截断，一个字节都不截
//
// 上一版是 boundedOutput：总量超过 10000 字节就只留头尾、中间挖掉，并置一个
// truncated 标记。那个设计被推翻了，理由是**它在损害模型的判断**，而且损害的
// 方式很隐蔽——正文读起来是连续的，模型不容易意识到中间少了一段。
//
// 一次真实的翻车：模型执行 `ls -laR /usr/share/man/man1 | head -500`，输出被截到
// 160 行，它数不出条目数，于是从 `ls -la` 第二列的**硬链接数** 1173 推断出
// "1173 个 man 页面"——实际是 1171。这个数随后进了叶子摘要，又进了合并摘要，
// 一路当成事实继承下去。截断制造的不是"信息少了"，是"模型开始猜，而猜出来的东西
// 和事实长得一模一样"。
//
// 在 1M 上下文的模型面前，截断没有需求：一条命令如果真能产出接近窗口大小的输出，
// 那是这条命令写错了，不是压缩该兜住的边界情况。
//
// # 代价：进程内存不再有上界
//
// `find / -type f` 这类命令会把全部输出读进内存。这是上面那个决定的直接代价，
// 已记入文档的已知风险。工具本身不再设防——设防就是截断。
type commandOutput struct {
	// buffer 是完整输出。
	//
	// bytes.Buffer 会按需扩容，不预设上限——上限就是截断。
	buffer bytes.Buffer
	// onChunk 在每次写入时收到这一片原始内容，用于实时展示。
	onChunk domain.OutputFunc
}

// newCommandOutput 创建一个输出收集器。onChunk 可以为 nil。
func newCommandOutput(onChunk domain.OutputFunc) *commandOutput {
	if onChunk == nil {
		onChunk = func(string) {}
	}
	return &commandOutput{onChunk: onChunk}
}

// Write 实现 io.Writer。
//
// 它永远不返回错误并且始终报告写入了全部字节：返回短写会让 os/exec 的拷贝
// 提前中止，从而丢掉本可以观察到的输出。
//
// 它同时被用作命令的 Stdout 和 Stderr。os/exec 在 Stderr 与 Stdout 是同一个值时
// 复用同一个管道和同一个拷贝 goroutine，因此这里不需要加锁。
func (output *commandOutput) Write(chunk []byte) (int, error) {
	// 先原样交给展示，再落进缓冲。两条路径拿到的内容完全一样——
	// 不再存在"展示看到完整流、模型看到有界内容"这种分叉。
	output.onChunk(string(chunk))
	output.buffer.Write(chunk)
	return len(chunk), nil
}

// Result 返回收集到的完整内容。
//
// 没有第二个返回值：不存在"是否被截断"这个问题了。
func (output *commandOutput) Result() string {
	return output.buffer.String()
}
