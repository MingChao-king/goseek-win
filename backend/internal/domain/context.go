package domain

// ContextUsageSource 说明 ContextUsage 里的数字是怎么来的，也就是它有多可信。
//
// 界面上要把这一点讲清楚：一个估算出来的 78% 和一个供应商实测的 78%，
// 用户对它的信任程度应该不一样。
type ContextUsageSource string

const (
	// ContextUsageUnknown 表示还没有测量过，或者根本没有配置上下文窗口。
	// 此时界面显示"未知"，而不是编一个数字。
	ContextUsageUnknown ContextUsageSource = "unknown"
	// ContextUsageEstimated 表示由本地按字符估算得出，在请求发出之前就有。
	ContextUsageEstimated ContextUsageSource = "estimated"
	// ContextUsageProvider 表示来自供应商响应里的 prompt_tokens，是这次请求的真实输入量。
	ContextUsageProvider ContextUsageSource = "provider"
)

// ContextUsage 描述一次模型请求占用了多少上下文容量。
//
// # 为什么只有两个原始量
//
// 剩余容量和占用比例都是从这两个数算出来的，因此写成方法而不是字段。
//
// 前一版方案把它们做成了字段，于是不得不写一段校验去保证
// "RemainingTokens 必须精确等于 window - input"——需要写这种校验，恰恰说明它们
// 不该是字段。派生值用方法算，那段校验和它的测试就都不存在了。这也是当初推翻
// "横切分层"那套阶段划分的证据之一（见设计文档 3.1）。
type ContextUsage struct {
	// ContextWindow 是模型一次调用可用的总容量（输入与输出共享）。
	//
	// 0 表示未配置。供应商的接口不返回这个值，只能由用户配置，因此它可能缺席——
	// 缺席时不猜一个数字，因为一个假的窗口会让压缩在错误的时机触发。
	ContextWindow int
	// InputTokens 是本次视图的输入 token 数。0 且 Source 为 unknown 表示还没测过。
	InputTokens int
	// Source 说明这个数字是估算的还是实测的。
	Source ContextUsageSource
	// CacheHitTokens 与 CacheMissTokens 是这次输入里命中/未命中供应商上下文缓存的
	// 部分。只有 Source 为 provider 时才有意义——估算阶段无从得知。
	//
	// 它们**不参与任何决策**，只用于观测。放进 ContextUsage 而不是另开一条事件，
	// 是因为它们和 InputTokens 是同一次响应里的同一组数字，拆开会让消费端要自己
	// 把两条事件对起来。
	CacheHitTokens  int
	CacheMissTokens int
}

// CacheKnown 表示供应商这次报了缓存数据。
//
// 判据是"命中 + 未命中 == 总输入"，而不是"两者是否为 0"：**0 命中是一个有意义的
// 值**（前缀全新，比如一次压缩刚换掉整条前缀），把它和"供应商没报"混为一谈，
// 恰好会漏掉最值得看的那一次。
func (usage ContextUsage) CacheKnown() bool {
	return usage.InputTokens > 0 &&
		usage.CacheHitTokens+usage.CacheMissTokens == usage.InputTokens
}

// CacheHitRatio 返回缓存命中比例，取值 [0,1]。没有缓存数据时返回 0。
func (usage ContextUsage) CacheHitRatio() float64 {
	if !usage.CacheKnown() {
		return 0
	}
	return float64(usage.CacheHitTokens) / float64(usage.InputTokens)
}

// Known 表示这份占用信息是否可用于展示和判断。
//
// 窗口和输入量缺任何一个，比例就算不出来，界面只能显示"未知"。
func (usage ContextUsage) Known() bool {
	return usage.ContextWindow > 0 && usage.Source != ContextUsageUnknown
}

// Remaining 返回还能容纳多少 token。
//
// 取 max(window-input, 0)：输入超过窗口在估算偏高时是可能的，此时返回 0 比返回
// 一个负数好——界面上"剩余 -300"没有意义，而"剩余 0"正确地表达了"满了"。
func (usage ContextUsage) Remaining() int {
	if !usage.Known() {
		return 0
	}
	if remaining := usage.ContextWindow - usage.InputTokens; remaining > 0 {
		return remaining
	}
	return 0
}

// Ratio 返回占用比例，窗口未知时为 0。
//
// 允许大于 1：估算偏高时确实会出现这种情况，界面把它显示成"超过 100%"比悄悄
// 截断成 100% 更诚实——那正是需要压缩的信号。
func (usage ContextUsage) Ratio() float64 {
	if !usage.Known() {
		return 0
	}
	return float64(usage.InputTokens) / float64(usage.ContextWindow)
}

// WithProviderTokens 返回一份把输入量换成供应商实测值的副本。
//
// 供应商在响应里给出的 prompt_tokens 是这次请求的**真实**输入 token，
// 比本地估算准确，因此收到之后就用它覆盖，并把来源标成 provider。
//
// 返回副本而不是就地改：ContextUsage 会被放进事件 payload 交给多个消费者，
// 就地改会让已经发出去的那份跟着变。
// hit / miss 是同一次响应里的缓存数据，一并带进来——它们和 promptTokens 是一组，
// 分两次传会让调用方有机会把不同请求的数字拼在一起。
func (usage ContextUsage) WithProviderTokens(promptTokens, hit, miss int) ContextUsage {
	usage.InputTokens = promptTokens
	usage.CacheHitTokens = hit
	usage.CacheMissTokens = miss
	usage.Source = ContextUsageProvider
	return usage
}
