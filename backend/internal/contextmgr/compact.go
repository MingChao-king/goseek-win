package contextmgr

import (
	"context"
	"fmt"
	"math"

	"goseek/internal/domain"
)

// 上下文压缩。
//
// # 什么时候压
//
//	输入硬边界 = 窗口 × (1 - 安全余量 5%)
//	触发线     = 硬边界 × 80%
//	目标线     = 硬边界 × 20%
//
// 超过触发线就开始压，一直压到低于目标线为止。两条线拉这么开是有意的：如果压到
// 79% 就停，下一条消息又会把它顶回 80%，于是几乎每轮都要压一次，每次都是一轮额外
// 的模型调用。压到 20% 则能撑很多轮。
//
// # 大前提：压缩必须成功
//
// **压缩存在的意义是支撑"无限"轮次的对话。** 因此"压不成功"不能是一个合法终态。
// 三条线的后果并不对称：
//
//	触发线   硬边界 × 80%   这是**开始**压的判据，不是终点
//	目标线   硬边界 × 20%   压不到只是这一轮贵一些，下一轮还会再压。**可接受**
//	硬边界   窗口   × 95%   压不到则请求发不出去，**对话终止**。**不可接受**
//
// 两条线拉这么开是有意的：如果压到 79% 就停，下一条消息又会把它顶回 80%，几乎每轮
// 都要压一次。压到 20% 能撑很多轮。
//
// # 怎么压：至多三步
//
//  1. 保留最近**两个完整轮次**的原文不动，把游标到保留区之间的全部原文压成一个
//     叶子摘要。低于目标线就结束。
//  2. 把活跃前沿里除最新叶子之外的全部节点**一次塌成**一个父节点。低于目标线就
//     结束——绝大多数情况到此为止。
//  3. 收尾诊断：两步都没压到，说明问题不在原文。判断是用户原话超标还是保留区
//     单轮超标，对症下手（见 Compactor.diagnoseStep）。
//
// # 为什么这样压一定能成功
//
// 第 3 步之后，上下文里只剩这些，每一项都有已知上界：
//
//	system 指令        489        固定
//	工具定义           687        固定
//	一个摘要           ≤ 应急上限
//	当前用户消息        ≤ 500 tok   单条截断阈值
//	最后一对调用/观察   ≤ 10000 字节 + 参数
//
// **这个和是常数**，不随会话长度增长。于是"压缩一定成功"有了精确形式：只要窗口
// 大到能装下「常量 + 一个摘要 + 一条用户消息 + 一对调用/观察」，压缩就永远能把
// 上下文压到硬边界以下。而这个条件在**启动时**就能检查（见 MinimumViableWindow）。
//
// # 收敛
//
// 每个被接受的步骤都让活跃 token 严格减少（acceptIfSmaller）。流程本身只有三步，
// 所以收敛是显然的——步数上限只是防"某种没预料到的输入"的保险丝。

const (
	// safetyMarginRatio 是从窗口里预留出来的安全余量。
	//
	// 估算不可能完全准确（见 domain/tokens.go 的校准记录），留 5% 让"估算说没超"
	// 和"实际真没超"之间有一点缓冲。
	safetyMarginRatio = 0.05
	// compactAtRatio 是触发压缩的水位，相对硬边界。
	compactAtRatio = 0.80
	// compactToRatio 是压缩的目标水位，相对硬边界。
	compactToRatio = 0.20
	// retainedTurns 是保留原文的最近轮次数。
	retainedTurns = 2
	// minRetainedTurns 是压到极限时的最低保留轮次。
	//
	// 正常情况保留 retainedTurns 轮。但如果压无可压之后仍然超出硬边界，请求多半
	// 会被供应商拒掉——那时宁可少保留一轮原文，也要让请求发得出去。
	//
	// 不退到 0：当前这一轮里有用户刚提的问题，把它也摘要掉，模型就不知道自己
	// 在回答什么了。
	minRetainedTurns = 1
	// maxCompactionSteps 是一次压缩里允许的模型调用次数上限。
	//
	// 从 12 降到 4：正常路径永远碰不到它——流程本身只有三步，收敛由 acceptIfSmaller
	// 保证。它只是防"某种没预料到的输入让每一步都看起来有进展"的保险丝。
	//
	// 12 那个值有过实际危害：成对合并把 K 个摘要收成 1 个需要恰好 K−1 次调用，
	// 于是 K≥20 时必然撞上限（实测 K=20 剩 8 段、K=40 剩 28 段），而"撞上限"被
	// 报成了失败。那不是保险丝，是一条会被撞到的成本闸门。
	maxCompactionSteps = 4
)

