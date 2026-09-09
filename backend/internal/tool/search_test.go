package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// searchSession 造一个内容各异的会话，用来验各种检索路径。
//
// 刻意让同一个标识符只出现在**一个**角色的消息里：这样 role 过滤是不是真的生效
// 一眼可判，而不是靠"结果条数变少了"这种间接证据。
func searchSession(t *testing.T) *domain.Session {
	t.Helper()
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	turn := func(index int) domain.TurnID { return domain.TurnID(fmt.Sprintf("trn_%032d", index)) }

	session.Append(domain.Message{
		Role: domain.RoleUser, TurnID: turn(0),
		Content: "部署密钥编号是 QX-7731，记住它",
	})
	session.Append(domain.Message{
		Role: domain.RoleAssistant, TurnID: turn(0),
		Content: "记住了。接下来我去看看配置。",
		ToolCalls: []domain.ToolCall{{
			ID: "call-1", Name: "bash",
			Arguments: json.RawMessage(`{"command":"ls -la /etc/deploy"}`),
		}},
	})
	session.Append(domain.Message{
		Role: domain.RoleTool, ToolCallID: "call-1", TurnID: turn(0),
		Content: "total 8\ndrwxr-xr-x  keys.pem\n-rw-------  EWOULDBLOCK.log",
	})
	session.Append(domain.Message{
		Role: domain.RoleUser, TurnID: turn(1),
		Content: "改成数据库模式吧",
	})
	session.Append(domain.Message{
		Role: domain.RoleAssistant, TurnID: turn(1),
		Content: "好的，我改成 SQLite。",
	})

	session.Memory = domain.ConversationMemory{
		RawCompactionCursor: 3,
		Batches: []domain.MemoryBatch{{
			ID: "mem_0000000000000000000000000000000a", Level: 0,
			Content:           "主题：部署密钥与目录检查\n做成了什么：确认了部署目录的内容。",
			StartMessageIndex: 0, EndMessageIndex: 3,
		}},
		ActiveBatchIDs: []domain.MemoryBatchID{"mem_0000000000000000000000000000000a"},
	}
	return session
}

// search 找到的是**确切的字符串**——这正是摘要装不下、而向量检索也做不好的那一类。
func TestSearchFindsAnExactIdentifier(t *testing.T) {
	result := runHistory(t, searchSession(t), `{"action":"search","query":"QX-7731"}`)

	if result.Status != domain.ToolSuccess {
		t.Fatalf("status = %q: %s", result.Status, result.Content)
	}
	if !strings.Contains(result.Content, "#1") {
		t.Errorf("没有给出命中的序号:\n%s", result.Content)
	}
	// 结果里要直接告诉模型下一步怎么走。检索原语和读取原语必须能串起来，
	// 否则模型拿到序号也不知道能拿它做什么。
	if !strings.Contains(result.Content, "read from=") {
		t.Errorf("没有指出用 read from=<序号> 继续:\n%s", result.Content)
	}
}

// 搜工具调用的参数：模型问"那条 ls 命令的完整参数是什么"时，字符串在 arguments 的
// JSON 里而不在 Content 里。漏掉它，最常见的一类检索需求直接失效。
func TestSearchLooksInsideToolCallArguments(t *testing.T) {
	result := runHistory(t, searchSession(t), `{"action":"search","query":"/etc/deploy"}`)

	if !strings.Contains(result.Content, "#2") {
		t.Errorf("没有搜到工具调用参数里的路径:\n%s", result.Content)
	}
}

