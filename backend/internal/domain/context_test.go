package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// 估算的绝对精度不重要，但方向必须对：**宁可偏高**。
// 高估只会让压缩早一点触发，低估则会让请求被供应商直接拒。
func TestEstimateTokensStaysInAReasonableRange(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		atLeast int
		atMost  int
	}{
		{"空文本", "", 0, 0},
		// 100 个 ASCII 字符，按 2.8 字符一 token 约 36 个。
		{"纯 ASCII", strings.Repeat("a", 100), 30, 42},
		// 30 个汉字，按一字一 token 就是 30 个。
		{"纯中文", strings.Repeat("中", 30), 28, 34},
		// 30 个 "a中"：30 个 ASCII 约 11 个 token，30 个汉字 30 个 token。
		{"中英混合", strings.Repeat("a中", 30), 38, 46},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := domain.EstimateTokens(testCase.text)
			if got < testCase.atLeast || got > testCase.atMost {
				t.Errorf("EstimateTokens = %d; want 落在 [%d, %d]",
					got, testCase.atLeast, testCase.atMost)
			}
		})
	}
}

// 文本变长，估算值必须单调不减——否则"占用随对话增长"这个前提就不成立了。
func TestEstimateTokensIsMonotonic(t *testing.T) {
	previous := 0
	for length := 0; length <= 200; length += 20 {
		current := domain.EstimateTokens(strings.Repeat("字", length))
		if current < previous {
			t.Fatalf("长度 %d 的估算值 %d 小于更短文本的 %d", length, current, previous)
		}
		previous = current
	}
}

// 请求里所有会发出去的东西都要算上：消息正文、工具调用参数、工具定义。
// 漏算任何一项都会让估算系统性偏低。
func TestEstimateRequestTokensCountsToolsAndArguments(t *testing.T) {
	base := domain.ModelRequest{
		Messages: []domain.ModelMessage{{Role: domain.ModelRoleUser, Content: "你好"}},
	}
	withCall := domain.ModelRequest{
		Messages: []domain.ModelMessage{{
			Role: domain.ModelRoleAssistant,
			ToolCalls: []domain.ToolCall{{
				ID: "call-1", Name: "bash",
				Arguments: json.RawMessage(`{"command":"ls -la /usr/bin"}`),
			}},
		}},
	}
	withTools := domain.ModelRequest{
		Messages: base.Messages,
		Tools: []domain.ToolSpec{{
			Name: "bash", Description: "在用户本机运行一条命令",
			Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		}},
	}

	baseTokens := domain.EstimateRequestTokens(base)
	if domain.EstimateRequestTokens(withCall) <= baseTokens {
		t.Error("工具调用的参数没有被算进去")
	}
	if domain.EstimateRequestTokens(withTools) <= baseTokens {
		t.Error("工具定义没有被算进去")
	}
}

// 剩余容量和占用比例是方法而不是字段，因此不存在"字段与原始量不一致"的可能。
// 这几条钉住它们的边界行为。
func TestContextUsageDerivedValues(t *testing.T) {
	t.Run("正常情况", func(t *testing.T) {
		usage := domain.ContextUsage{ContextWindow: 1000, InputTokens: 250, Source: domain.ContextUsageEstimated}
		if usage.Remaining() != 750 {
			t.Errorf("Remaining = %d; want 750", usage.Remaining())
		}
		if usage.Ratio() != 0.25 {
			t.Errorf("Ratio = %v; want 0.25", usage.Ratio())
		}
	})

	// 估算偏高时输入可能超过窗口。剩余返回 0 而不是负数——"剩余 -300"没有意义。
	t.Run("超出窗口", func(t *testing.T) {
		usage := domain.ContextUsage{ContextWindow: 1000, InputTokens: 1300, Source: domain.ContextUsageEstimated}
		if usage.Remaining() != 0 {
			t.Errorf("Remaining = %d; want 0", usage.Remaining())
		}
		// 比例允许大于 1：显示成"超过 100%"比悄悄截断更诚实，那正是要压缩的信号。
		if usage.Ratio() <= 1 {
			t.Errorf("Ratio = %v; want 大于 1", usage.Ratio())
		}
	})

	// 窗口未配置时算不出比例，界面显示"未知"而不是编数字。
	t.Run("窗口未知", func(t *testing.T) {
		usage := domain.ContextUsage{InputTokens: 500, Source: domain.ContextUsageUnknown}
		if usage.Known() {
			t.Error("没有窗口时 Known 应为 false")
		}
		if usage.Remaining() != 0 || usage.Ratio() != 0 {
			t.Errorf("窗口未知时派生值 = %d / %v; want 0 / 0", usage.Remaining(), usage.Ratio())
		}
	})
}

