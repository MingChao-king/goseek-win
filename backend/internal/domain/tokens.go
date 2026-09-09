package domain

import "unicode"

// token 估算。
//
// # 它的契约是"上界"，不是"近似值"
//
// 这一点必须写在最前面，因为整个压缩体系的终止性保证依赖它（设计文档 5.13.3）：
// 只要估算 ≥ 实际，压缩就永远能把上下文压到硬边界以下；一旦估算 < 实际，那条
// 保证就有洞，请求会被供应商直接拒。
//
// 所以这里的每一处取舍都朝"宁可偏高"那一侧：向上取整、乘安全系数、把看不见的
// 开销也算上。**估高的代价是压缩早触发一次（多花一次摘要调用），估低的代价是
// 对话中断。两者不对称。**
//
// 上界这个性质由 EstimateBreach 持续校验——供应商在每次响应里报的
// prompt_tokens 就是真值，一旦它超过我们的估算，说明上界被击穿，那是一个必须
// 立刻知道的信号。
//
// 这一块之后真正要推广使用的话，考虑使用脚本调用PYTHON使用分词器准确计算token
// 目前暂不考虑（Review 结论：不引入分词器。精确分词只能消除文本那部分误差，
// 供应商的消息框架开销没有公开文档、照旧要估；而这里唯一的硬要求是上界，
// 不是精确值。见设计文档 5.15）
//
// # 为什么不用真正的 tokenizer
//
// 真正的分词要么依赖一个几 MB 的词表——而且公开可得的是 OpenAI 的，与 DeepSeek
// 的切分并不一致——要么每次请求都多打一次网络。这两样在这个规模下都不划算。
//
// 而我们真正需要的精度并不高：这个数字有两个用途，一是显示给用户看个大概，
// 二是判断"是不是该压缩了"。前者差个百分之十无所谓，后者只要**宁可偏高**就行——
// 高估让压缩早一点触发（多花一次摘要调用），低估则会让请求被供应商直接拒。
//
// 请求发出之后还有第二道：供应商在响应里给出真实的 prompt_tokens，那时用实测值
// 覆盖估算值（见 ContextUsage.WithProviderTokens）。所以估算只需要在"发出之前"
// 这段时间里大致可靠。

// 下面这组系数是**用真实数据校准过的**，不是拍脑袋的经验值。
//
// 校准方法：跑一段多轮对话，把每次请求的本地估算与供应商在响应里报的
// prompt_tokens 逐对比较。第一版取 ASCII 4 字符/token、宽字符 1.5 字符/token，
// 结果五次请求全部偏低 33%–38%：
//
//	估算/实测: 440/688 (-36%), 496/738 (-33%), 575/908 (-37%),
//	           648/1044 (-38%), 670/1041 (-36%)
//
// 偏差如此一致，说明不是算法错而是系数错。两处都偏乐观：
//
//   - 中文在这个分词器下接近"一个字一个 token"，而不是 1.5 个字一个；
//   - 请求里的 ASCII 大头是 JSON（工具 Schema、工具调用参数、tool 消息的正文），
//     标点和引号密集，切得比英文散文碎得多，4 字符/token 只适用于散文。
//
// 校准后的系数见下。**偏差方向比精度更重要**：高估只会让压缩早一点触发，
// 多花一次摘要调用；低估会让请求被供应商直接拒，那是不可恢复的失败。
const (
	// asciiCharsPerToken 是 ASCII 文本的字符/token 比。
	//
	// 取 2.8 而不是散文常见的 4：这个项目里的 ASCII 绝大多数是 JSON 和 shell
	// 命令，切分密度更接近代码。
	asciiCharsPerToken = 2.8
	// wideCharsPerToken 是中日韩等宽字符的字符/token 比。
	//
	// 取 1.0，也就是一个字算一个 token。实测下来这个分词器对中文基本就是这个
	// 密度，1.5 明显低估。
	wideCharsPerToken = 1.0
	// messageOverheadTokens 是每条消息的固定开销。
	//
	// 供应商把消息渲染成带角色标记的结构，这部分不在正文里但要占额度。
	messageOverheadTokens = 4
	// toolSpecOverheadTokens 是每个工具定义除 Schema 正文之外的固定开销。
	toolSpecOverheadTokens = 8
	// DefaultSafetyFactor 是加在最终结果上的安全系数的初值。
	//
	// 校准过字符比之后，估算仍然稳定偏低约 3%–13%（见上面第二轮实测）。剩下的
	// 差额来自我们看不见的东西：供应商自己拼的请求前缀、工具协议的包装结构、
	// 以及分词器在标点和边界上的具体切法。继续拧字符比只是在拟合噪声。
	//
	// 与其假装能算准，不如把"宁可偏高"这个要求显式写出来：乘一个系数，让估算
	// 落在实测值上方一点。高估的代价是压缩早触发一次（多一次摘要调用），
	// 低估的代价是请求被供应商拒——后者不可恢复，前者只是多花一点钱。
	//
	// 它是**初值**而不是常量：运行中若发现实测值超过了估算（上界被击穿），
	// 调用方会把它调高，见 RaisedSafetyFactor。
	DefaultSafetyFactor = 1.15

	// minSafetyFactor 是安全系数的下界。
	//
	// 系数只会被调高不会被调低——调低要有"上界一直很宽裕"的证据，而那种证据
	// 需要长期统计，收益却只是省一点点上下文。不做。
	minSafetyFactor = 1.0
)