// 三种 role 过滤都要生效。
func TestSearchFiltersByRole(t *testing.T) {
	session := searchSession(t)
	cases := []struct {
		role      string
		query     string
		wantHit   bool
		wantEmpty string
	}{
		{"user", "QX-7731", true, ""},
		{"user", "EWOULDBLOCK", false, "没有找到"},
		{"tool", "EWOULDBLOCK", true, ""},
		{"tool", "QX-7731", false, "没有找到"},
		{"assistant", "SQLite", true, ""},
		{"assistant", "QX-7731", false, "没有找到"},
	}

	for _, testCase := range cases {
		t.Run(testCase.role+"/"+testCase.query, func(t *testing.T) {
			arguments := fmt.Sprintf(`{"action":"search","query":%q,"role":%q}`,
				testCase.query, testCase.role)
			result := runHistory(t, session, arguments)
			if result.Status != domain.ToolSuccess {
				t.Fatalf("status = %q: %s", result.Status, result.Content)
			}
			if testCase.wantHit && strings.Contains(result.Content, "没有找到") {
				t.Errorf("本该命中却没有:\n%s", result.Content)
			}
			if !testCase.wantHit && !strings.Contains(result.Content, testCase.wantEmpty) {
				t.Errorf("本该落空却命中了:\n%s", result.Content)
			}
		})
	}
}

// 未命中要明确说"没有"，而不是给一段空结果——模型据此换个词再搜，
// 而不是以为工具坏了。
func TestSearchSaysNothingFoundClearly(t *testing.T) {
	result := runHistory(t, searchSession(t), `{"action":"search","query":"根本不存在的东西"}`)

	if result.Status != domain.ToolSuccess {
		t.Fatalf("未命中不该是 error：%s", result.Content)
	}
	if !strings.Contains(result.Content, "没有找到") {
		t.Errorf("没有明说未命中:\n%s", result.Content)
	}
	// 未命中时提示另一个 scope：那是模型最可能想不起来的入口。
	if !strings.Contains(result.Content, "summaries") {
		t.Errorf("未命中时没有提示可以搜摘要:\n%s", result.Content)
	}
}

// scope=summaries 搜摘要正文。
//
// 这是"语义检索"在本系统里最便宜的形态：摘要只有几十条，而且是自然语言写的语义
// 压缩，由将要使用它的同一个模型写的。
func TestSearchCanLookInSummaries(t *testing.T) {
	result := runHistory(t, searchSession(t),
		`{"action":"search","query":"部署目录","scope":"summaries"}`)

	if !strings.Contains(result.Content, "mem_0000000000000000000000000000000a") {
		t.Errorf("没有搜到摘要正文:\n%s", result.Content)
	}
	// 原始消息里没有"部署目录"这四个字，所以 raw 应当落空——两个 scope 确实分开。
	raw := runHistory(t, searchSession(t), `{"action":"search","query":"部署目录"}`)
	if !strings.Contains(raw.Content, "没有找到") {
		t.Errorf("scope=raw 竟然也命中了摘要正文:\n%s", raw.Content)
	}
}

// context 参数让命中处带上前后几条，且命中那条要标出来。
func TestSearchCanCarryContextAroundTheHit(t *testing.T) {
	result := runHistory(t, searchSession(t),
		`{"action":"search","query":"EWOULDBLOCK","context":1}`)

	// 命中在 #3，带一条上下文应当同时出现 #2 和 #4。
	for _, want := range []string{"#2", "#3", "#4"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("上下文里缺 %s:\n%s", want, result.Content)
		}
	}
	// 带了上下文之后必须能分清哪一条是命中的。
	if !strings.Contains(result.Content, "→ #3") {
		t.Errorf("命中那条没有标出来:\n%s", result.Content)
	}
}