// 供应商实测值覆盖估算值，并把来源标成 provider——界面据此说明这个数字有多可信。
func TestWithProviderTokensOverridesTheEstimate(t *testing.T) {
	estimated := domain.ContextUsage{
		ContextWindow: 1000, InputTokens: 300, Source: domain.ContextUsageEstimated,
	}

	measured := estimated.WithProviderTokens(420, 0, 420)

	if measured.InputTokens != 420 || measured.Source != domain.ContextUsageProvider {
		t.Errorf("实测后 = %+v", measured)
	}
	// 返回副本而不是就地改：这份占用会被放进事件 payload 交给多个消费者，
	// 就地改会让已经发出去的那份跟着变。
	if estimated.InputTokens != 300 || estimated.Source != domain.ContextUsageEstimated {
		t.Errorf("原始的估算值被改动了: %+v", estimated)
	}
}

// payload 把派生值算好一并发出：前端拿到的是 JSON，方法过不去，
// 让它自己重算规则等于把规则散到消费端。
func TestContextUsagePayloadCarriesDerivedValues(t *testing.T) {
	payload := domain.NewContextUsagePayload(domain.ContextUsage{
		ContextWindow: 1000, InputTokens: 250, Source: domain.ContextUsageProvider,
	})

	if payload.Remaining != 750 || payload.Ratio != 0.25 {
		t.Errorf("payload = %+v", payload)
	}
	if payload.Source != "provider" {
		t.Errorf("Source = %q", payload.Source)
	}
}

// 安全系数让请求级估算落在"分项之和"之上。方向是刻意的：高估只让压缩早触发
// 一次，低估会让请求被供应商直接拒。
func TestEstimateRequestTokensLeansHigh(t *testing.T) {
	request := domain.ModelRequest{
		Messages: []domain.ModelMessage{
			{Role: domain.ModelRoleSystem, Content: strings.Repeat("指令", 200)},
			{Role: domain.ModelRoleUser, Content: strings.Repeat("问题", 50)},
		},
	}

	// 不含安全系数与固定开销时的纯文本量。
	plain := domain.EstimateTokens(request.Messages[0].Content) +
		domain.EstimateTokens(request.Messages[1].Content)

	if got := domain.EstimateRequestTokens(request); got <= plain {
		t.Errorf("请求级估算 %d 没有高于纯文本量 %d", got, plain)
	}
}

// —— M5.3：估算的契约是"上界" ——

// 上界被击穿时，新系数必须足以覆盖这次击穿，而且要多留一点余量——
// 只补到刚好等于实测值，下一次内容构成稍有不同就会再次击穿，变成反复告警。
func TestRaisedSafetyFactorCoversTheBreachWithMargin(t *testing.T) {
	breach := domain.EstimateBreach{Estimated: 1000, Actual: 1200, UsedFactor: 1.15}

	raised := domain.RaisedSafetyFactor(1.15, breach)

	if raised <= 1.15 {
		t.Fatalf("系数没有调高：%.3f", raised)
	}
	// 用新系数重估这次的内容，必须已经盖过实测值。
	// 原估算 1000 是在 1.15 下算出来的，所以基数是 1000/1.15。
	reestimated := 1000 / 1.15 * raised
	if reestimated <= 1200 {
		t.Errorf("调高之后仍然盖不住实测值：%.0f vs 1200", reestimated)
	}
}

// 系数只调高不调低——调低要有"上界一直很宽裕"的长期证据，而收益只是省一点上下文。
func TestSafetyFactorIsNeverLowered(t *testing.T) {
	// 实测远低于估算（上界很宽裕）时也不该往下调。
	generous := domain.EstimateBreach{Estimated: 1000, Actual: 100, UsedFactor: 1.15}

	if raised := domain.RaisedSafetyFactor(1.15, generous); raised < 1.15 {
		t.Errorf("系数被调低到 %.3f", raised)
	}
}