// Summarizer 是压缩对模型的要求：给一段指令和一段文本，返回摘要。
//
// 接口定义在调用方。它和 Agent 用的是同一个模型，但**不是同一种请求**：摘要请求
// 有自己的 system 指令、不带任何工具、也不让模型回答用户或执行任务。用一个独立的
// 窄接口把这件事表达出来，比传一个完整的 Model 进来再约定"记得别传 tools"要可靠。
type Summarizer interface {
	Summarize(ctx context.Context, instructions string, source string) (string, error)
}

// Thresholds 是根据窗口算出来的三条线。
type Thresholds struct {
	// HardLimit 是输入的硬边界，超过它请求多半会被供应商拒。
	HardLimit int
	// CompactAt 是触发线。
	CompactAt int
	// CompactTo 是目标线。
	CompactTo int
}

// NewThresholds 按窗口大小算出三条线。窗口未知（<=0）时全为 0，表示永不触发压缩。
//
// 窗口未知时**不压缩**，而不是猜一个窗口：压缩要花钱、要等待，还会把早期对话变成
// 有损的摘要。基于一个编出来的窗口做这件事，代价由用户承担。
func NewThresholds(contextWindow int) Thresholds {
	if contextWindow <= 0 {
		return Thresholds{}
	}
	hardLimit := int(float64(contextWindow) * (1 - safetyMarginRatio))
	return Thresholds{
		HardLimit: hardLimit,
		CompactAt: int(float64(hardLimit) * compactAtRatio),
		CompactTo: int(float64(hardLimit) * compactToRatio),
	}
}

// ShouldCompact 判断当前占用是否达到了触发线。
func (thresholds Thresholds) ShouldCompact(inputTokens int) bool {
	return thresholds.CompactAt > 0 && inputTokens >= thresholds.CompactAt
}

// CompactionResult 描述一次压缩做了什么。
type CompactionResult struct {
	// Memory 是压缩之后的记忆状态。
	Memory domain.ConversationMemory
	// NewBatches 是本次新生成的节点，供事件和界面展示。
	NewBatches []domain.MemoryBatch
	// BeforeTokens 与 AfterTokens 是压缩前后的输入 token 估算。
	BeforeTokens int
	AfterTokens  int
	// TargetUnreachable 表示已经**低于硬边界、但没到目标线**。
	//
	// # 语义在 M5.1 收窄了
	//
	// 它原本表示"压缩失败"，把"压不动"合理化了：系统在压不动时安静地继续超线，
	// 直到某一刻请求被供应商拒绝，对话就此终止。
	//
	// 现在它表示一个**可接受的中间状态**：这一轮的上下文贵一些，下一轮压缩会继续。
	// 界面据此说"这次只整理了一部分，下一轮会继续"，而不是报失败。
	//
	// "压不到硬边界"则根本不是一个合法终态，由本文件开头那条保证兜住。
	TargetUnreachable bool
	// Reason 说明为什么没压到目标线，或者第 3 步诊断出了什么。
	//
	// 界面要能告诉用户**现在发生了什么、该做什么**：走到第 3 步不是正常工作状态，
	// 而是一个信号——这个会话该结束了，或者窗口配小了。只说"没压到"等于没说。
	Reason string
}

// Changed 表示这次压缩是否真的改变了什么。
func (result CompactionResult) Changed() bool {
	return len(result.NewBatches) > 0
}