// 命中太多时按 limit 截断，并**说明截断了**。
//
// 不说明的话，模型会把"前 5 条"当成"全部"，据此断定某个东西只出现过 5 次。
func TestSearchTruncatesAndSaysSo(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	for index := range 30 {
		session.Append(domain.Message{
			Role: domain.RoleUser, TurnID: domain.TurnID(fmt.Sprintf("trn_%032d", index)),
			Content: "重复出现的关键词",
		})
	}

	result := runHistory(t, session, `{"action":"search","query":"关键词","limit":3}`)

	if !strings.Contains(result.Content, "找到 30 处") {
		t.Errorf("没有报出总命中数:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "只列出前 3 处") {
		t.Errorf("截断了却没有说明:\n%s", result.Content)
	}
}

// search 只搜当前会话。参数里没有 session_id，也没有任何路径。
func TestSearchCannotReachAnotherSession(t *testing.T) {
	result := runHistory(t, searchSession(t),
		`{"action":"search","query":"x","session_id":"ses_ffffffffffffffffffffffffffffffff"}`)

	if result.Status != domain.ToolError {
		t.Fatalf("带 session_id 竟然被接受了：%s", result.Content)
	}
	if !strings.Contains(result.Content, "不符合") {
		t.Errorf("说明里没有指出参数不合法:\n%s", result.Content)
	}
}

// read 的两个入口都要工作，而且能和 search 的输出直接对接。
//
// 这是 M5.1 修掉的一处**设计失误**：原来 read 只接受 {batch_id, offset}，
// offset 相对某个节点；而 search 返回的是绝对序号。模型拿到 #127 之后得先猜哪个
// batch 覆盖它——两个原语串不起来，各自再好用也没有意义。
func TestReadAcceptsAbsoluteMessageNumbers(t *testing.T) {
	session := searchSession(t)

	// 先搜到序号。
	found := runHistory(t, session, `{"action":"search","query":"数据库模式"}`)
	if !strings.Contains(found.Content, "#4") {
		t.Fatalf("没有搜到预期的序号:\n%s", found.Content)
	}

	// 再按那个序号读它周围。
	read := runHistory(t, session, `{"action":"read","from":4,"count":2}`)
	if read.Status != domain.ToolSuccess {
		t.Fatalf("status = %q: %s", read.Status, read.Content)
	}
	if !strings.Contains(read.Content, "改成数据库模式吧") {
		t.Errorf("没有读到第 4 条:\n%s", read.Content)
	}
	if !strings.Contains(read.Content, "SQLite") {
		t.Errorf("没有读到第 5 条:\n%s", read.Content)
	}
}

// 按序号读的边界：越界要说清楚范围，而不是返回空结果。
func TestReadRangeReportsOutOfBounds(t *testing.T) {
	cases := []string{
		`{"action":"read","from":0}`,
		`{"action":"read","from":999}`,
	}
	for _, arguments := range cases {
		result := runHistory(t, searchSession(t), arguments)
		if result.Status != domain.ToolError {
			t.Fatalf("%s 竟然成功了：%s", arguments, result.Content)
		}
		if !strings.Contains(result.Content, "共 5 条消息") {
			t.Errorf("没有告诉模型合法范围:\n%s", result.Content)
		}
	}
}

// read 按序号读到末尾时不越界，并说明后面还有没有。
func TestReadRangeStopsAtTheEnd(t *testing.T) {
	result := runHistory(t, searchSession(t), `{"action":"read","from":5,"count":10}`)

	if result.Status != domain.ToolSuccess {
		t.Fatalf("status = %q: %s", result.Status, result.Content)
	}
	if strings.Contains(result.Content, "还有消息") {
		t.Errorf("已经读到末尾却说后面还有:\n%s", result.Content)
	}
}

// search 的返回量必须与被搜消息的体量**无关**——它报位置，不报正文。
//
// 这条是真机跑出来的级联爆炸逼出来的：上一版把整条消息压成一行返回，一次
// `search "ls -laR" role=tool` 返回了 2.53 MB，而且**自我放大**——search 的结果本身
// 成为一条 tool 消息，正文里带着查询词，于是被下一次 search 匹配到并整条捞回来。
// 最后连终结性全塌都发不出去了（它的源文本就是那 136 万 token 的保留区）。
func TestSearchResultSizeDoesNotScaleWithMessageSize(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "列一下目录"})
	// 一条巨大的工具观察：50000 行，其中只有一行含目标字符串。
	var huge strings.Builder
	for index := range 50000 {
		fmt.Fprintf(&huge, "-rw-r--r--  1 root wheel  %d  file%d.txt\n", index*13, index)
	}
	huge.WriteString("drwxr-xr-x  2 root wheel  64  QX-7731-secret\n")
	session.Append(domain.Message{
		Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: huge.String(),
	})

	result := runHistory(t, session, `{"action":"search","query":"QX-7731"}`)

	if result.Status != domain.ToolSuccess {
		t.Fatalf("status = %q: %s", result.Status, result.Content)
	}
	// 命中要报出来。
	if !strings.Contains(result.Content, "QX-7731-secret") {
		t.Errorf("匹配的那一行没有返回:\n%s", result.Content[:min(400, len(result.Content))])
	}
	// 但返回量绝不能和那条消息的体量成正比。被搜的消息约 2.5 MB。
	if len(result.Content) > 4000 {
		t.Errorf("返回了 %d 字节；被搜消息 %d 字节——search 在返回整条消息，"+
			"这会造成级联爆炸", len(result.Content), len(huge.String()))
	}
	// 要报出那条有多大，模型才能在 read 之前判断值不值得读。
	if !strings.Contains(result.Content, "字节") {
		t.Errorf("没有报出命中消息的体量:\n%s", result.Content)
	}
}