// EstimateBreach 描述一次"上界被击穿"：实测值超过了我们的估算。
//
// 它是一个应当被大声报出来的信号，不是一个可以忽略的偏差——上界不成立意味着
// 压缩的终止性保证有洞（见文件开头）。
type EstimateBreach struct {
	// Estimated 是发请求之前我们算出来的值。
	Estimated int
	// Actual 是供应商在响应里报的真值。
	Actual int
	// UsedFactor 是这次估算用的安全系数。
	UsedFactor float64
}

// Ratio 返回实测相对估算的倍数。大于 1 就是击穿。
func (breach EstimateBreach) Ratio() float64 {
	if breach.Estimated <= 0 {
		return 0
	}
	return float64(breach.Actual) / float64(breach.Estimated)
}

// RaisedSafetyFactor 返回一个足以覆盖这次击穿的新安全系数。
//
// 做法是把系数按击穿的倍数放大，再多留 5%——只补到刚好等于实测值是不够的，
// 下一次请求的内容构成稍有不同就会再次击穿，于是变成反复告警。
//
// **只调高不调低**：见 minSafetyFactor 的说明。
func RaisedSafetyFactor(current float64, breach EstimateBreach) float64 {
	raised := current * breach.Ratio() * 1.05
	if raised < current {
		return current
	}
	if raised < minSafetyFactor {
		return minSafetyFactor
	}
	return raised
}

// EstimateTokens 估算一段文本占多少 token。
//
// 按字符分两类累加：ASCII 走 4 字符一 token，其余走 1.5 字符一 token。
// 用 float 累加再向上取整，避免逐字符取整把小数丢光——一段 100 个 ASCII 字符
// 若逐字符取整会算成 100 个 token，而不是 25 个。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	var tokens float64
	for _, character := range text {
		if character < unicode.MaxASCII {
			tokens += 1 / asciiCharsPerToken
		} else {
			tokens += 1 / wideCharsPerToken
		}
	}

	// 向上取整：半个 token 也要占一个位置，而且这个方向符合"宁可偏高"。
	rounded := int(tokens)
	if tokens > float64(rounded) {
		rounded++
	}
	return rounded
}

// EstimateRequestTokens 用默认安全系数估算一次模型请求的输入 token 总数。
func EstimateRequestTokens(request ModelRequest) int {
	return EstimateRequestTokensWith(request, DefaultSafetyFactor)
}

// EstimateRequestTokensWith 用指定的安全系数估算一次模型请求的输入 token 总数。
//
// 它把请求里所有会被发给供应商的东西都算上：每条消息的正文、消息的固定开销、
// 工具调用的参数原文，以及工具定义。漏算任何一项都会让估算系统性偏低，
// 而偏低正是我们要避免的方向。
//
// 系数作为参数传进来而不是读一个包级变量：包级可变状态会让"这次估算用的是哪个
// 系数"变得不可知，而这个问题在排查上界击穿时恰恰是第一个要问的。
func EstimateRequestTokensWith(request ModelRequest, safetyFactor float64) int {
	total := 0

	for _, message := range request.Messages {
		// 正文。
		total += EstimateTokens(message.Content)
		// 图片：按 GLM 视觉 28×28 patch 的精确公式计（见设计文档 5.20.4）。
		// 与文本不同，这不是估算而是实测校准的精确值——但仍然放在安全系数之前
		// 加进去，让它和文本部分一起走系数放大。
		for _, image := range message.Images {
			total += EstimateImageTokens(image.Width, image.Height)
		}
		// 角色标记等固定开销。
		total += messageOverheadTokens
		// assistant 消息上的工具调用：名字和参数原文都会原样发出去。
		for _, call := range message.ToolCalls {
			total += EstimateTokens(call.Name)
			total += EstimateTokens(string(call.Arguments))
		}
		// tool 消息上的调用 id 也占额度。
		total += EstimateTokens(message.ToolCallID)
	}

	// 工具定义每次请求都完整发送一遍，是一笔固定但不小的开销——
	// 一个带 JSON Schema 的工具定义可能上百 token，漏算它会明显偏低。
	for _, spec := range request.Tools {
		total += EstimateTokens(spec.Name)
		total += EstimateTokens(spec.Description)
		total += EstimateTokens(string(spec.Parameters))
		total += toolSpecOverheadTokens
	}

	// 最后乘安全系数，把估算掰到实测值上方。理由见 DefaultSafetyFactor 的注释。
	if safetyFactor < minSafetyFactor {
		safetyFactor = minSafetyFactor
	}
	return int(float64(total)*safetyFactor + 0.5)
}