// 最小可行窗口。
//
// 5.13 那条保证——"只要窗口大到能装下常量部分加最后那点原文，压缩就永远能把上下文
// 压到硬边界以下"——的成立条件是一个可以**在启动时检查**的常数。检查它，就把
// "第一万轮时的运行时失败"提前成了"启动时的配置错误"。
const (
	// constantOverheadTokens 是每次请求必发的固定开销，实测值。
	//
	//	system 指令        489 tok
	//	工具定义（2 个）    687 tok
	//	合计              1176 tok        ← 与窗口大小无关
	//
	// 这个数曾经造成过一次难以归因的故障：GOSEEK_CONTEXT_WINDOW=6000 时目标线是
	// 1140，**比常量部分还小**。当时把"每次压缩都 TargetUnreachable"归因为
	// "保留区占满"，其实光 system 加工具定义就已经超了。
	constantOverheadTokens = 1176
	// emergencySummaryTokens 是第 3 步之后上下文里那个摘要的应急上限。
	//
	// 它不是猜的，而是从保证反推：硬边界减掉别的几项，剩下多少就是多少。这里给的
	// 是**下限要求**——最小可行窗口必须能装下一个像样的摘要，否则"压缩一定成功"
	// 只在纸面上成立（压到只剩一句话的摘要，模型什么都做不了）。
	emergencySummaryTokens = 4000
	// lastExchangeTokens 是"当前用户消息 + 最后一对调用/观察"的上界。
	//
	//	当前用户消息       ≤  500 tok   单条截断阈值（见 quotes.go）
	//	最后一对调用/观察   ≤ 3000 tok   观察本身有 10000 字节的上限（tool/output.go）
	lastExchangeTokens = 3500
)

// MinimumViableWindow 返回压缩能够成立的最小上下文窗口。
//
// 小于它的窗口装不下「常量 + 一个摘要 + 一条用户消息 + 一对调用/观察」，
// "压缩一定成功"这条保证就不成立，系统迟早会在某一轮被供应商拒绝。
//
// 除以 (1 - safetyMarginRatio) 是因为上面几项加起来要落在**硬边界**之内，
// 而硬边界只有窗口的 95%。
func MinimumViableWindow() int {
	needed := constantOverheadTokens + emergencySummaryTokens + lastExchangeTokens
	// 向上取整。差一个 token 也是差：这个值的用途是判断"硬边界装不装得下"，
	// 而硬边界那边是向下取整的，两处都截断的话会算出一个刚好装不下的"最小值"。
	return int(math.Ceil(float64(needed) / (1 - safetyMarginRatio)))
}

// CheckWindow 判断一个窗口配置能不能支撑压缩。
//
// 返回 nil 表示可用。窗口为 0（未知）也返回 nil：那表示"永不压缩"，是一个明确的、
// 用户可以选择的模式，不是配置错误。
//
// # 为什么是报错而不是打一行告警
//
// 在这种窗口下那条保证不成立。告警会被划过去，然后在第几百轮的某一次请求上，
// 用户得到的是供应商的一句 400 而不是回答，而那时根本看不出是窗口配小了。
//
// 理由的另一面：在一个连 system 指令加工具定义都装不下的窗口里做分层记忆本身没有
// 意义。真要支持 8k 级别的小模型，那是另一套设计（更可能是"只保留最近 N 轮，
// 早的直接丢"），而不是把这套硬塞进去。
func CheckWindow(contextWindow int) error {
	if contextWindow <= 0 {
		return nil
	}
	if minimum := MinimumViableWindow(); contextWindow < minimum {
		return fmt.Errorf(
			"上下文窗口 %d 太小：压缩至少需要 %d，才装得下固定开销 %d、一个摘要 %d、"+
				"以及当前这一轮的用户消息和最后一对命令/输出 %d。"+
				"在更小的窗口里，压缩迟早会压不下去，请求会被供应商直接拒绝",
			contextWindow, minimum, constantOverheadTokens,
			emergencySummaryTokens, lastExchangeTokens)
	}
	return nil
}