// 同一条消息里多行匹配时，**每一行都完整给出**——这不是截断。
func TestSearchReturnsAllMatchingLinesInFull(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "跑一下"})
	session.Append(domain.Message{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1",
		Content: "无关的一行\n第一处 TARGET 在这里，这一行有一些额外的内容需要完整保留\n中间无关\n第二处 TARGET 也在\n结尾无关"})

	result := runHistory(t, session, `{"action":"search","query":"TARGET"}`)

	for _, want := range []string{
		"第一处 TARGET 在这里，这一行有一些额外的内容需要完整保留",
		"第二处 TARGET 也在",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("匹配行缺失或被裁: %q\n%s", want, result.Content)
		}
	}
	// 不匹配的行不该出现——那是 read 的职责。
	if strings.Contains(result.Content, "无关的一行") {
		t.Errorf("返回了不匹配的行，那是 read 的活:\n%s", result.Content)
	}
}

// context 带出的邻居只给导航信息，不给正文。
func TestSearchContextGivesNavigationNotContent(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "问题"})
	session.Append(domain.Message{Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1",
		Content: "邻居的首行\n" + strings.Repeat("邻居的大量正文\n", 20000)})
	session.Append(domain.Message{Role: domain.RoleAssistant, TurnID: "t1",
		Content: "这里有 NEEDLE"})

	result := runHistory(t, session, `{"action":"search","query":"NEEDLE","context":1}`)

	// 邻居要出现（导航），但只给首行与体量。
	if !strings.Contains(result.Content, "#2") || !strings.Contains(result.Content, "邻居的首行") {
		t.Errorf("邻居的导航信息缺失:\n%s", result.Content)
	}
	if len(result.Content) > 4000 {
		t.Errorf("带 context 之后返回了 %d 字节——邻居的正文被整条塞进来了", len(result.Content))
	}
}

// search 的结果不该让下一次 search 变得更大——上一版那个正反馈回路。
func TestSearchIsNotSelfAmplifying(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "跑 ls -laR"})
	for index := range 6 {
		session.Append(domain.Message{
			Role: domain.RoleTool, TurnID: "t1", ToolCallID: fmt.Sprintf("c%d", index),
			// 每条都很大，且都含查询词——最容易触发放大的形状。
			Content: strings.Repeat("ls -laR 的一段输出\n", 8000),
		})
	}

	result := runHistory(t, session, `{"action":"search","query":"ls -laR","role":"tool"}`)

	// 六条各约 150 KB，合计约 900 KB。返回量必须与它无关。
	if len(result.Content) > 8000 {
		t.Errorf("返回 %d 字节；被搜的六条合计约 900 KB——放大回路还在", len(result.Content))
	}
	if !strings.Contains(result.Content, "找到 6 处") {
		t.Errorf("没有如实报出命中数:\n%s", result.Content[:min(300, len(result.Content))])
	}
}

