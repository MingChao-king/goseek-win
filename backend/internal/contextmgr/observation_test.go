package contextmgr

import (
	"fmt"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// 超大工具观察在视图层收边界，而**存储一个字节没少**。
//
// 这是与工具层截断的关键区别：上一版在工具层截断，被挖掉的部分从此不在库里，
// 模型没有任何追索路径——那才是"1173 个 man 页面"那类假事实的成因。
func TestHugeObservationIsBoundedInTheViewOnly(t *testing.T) {
	var huge strings.Builder
	for index := 1; index <= 20000; index++ {
		fmt.Fprintf(&huge, "-rw-r--r--  1 root wheel  %d  file%d.txt\n", index*13, index)
	}
	original := huge.String()

	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "列目录"},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: original},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)

	view := buildMessages(session.Messages(), domain.ConversationMemory{}, nil)

	// 存储不动。
	if session.Messages()[1].Content != original {
		t.Error("存储被改动了——视图层收边界不该碰历史")
	}
	// 视图有界。
	rendered := view[len(view)-1].Content
	if len(rendered) > maxObservationChars+600 {
		t.Errorf("视图里这条渲染成 %d 字节；上限 %d", len(rendered), maxObservationChars)
	}
	// 头尾都在：失败原因常在 stderr 的最后几行，只留头部会丢掉它。
	if !strings.Contains(rendered, "file1.txt") {
		t.Error("头部丢了")
	}
	if !strings.Contains(rendered, "file20000.txt") {
		t.Error("尾部丢了")
	}
	// 标注必须同时有总量、序号、取回命令——缺一样就退化成有害截断。
	for _, want := range []string{
		fmt.Sprintf("%d 字节", len(original)), // 总量
		"20000 行",                           // 总行数
		"read from=2",                       // 序号 + 取回命令
		"lines=",                            // 分页入口
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("标注里缺 %q:\n%s", want, rendered[len(rendered)/2-200:len(rendered)/2+400])
		}
	}
}

// 只收工具观察的边界。用户消息与模型输出不收。
//
// 实测用户消息只占全部内容的 0.4%–0.8%，工具观察占 84%–95.6%——收边界要收在占地方
// 的那一类上，而不是收在唯一无法重新推导的那一类上。
func TestOnlyToolObservationsAreBounded(t *testing.T) {
	long := strings.Repeat("用户贴进来的一大段内容。\n", 5000)
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: long},
		{Role: domain.RoleAssistant, TurnID: "t1", Content: long},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)

	view := buildMessages(session.Messages(), domain.ConversationMemory{}, nil)

	for index, message := range view[1:] {
		if message.Content != long {
			t.Errorf("第 %d 条（%s）被收了边界，它不该被收", index+1, message.Role)
		}
	}
}

// 不超限的观察一个字都不动。
func TestSmallObservationIsUntouched(t *testing.T) {
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "跑一下"},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: "a.txt\nb.txt\n"},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)

	view := buildMessages(session.Messages(), domain.ConversationMemory{}, nil)

	if view[len(view)-1].Content != "a.txt\nb.txt\n" {
		t.Errorf("小观察被改动了: %q", view[len(view)-1].Content)
	}
}

// 按字节切必然切在多字节字符中间，残缺字节要去掉。
//
// 乱码是模型唯一无法判断"这是内容还是故障"的东西。
func TestBoundedObservationIsValidUTF8(t *testing.T) {
	// 全中文，保证 headLimit 那一刀必然落在某个三字节字符中间。
	content := strings.Repeat("这是一段中文输出内容。", 6000)
	messages := []domain.Message{
		{Role: domain.RoleUser, TurnID: "t1", Content: "跑"},
		{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: content},
	}
	session := domain.LoadSession("ses_0123456789abcdef0123456789abcdef", "/tmp", messages)

	rendered := buildMessages(session.Messages(), domain.ConversationMemory{}, nil)
	body := rendered[len(rendered)-1].Content
	if !utf8ValidString(body) {
		t.Error("渲染结果不是合法 UTF-8——按字节切之后没有清掉残缺字符")
	}
}

// utf8ValidString 是 utf8.ValidString 的一层包装，让断言读起来像话。
func utf8ValidString(text string) bool {
	for _, r := range text {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// 摘要器的源文本也必须按视图口径收边界。
//
// 两条理由：不然兜底自己会发不出去（真机上出现过 136 万 token 的源文本，超过了
// 模型的真实窗口）；而且摘要应该总结"模型当时看到的东西"——视图给的是头 + 尾，
// 拿完整原文去总结会让摘要提到模型从没见过的内容。
func TestSummarizerSourceIsAlsoBounded(t *testing.T) {
	var huge strings.Builder
	for index := 1; index <= 30000; index++ {
		fmt.Fprintf(&huge, "第 %d 行，这一行有一些内容占位\n", index)
	}

	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	// 三轮：前一轮可压（游标之前），后两轮是保留区。
	for turn := range 3 {
		id := fmt.Sprintf("trn_%032d", turn)
		session.Append(domain.Message{Role: domain.RoleUser, TurnID: domain.TurnID(id),
			Content: fmt.Sprintf("第 %d 轮", turn)})
		session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: domain.TurnID(id),
			ToolCalls: []domain.ToolCall{{ID: fmt.Sprintf("c%d", turn), Name: "bash"}}})
		session.Append(domain.Message{Role: domain.RoleTool, TurnID: domain.TurnID(id),
			ToolCallID: fmt.Sprintf("c%d", turn), Content: huge.String()})
	}

	summarizer := &fakeSummarizer{}
	compactor := newTestCompactor(summarizer, 128000)
	if _, err := compactor.Compact(t.Context(), session, domain.ConversationMemory{}); err != nil {
		t.Fatalf("Compact 返回错误: %v", err)
	}
	if len(summarizer.sources) == 0 {
		t.Fatal("没有发出摘要请求，这条测试没测到东西")
	}

	for index, source := range summarizer.sources {
		// 一条观察 30000 行约 900 KB；源文本里若出现完整的它，说明没走 boundedContent。
		if len(source) > 4*maxObservationChars {
			t.Errorf("第 %d 次摘要请求的源文本 %d 字节——超大观察没有收边界",
				index+1, len(source))
		}
		// 收了边界就必须带上取回入口，否则模型无从追索。
		if strings.Contains(source, "中间省略") && !strings.Contains(source, "read from=") {
			t.Errorf("第 %d 次源文本收了边界却没给取回入口", index+1)
		}
	}
}