// 估算为 0 时比值没有意义，不能拿它去乘系数。
func TestBreachRatioHandlesZeroEstimate(t *testing.T) {
	if ratio := (domain.EstimateBreach{Estimated: 0, Actual: 500}).Ratio(); ratio != 0 {
		t.Errorf("Ratio() = %v; want 0", ratio)
	}
	if raised := domain.RaisedSafetyFactor(1.15, domain.EstimateBreach{Estimated: 0, Actual: 500}); raised != 1.15 {
		t.Errorf("估算为 0 时系数被改成了 %.3f", raised)
	}
}

// 安全系数不能被压到 1 以下——那等于承认估算可以低于实际。
func TestEstimateNeverUsesAFactorBelowOne(t *testing.T) {
	request := domain.ModelRequest{Messages: []domain.ModelMessage{{Role: domain.ModelRoleUser, Content: "一段中文内容"}}}

	atOne := domain.EstimateRequestTokensWith(request, 1.0)
	below := domain.EstimateRequestTokensWith(request, 0.1)

	if below != atOne {
		t.Errorf("系数 0.1 得到 %d，系数 1.0 得到 %d；应当被夹到 1.0", below, atOne)
	}
}

// 同样的请求，系数越大估得越多——这是"上界可以被调紧"的基础。
func TestEstimateGrowsWithSafetyFactor(t *testing.T) {
	request := domain.ModelRequest{Messages: []domain.ModelMessage{{Role: domain.ModelRoleUser, Content: "一段足够长的中文内容用来估算"}}}

	if domain.EstimateRequestTokensWith(request, 1.5) <= domain.EstimateRequestTokensWith(request, 1.15) {
		t.Error("调高系数之后估算没有变大")
	}
}

// 缓存数据只在供应商真的报了的时候才算数。
//
// 判据是"命中 + 未命中 == 总输入"而不是"两者是否为 0"：**0 命中是一个有意义的值**
// ——一次压缩刚换掉整条前缀时就正好是 0，而那恰恰是最值得看的一次。用"是否为 0"
// 判断有没有数据，会把这一次误当成"供应商没报"而丢掉。
func TestCacheDataIsOnlyKnownWhenItAddsUp(t *testing.T) {
	base := domain.ContextUsage{ContextWindow: 128000}

	cases := []struct {
		name             string
		hit, miss, total int
		wantKnown        bool
		wantRatio        float64
	}{
		{"供应商没报", 0, 0, 1000, false, 0},
		{"全部命中", 1000, 0, 1000, true, 1},
		{"全部未命中（压缩刚换掉前缀）", 0, 1000, 1000, true, 0},
		{"部分命中", 900, 100, 1000, true, 0.9},
		{"对不上，当成没报", 500, 100, 1000, false, 0},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			usage := base.WithProviderTokens(testCase.total, testCase.hit, testCase.miss)
			if usage.CacheKnown() != testCase.wantKnown {
				t.Errorf("CacheKnown() = %v; want %v", usage.CacheKnown(), testCase.wantKnown)
			}
			if got := usage.CacheHitRatio(); got != testCase.wantRatio {
				t.Errorf("CacheHitRatio() = %v; want %v", got, testCase.wantRatio)
			}
		})
	}
}

// 没有缓存数据时，payload 里干脆不出现那几个字段，而不是给一串 0。
//
// 给 0 的话，界面无法区分"这次全部未命中"和"这个供应商不支持缓存"——
// 前者是需要注意的信号，后者只是一个不适用的字段。
func TestUsagePayloadOmitsCacheFieldsWhenUnknown(t *testing.T) {
	usage := domain.ContextUsage{
		ContextWindow: 128000, InputTokens: 1000, Source: domain.ContextUsageEstimated,
	}

	encoded, err := json.Marshal(domain.NewContextUsagePayload(usage))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{"cache_hit_tokens", "cache_miss_tokens", "cache_hit_ratio"} {
		if strings.Contains(string(encoded), field) {
			t.Errorf("没有缓存数据却带上了 %s: %s", field, encoded)
		}
	}
}