// 超大观察可以**一页页读完整**——这是"截断"和"分页"的分界。
//
// 视图层把超大观察收成头 + 标注 + 尾，标注指向 read from=N。如果 read 只能整条
// 返回，那个指向就是死路（2.5 MB 读回来就是炸掉上下文）。有了行区间，存储零截断 +
// 视图有界 + 按需分页三件事才凑齐。
func TestReadPaginatesWithinOneMessage(t *testing.T) {
	session := domain.NewSession("ses_0123456789abcdef0123456789abcdef", "/tmp")
	session.Append(domain.Message{Role: domain.RoleUser, TurnID: "t1", Content: "列目录"})
	var huge strings.Builder
	for index := 1; index <= 5000; index++ {
		fmt.Fprintf(&huge, "第 %d 行的内容\n", index)
	}
	session.Append(domain.Message{
		Role: domain.RoleTool, TurnID: "t1", ToolCallID: "c1", Content: huge.String(),
	})

	first := runHistory(t, session, `{"action":"read","from":2,"lines":"1-100"}`)
	if first.Status != domain.ToolSuccess {
		t.Fatalf("status = %q: %s", first.Status, first.Content)
	}
	// 报出总量：模型才知道自己在哪、还剩多少。
	if !strings.Contains(first.Content, "5002 行") && !strings.Contains(first.Content, "500") {
		t.Errorf("没有报出总行数:\n%s", first.Content[:200])
	}
	// 区间内的行完整给出。
	if !strings.Contains(first.Content, "第 50 行的内容") {
		t.Errorf("区间内的行缺失:\n%s", first.Content[:300])
	}
	// 区间外的不给。
	if strings.Contains(first.Content, "第 4000 行的内容") {
		t.Error("返回了区间之外的行")
	}
	// 直接给出下一页的参数，而不是让模型自己推算。
	if !strings.Contains(first.Content, `lines="101-`) {
		t.Errorf("没有给出下一页的参数:\n%s", first.Content[len(first.Content)-200:])
	}

	// 翻到最后一页要明说读完了，否则模型会一直翻。
	last := runHistory(t, session, `{"action":"read","from":2,"lines":"4900-9999"}`)
	if !strings.Contains(last.Content, "已经读完") {
		t.Errorf("读到末尾没有说明:\n%s", last.Content[len(last.Content)-200:])
	}
	// 终点越界只夹回来，不报错——模型写 "1-9999" 表达"从头读一大段"是合理的。
	if last.Status != domain.ToolSuccess {
		t.Errorf("终点越界被当成了错误: %s", last.Content)
	}
}

// 行区间的解析：合法与四类非法。
func TestLineRangeParsing(t *testing.T) {
	cases := []struct {
		spec                string
		total               int
		wantFirst, wantLast int
		wantErr             bool
	}{
		{"1-200", 5000, 1, 200, false},
		{"7", 5000, 7, 7, false},           // 单个数字等价于 "7-7"
		{" 10 - 20 ", 5000, 10, 20, false}, // 容忍空格
		{"1-9999", 500, 1, 500, false},     // 终点越界只夹回来
		{"0-10", 5000, 0, 0, true},         // 起点必须从 1 开始
		{"600-700", 500, 0, 0, true},       // 起点超过总行数
		{"200-100", 5000, 0, 0, true},      // 终点小于起点
		{"abc", 5000, 0, 0, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.spec, func(t *testing.T) {
			first, last, err := parseLineRange(testCase.spec, testCase.total)
			if testCase.wantErr {
				if err == nil {
					t.Errorf("parseLineRange(%q,%d) 没有报错", testCase.spec, testCase.total)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if first != testCase.wantFirst || last != testCase.wantLast {
				t.Errorf("= %d,%d; want %d,%d", first, last, testCase.wantFirst, testCase.wantLast)
			}
		})
	}
}
