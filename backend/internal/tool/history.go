package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"goseek/internal/domain"
)

// HistoryTool 让模型回查被压缩掉的原始对话。
//
// # 它为什么存在
//
// 压缩之后，模型在上下文里看到的是摘要——一段有损的自然语言概括。大多数时候够用，
// 但有些事情摘要天然装不下：某条命令的精确写法、某个报错的原文、某个路径的确切
// 拼写。没有回查手段的话，模型只能凭摘要里的印象去猜，而猜错一个路径就是一次
// 失败的命令。
//
// 有了它，压缩才是"安全"的：信息没有丢，只是移到了需要时再取的地方。
//
// # 三个动作
//
//	search   在全部消息里找一个确切的字符串，返回命中处的序号与上下文
//	read     读原始消息。可以按节点（batch_id）读，也可以按绝对序号（from/count）读
//	inspect  给一个节点 ID，返回它的层级、覆盖范围、摘要首行，以及**直接**子节点
//
// # 为什么 search 是主力，inspect 是备用
//
// **问题不是树的深度，是索引信息在压缩中丢失。** 沿树 inspect 的前提是模型能从
// 上层摘要判断出"我要的东西在这个分支里"；而用户问"当时那个门禁码是多少"时，
// 压过 10 倍的父摘要里可能根本没有"门禁码"三个字——模型不知道该不该往下走。
//
// 所以职责这样分：
//
//	派生的用户原话（带区间）   定位：哪一轮问的、在第几条   零模型调用   精确
//	摘要散文（七节）          定位：没提过的事在哪一段     压缩时付清   语义、粗
//	search                   召回：那个确切的字符串在哪    **零模型调用**  精确
//	read                     原文：逐条读出来             零模型调用   完整
//
// **摘要负责 orientation，search 负责 recall。** 摘要粗一点可以接受，只要召回不
// 依赖它。这条分工同时化解了一个逃不掉的权衡：总模型调用次数正比于"会话总长度 ÷
// 每个叶子的跨度"——想要细粒度就得多花调用，除非把"召回"从模型身上拿掉。
//
// inspect 只返回直接子节点，不展开整棵树——一次全展开等于把历史重新灌回上下文，
// 压缩就白做了。
//
// # 为什么不引入向量库
//
// 最强的一条：**我们需要的是精确召回，不是语义召回。** 摘要丢掉的是 ZZ-8842、
// --follow-tags、EWOULDBLOCK 这类东西，而嵌入向量恰好最不擅长这个——它把 QX-7731
// 和 QX-7732 映射到几乎同一个点。子串搜索一击即中。
//
// 其次：一个会话几百到几千条消息，向量检索的用武之地是 10⁵–10⁹ 篇文档，在 10³
// 这个量级全表扫描比建索引快而且不会错；它的失败模式是"自信地返回不相关的东西"
// （永远返回 top-k），对一个会照着结果去执行命令的 Agent 比"没找到"危险得多；
// 还要引入嵌入端点、另一份成本、另一种失败模式，模型换版本索引就得重建。
//
// 而语义索引其实已经有了，就是摘要本身——由**将要使用它的同一个模型**写的，
// 比一个独立的嵌入空间更对齐。
//
// # 边界
//
// 数据源固定绑定当前会话，参数里**没有 session_id 也没有文件路径**。模型无法用
// 它读别的会话，也无法用它读任何文件——那是 bash 的事，而 bash 有它自己的边界。
type HistoryTool struct {
	// session 是数据源。它是个指针，因此工具看到的永远是当前最新的历史——
	// 一轮之内历史会不断增长，拿快照会让后半轮读到过期数据。
	session *domain.Session
}

const (
	// historyDefaultLimit 是 read 默认返回的消息条数。
	historyDefaultLimit = 10
	// historyMaxLimit 是 read 单次允许的最大条数。
	//
	// 设上限是因为回查结果本身也要进上下文：一次读回五十条消息，等于把刚压缩
	// 掉的东西又装回去了。
	historyMaxLimit = 20
	// 这里曾经有一个 historyMaxResultChars = 10000 的返回上限。**已移除。**
	//
	// 回查的全部意义就是拿到**逐字原文**。在一个专门用来取回原文的入口上做截断，
	// 是自相矛盾的——模型来这里正是因为摘要不够精确。要控制返回量，用条数
	// （limit / count）：那是模型能看懂、能调整、能翻页的旋钮，而字符上限是一把
	// 它看不见的刀。
	// searchDefaultLimit 与 searchMaxLimit 是 search 返回的命中条数。
	//
	// 比 read 更克制：一次搜索可能命中几十处，而每一处都要带上下文。返回太多等于
	// 把刚压掉的东西装回去，而模型真正需要的通常是头几条——它接着会用 read 去看
	// 具体某一处的邻域。
	searchDefaultLimit = 5
	searchMaxLimit     = 20
	// searchMaxContext 是每处命中前后各带几条的上限。
	searchMaxContext = 3
	// maxLinesPerHit 是每处命中列出的匹配行数上限。
	//
	// # 为什么必须有这个数，而它不是"截断"
	//
	// `search` 不可能同时做到"返回完整内容"和"输出有界"——这两条在数学上冲突：
	// grep 在每行都含查询词的文件上就是返回整个文件。真机跑出来的后果是级联爆炸：
	// 一次 `search "ls -laR" role=tool` 返回 2.53 MB，而且**自我放大**（search 的
	// 结果本身成为一条 tool 消息，带着查询词，被下一次 search 整条捞回来），
	// 最后连终结性全塌都发不出去。
	//
	// 所以 search 的承诺不是"内容"，是"**在哪**"（设计文档给它定的职责原话）。
	// 限的是**匹配行的条数**，而每一条列出来的行都是完整的行；同时如实报出总共有
	// 多少行匹配、那条消息有多大。模型据此决定要不要 `read` ——**它知道自己拿到的
	// 是什么，也知道漏了什么**，这正是与有害截断的区别：有害的那种让模型以为自己
	// 看到了全貌。
	//
	// **残留的洞**：单条消息只有一行、而那一行几 MB（压扁的 JSON）时，这个上限
	// 拦不住。已记入文档的已知风险——限行数不限行内容，是刻意的取舍。
	maxLinesPerHit = 5
	// searchContextChars 是 scope=summaries 时摘录命中处前后各多少字符。
	//
	// 这**不是截断**：摘要正文本来就完整存在库里，一次 read 或 inspect 就能拿到
	// 全文；这里给的是"命中在哪"的定位信息。区别在于原文是否可达——原文可达时
	// 给摘录是导航，原文不可达时给摘录才是截断。
	//
	// raw 消息的命中不摘录，逐字全给（见 renderSearchHit）。
	searchContextChars = 400
)

// NewHistory 创建绑定到给定会话的回查工具。
func NewHistory(session *domain.Session) *HistoryTool {
	return &HistoryTool{session: session}
}

// historyArguments 是 conversation_history 接受的参数。
type historyArguments struct {
	// Action 是 search、read 或 inspect。
	Action string `json:"action"`
	// BatchID 是要查看的摘要节点。inspect 必填；read 时它是两种入口之一。
	BatchID string `json:"batch_id"`
	// Offset 是相对该节点覆盖范围的消息偏移，默认 0。只在按节点 read 时有意义。
	Offset int `json:"offset"`
	// Limit 是本次最多返回几条。
	Limit int `json:"limit"`
	// From 与 Count 是按**绝对序号**读的入口：从第 From 条开始读 Count 条，
	// 序号从 1 开始，与 search 的输出、以及摘要里那些 #N 标注同一个口径。
	//
	// # 为什么必须有这个入口
	//
	// 原来 read 只接受 {batch_id, offset}，offset 相对某个节点；而 search 返回的
	// 是绝对序号。模型拿到 #127 之后得先猜哪个 batch 覆盖它——**两个原语串不
	// 起来**。这是设计失误，不是实现问题：一个检索原语和一个读取原语如果不能
	// 直接对接，那么它们各自再好用也没有意义。
	//
	// From 是指针而不是 int：0 是模型**真会写**的值（很多语言的下标从 0 开始），
	// 而它和"没给 from"必须能分开。分不开的话，一次 off-by-one 会得到"你没给
	// from"这种驴唇不对马嘴的提示，模型只会把同样的参数再发一遍。
	From  *int `json:"from"`
	Count int  `json:"count"`
	// Lines 是**一条消息内部**的行区间，形如 "1-200"，从 1 起、两端闭合。
	//
	// # 为什么必须有它
	//
	// 视图层会把超大的工具观察收成"头 + 标注 + 尾"（见 contextmgr/observation.go），
	// 标注里指向 `read from=N`。但如果 read 只能整条返回，那个指向就是一条死路——
	// 一条 2.5 MB 的观察读回来就是把上下文炸掉，模型只能在"不读"和"炸掉"之间选。
	//
	// **光标注"这里省略了"不等于可完整阅读，必须有续读入口。** 有了行区间，
	// 存储零截断 + 视图有界 + 按需分页三件事才凑齐：任意长的内容都能一页页读完，
	// 而且每一行都是完整的行。
	//
	// 只在按 from 读单条时有意义；跨多条消息时忽略（那时该用 count 翻消息）。
	Lines string `json:"lines"`
	// Query 是 search 要找的字符串（子串匹配，大小写不敏感）。
	Query string `json:"query"`
	// Role 过滤命中的角色：user、assistant、tool，空表示全部。
	Role string `json:"role"`
	// Scope 是 raw（原始消息，默认）或 summaries（摘要正文）。
	Scope string `json:"scope"`
	// Context 是每处命中前后各带几条，默认 0、上限 3。
	Context int `json:"context"`
}

// Spec 返回发送给模型的工具定义。
func (tool *HistoryTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name: "conversation_history",
		Description: "回查这个会话的历史消息，包括已经被压缩成摘要的部分。" +
			"当摘要不足以确定精确的命令、路径、报错原文、标识符或旧的工具结果时使用。" +
			"search 在全部消息里找一个确切的字符串，返回它出现在第几条、那条有多大、" +
			"以及匹配的那几行（不返回整条消息——要完整原文用 read）；" +
			"read 读出原始消息，可以给 batch_id 读某个摘要节点覆盖的范围，" +
			"也可以给 from/count 按绝对序号读（search 的结果和摘要里的 #N 标注都是绝对序号）；" +
			"读单条超大观察时给 lines=\"1-200\" 之类的行区间分页，一页页把完整原文翻出来；" +
			"inspect 查看一个摘要节点的层级、覆盖范围和它的直接子节点。" +
			"典型用法是先 search 拿到序号，再 read from=那个序号 读它周围。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["search", "read", "inspect"],
      "description": "search 找字符串，read 读原始消息，inspect 查看节点结构"
    },
    "query": {
      "type": "string",
      "description": "search 要找的字符串，子串匹配、大小写不敏感"
    },
    "role": {
      "type": "string",
      "enum": ["user", "assistant", "tool"],
      "description": "search 时只搜这个角色的消息，不填搜全部。user 是用户说过的话，tool 是命令输出"
    },
    "scope": {
      "type": "string",
      "enum": ["raw", "summaries"],
      "description": "search 的范围：raw 搜原始消息（默认），summaries 搜摘要正文"
    },
    "context": {
      "type": "integer",
      "description": "search 时每处命中前后各带几条邻居（只给序号、角色、字节数与首行，不给正文），默认 0，最大 3"
    },
    "batch_id": {
      "type": "string",
      "description": "摘要节点 ID，形如 mem_ 开头。inspect 必填；read 时是两种入口之一"
    },
    "from": {
      "type": "integer",
      "description": "read 时按绝对序号读的起点，从 1 开始"
    },
    "count": {
      "type": "integer",
      "description": "read from 时读几条，默认 10，最大 20"
    },
    "lines": {
      "type": "string",
      "description": "read from 单条消息时，只读它的这个行区间，形如 \"1-200\"（从 1 起、两端闭合）。超大的观察在上下文里被收了边界，用它把完整原文一页页翻出来"
    },
    "offset": {
      "type": "integer",
      "description": "read batch_id 时相对该节点覆盖范围的消息偏移，默认 0"
    },
    "limit": {
      "type": "integer",
      "description": "read 时本次最多返回几条（默认 10、最大 20）；search 时最多返回几处命中（默认 5、最大 20）"
    }
  },
  "required": ["action"],
  "additionalProperties": false
}`),
	}
}

// DescribeCall 返回这次回查的一行标题。
func (tool *HistoryTool) DescribeCall(call domain.ToolCall) string {
	arguments, err := parseHistoryArguments(call.Arguments)
	if err != nil {
		return "回查较早对话"
	}
	switch arguments.Action {
	case "search":
		return fmt.Sprintf("在历史里搜索 %q", arguments.Query)
	case "inspect":
		return fmt.Sprintf("查看摘要节点 %s 的结构", arguments.BatchID)
	}
	if arguments.BatchID != "" {
		return fmt.Sprintf("读取摘要节点 %s 覆盖的原始消息", arguments.BatchID)
	}
	if arguments.From != nil {
		return fmt.Sprintf("读取第 %d 条起的历史消息", *arguments.From)
	}
	return "读取较早对话的原文"
}

// Run 执行一次回查。
//
// 和 BashTool 一样，返回的 error 恒为 nil：参数非法、节点不存在、偏移越界都是
// 模型可读的观察，它据此纠正之后重试即可，不该让整轮交互失败。
//
// 三个动作的分发顺序有意为之：search 最先（它不需要节点，参数形态完全不同）；
// 然后是 read 的按序号入口（只认 from/count）；剩下的 action 才走"校验 batch_id
// → 查仓库 → inspect/read"这条公共路径。batch_id 的格式校验和存在性检查只对
// 真正需要节点的动作做，search 和按序号的 read 不该因为节点状态被挡在外面。
func (tool *HistoryTool) Run(
	ctx context.Context,
	call domain.ToolCall,
	_ domain.OutputFunc,
) (domain.ToolResult, error) {
	arguments, err := parseHistoryArguments(call.Arguments)
	if err != nil {
		return errorObservation("参数不合法：%v", err), nil
	}

	// search 不需要 batch_id，先分出去。
	if arguments.Action == "search" {
		// 找一个字符串出现在那里，返回类似消息在第几条
		return tool.search(arguments), nil
	}

	// read 有两个入口，按参数形态判别：
	//
	//   按绝对序号   read from=7            → readRange（search 的输出直接接这里）
	//   按摘要节点   read batch_id=mem_x    → read（配合 offset 分页）
	//
	// 判据是"给了 from 且没给 batch_id"。注意 from 是 *int：0 是模型真会写的值
	// （很多语言下标从 0 起），它和"没给 from"必须能分开——分不开的话，一次
	// off-by-one 会得到"你没给 from"这种驴唇不对马嘴的提示，模型只会把同样的
	// 参数原样再发一遍。
	if arguments.Action == "read" && arguments.BatchID == "" && arguments.From != nil {
		// 模型传from，即代表把from开始的消息都传给模型
		// 模型给count表示传的条数，没传会按照默认上限来
		return tool.readRange(arguments), nil
	}

	// 走到这里，说明模型要求的action是inspect,或者read给了batch_id
	batchID := domain.MemoryBatchID(arguments.BatchID)
	// batch_id 是模型给的，它完全可能编一个出来。先校验格式再查仓库，
	// 错的格式得到一条明确的说明，而不是一句"没找到"。
	if err := batchID.Validate(); err != nil {
		return errorObservation("batch_id 格式不对：%v", err), nil
	}
	batch, found := tool.session.Memory.Batch(batchID)
	if !found {
		return errorObservation(
			"没有找到摘要节点 %s。可用的节点 ID 出现在上下文里那些标着"+
				"[较早对话的摘要 ...] 的消息上。", batchID), nil
	}

	switch arguments.Action {
	case "inspect":
		// 返回摘要的结构，如果有子节点，就列出子节点
		return tool.inspect(batch), nil
	case "read":
		// 返回一个摘要对应的全部消息
		return tool.read(batch, arguments), nil
	default:
		return errorObservation(
			"action 只能是 search、read 或 inspect，收到 %q", arguments.Action), nil
	}
}

// search 在会话里找一个确切的字符串。
//
// # 为什么它不碰数据库
//
// 工具持有 *domain.Session，历史已经全在内存里。几百到几千条消息，一次线性扫描
// 是微秒级——**不需要 SQL，不需要索引，也不需要另一个进程**。为这个规模建索引，
// 维护成本远大于收益，而且多一个会和历史不同步的东西。
//
// # 检索面向全部角色
//
// 模型可以搜任何角色的消息：
//
//	user       "我当时是怎么要求的"。只占 0.4%，几乎没有噪音，是最高频的用法
//	assistant  "我之前是怎么解释的"
//	tool       噪音最大（占 84%），但"那条命令的输出到底是什么"同样是真实需求
//
// 所以是支持而不是禁止 tool，噪音由 limit 和命中窗口控制。
//
// # 输出的三段结构
//
// 头部：命中总数；超过 limit 时如实说明只列了前几处——模型必须知道
// 自己看到的是子集，否则会把"列出的"当成"全部的"。
// 中部：每处命中一段（见 renderSearchHit）。
// 尾部：下一步的用法提示。报出每条的字节数，是为了让模型在 read 之前
// 能判断"这条值不值得读回来"——一条 2.5 MB 的观察读回来就是炸掉上下文，
// 而它此前无从预知。
//
// # 边界
//
// 只搜当前会话，参数里没有 session_id 也没有路径。模型无法用它读别的会话，
// 也无法用它读任何文件——那是 bash 的事，而 bash 有它自己的边界。
//
// # 序号口径
//
// 内部扫描用 0 基下标，输出一律转成 1 基序号（#N）——与 read from=N、
// 摘要里的区间标注同一个口径。search 的输出要能直接喂给 read，
// 两个原语才串得起来；口径不一致是这类检索原语最隐蔽的 bug 来源。
func (tool *HistoryTool) search(arguments historyArguments) domain.ToolResult {
	// 去掉首尾空白：模型有时会发带空格的查询词，而子串匹配对空格敏感，
	// 不处理的话" QX-7731"会搜不到"QX-7731"。空查询直接报错，让模型重试。
	query := strings.TrimSpace(arguments.Query)
	if query == "" {
		return errorObservation("search 需要 query：要找的那个字符串")
	}
	// 命中条数：默认 5，上限 20。上限是防自我放大的一道闸——
	// search 的结果本身会成为一条 tool 消息，返回越多，下一次 search 的扫描面越大。
	limit := arguments.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	limit = min(limit, searchMaxLimit)
	// 邻居条数：默认 0，上限 3。邻居只给导航信息不给正文，
	// 给多了等于变相把没压掉的内容灌回去。
	around := min(max(arguments.Context, 0), searchMaxContext)

	// scope 分流：summaries 搜摘要正文，raw（默认）搜原始消息。
	// 写错的 scope 得到一条明确的说明，而不是静默按默认处理——
	// 静默会让模型以为自己在摘要里搜过了。
	if arguments.Scope == "summaries" {
		return tool.searchSummaries(query, limit)
	}
	if arguments.Scope != "" && arguments.Scope != "raw" {
		return errorObservation("scope 只能是 raw 或 summaries，收到 %q", arguments.Scope)
	}
	return tool.searchRaw(query, arguments.Role, limit, around)
}

// searchRaw 在原始消息里搜。
//
// 搜**全部现存消息**，不限于已被压缩的部分：保留区里的也能搜到。规则简单一条，
// 比"只搜游标之前"好解释——后者会让模型在"刚说过的话"上搜不到东西，而它并不知道
// 游标在哪。
func (tool *HistoryTool) searchRaw(query, role string, limit, around int) domain.ToolResult {
	// 角色过滤参数先校验：写错的 role 得到明确报错，而不是静默搜出全部。
	if role != "" && role != "user" && role != "assistant" && role != "tool" {
		return errorObservation("role 只能是 user、assistant 或 tool，收到 %q", role)
	}

	history := tool.session.Messages()
	// 双方都转小写：大小写不敏感匹配。工具输出里标识符大小写混杂
	// （QX-7731 / qx-7731 都可能被模型记成查询词），精确匹配会漏掉一半。
	needle := strings.ToLower(query)

	// 核心循环：线性扫描全部现存消息。
	//
	// 搜的是**全部**消息，不只是已压缩的部分。规则只有一条——"search 搜的是
	// 这个会话里的一切"；如果只搜游标之前，模型在"刚说过的话"上会搜不到东西，
	// 而它并不知道游标在哪。保留区里被搜到没有额外代价：read 照样能取。
	var hits []int
	for index, message := range history {
		if role != "" && string(message.Role) != role {
			continue
		}
		// searchableText 把正文和工具调用参数拼在一起搜——
		// 模型发出过的命令、查过的文件，是后续最常想搜回来的东西。
		if strings.Contains(strings.ToLower(searchableText(message)), needle) {
			hits = append(hits, index) // 记 0 基下标，输出时再转 1 基序号
		}
	}

	// 零命中分支：返回的是一条有用的说明，不是错误、也不是空串。
	//
	// 明确说"没有"而不是返回空：模型据此换个词再搜，而不是以为工具坏了。
	// 顺带提示 scope="summaries" 这个入口——原始消息里搜不到时，它最可能
	// 想不起来还可以去摘要正文里搜。
	if len(hits) == 0 {
		return domain.ToolResult{
			Status: domain.ToolSuccess,
			Content: fmt.Sprintf(
				"在%s的原始消息里没有找到 %q。\n"+
					"可以换一个更短、更确切的片段再试；或者用 scope=\"summaries\" 搜摘要正文。",
				roleLabel(role), query),
		}
	}

	// ---- 报告头部：总数 + 截断说明 ----
	var report strings.Builder
	fmt.Fprintf(&report, "在%s的原始消息里找到 %d 处 %q",
		roleLabel(role), len(hits), query)
	if len(hits) > limit {
		// 超过 limit 时如实说明只列了前几处。把"列出的"当成"全部的"
		// 是检索类输出最危险的静默失真。
		fmt.Fprintf(&report, "，只列出前 %d 处", limit)
	}
	report.WriteString("：\n")

	// ---- 中部：逐处渲染命中 ----
	shown := 0
	for _, hit := range hits {
		if shown >= limit {
			break
		}
		report.WriteString(renderSearchHit(history, hit, around, needle))
		shown++
	}

	// ---- 尾部：下一步的用法提示 ----
	// 教模型怎么接 read：先看字节数再决定读哪条——
	// 把"要不要读、读哪条"的判断依据直接给它，而不是让它拿到 #N 之后盲读。
	fmt.Fprintf(&report,
		"\n上面每条都标了字节数。要看完整原文用 read from=<序号> count=<条数>——"+
			"read 会逐字返回，所以先看字节数再决定读哪一条。")

	return domain.ToolResult{Status: domain.ToolSuccess, Content: report.String()}
}

// searchSummaries 在摘要正文里搜。
//
// 这是"语义检索"这件事在本系统里最便宜的形态：摘要只有几十条，而且是自然语言写的
// 语义压缩，由**将要使用它的同一个模型**写的。真到摘要也覆盖不住的规模，扩展方向
// 也是这里，而不是去嵌入原始消息。
func (tool *HistoryTool) searchSummaries(query string, limit int) domain.ToolResult {
	needle := strings.ToLower(query)
	var report strings.Builder
	found := 0

	for _, batch := range tool.session.Memory.Batches {
		if found >= limit {
			break
		}
		// 用 EffectiveContent：人工修订过的那一版才是当前生效的。
		content := batch.EffectiveContent()
		if !strings.Contains(strings.ToLower(content), needle) {
			continue
		}
		found++
		fmt.Fprintf(&report, "\n摘要 %s（层级 %d，覆盖第 %d 到 %d 条）\n%s\n",
			batch.ID, batch.Level, batch.StartMessageIndex+1, batch.EndMessageIndex,
			snippetAround(content, needle))
	}

	if found == 0 {
		return domain.ToolResult{
			Status:  domain.ToolSuccess,
			Content: fmt.Sprintf("在摘要正文里没有找到 %q。原始消息里可能有，用 scope=\"raw\" 再搜一次。", query),
		}
	}
	return domain.ToolResult{
		Status: domain.ToolSuccess,
		Content: fmt.Sprintf("在摘要正文里找到 %d 处 %q：\n%s\n"+
			"用 read batch_id=<节点> 或 read from=<序号> 读它覆盖的原文。",
			found, query, report.String()),
	}
}

// readRange 按绝对序号读原始消息。
//
// 序号从 1 开始，和 search 的输出、摘要里的 #N 标注、renderHistoryMessage 的
// 编号完全同一个口径。**这一致性就是这个入口存在的全部理由**——它让检索和读取
// 两个原语能直接串起来。
func (tool *HistoryTool) readRange(arguments historyArguments) domain.ToolResult {
	history := tool.session.Messages()
	from := *arguments.From
	// 越界报错要把合法范围、会话总条数一起给出：模型拿到这些信息就能自己
	// 修正参数重试，而不是再发一轮试探。
	if from < 1 || from > len(history) {
		return errorObservation(
			"from 要在 1 到 %d 之间（这个会话共 %d 条消息，序号从 1 开始），收到 %d。"+
				"也可以给 batch_id 读某个摘要节点覆盖的范围。",
			len(history), len(history), from)
	}
	// 给了行区间就是"读这一条的这几行"，count 不参与——两者混用只会让语义含混。
	// 行区间是给超大单条消息分页用的（见 readLines）；跨多条消息翻页用 count。
	if strings.TrimSpace(arguments.Lines) != "" {
		return tool.readLines(history, from, arguments.Lines)
	}

	// 条数兼容两个参数名：count 是新口径（from/count 成对出现），limit 是旧的
	// 通用参数名——模型的上下文里两处文档提示都可能出现，都接住比报错省一轮往返。
	count := arguments.Count
	if count <= 0 {
		count = arguments.Limit
	}
	if count <= 0 {
		count = historyDefaultLimit // 10
	}
	count = min(count, historyMaxLimit) // 20：上限防一次性把上下文撑爆

	start := from - 1
	end := min(start+count, len(history))

	// 报告头部带上会话总条数：模型才知道自己在历史的什么位置、还剩多少没看。
	var report strings.Builder
	fmt.Fprintf(&report, "第 %d 到 %d 条消息（这个会话共 %d 条）：\n",
		start+1, end, len(history))

	// 逐字渲染，不给任何摘要或裁剪——这是一个专门取回原文的入口，
	// 在它身上截断是自相矛盾的（见 read 里 historyMaxResultChars 那条注释）。
	for index := start; index < end; index++ {
		report.WriteString(renderHistoryMessage(index+1, history[index]))
	}
	// 还有后续时直接给出下一页参数：让模型知道自己在哪、还剩多少、
	// 下一步怎么写，比让它自己推算可靠。
	if end < len(history) {
		fmt.Fprintf(&report, "\n后面还有消息，用 from=%d 继续。", end+1)
	}
	return domain.ToolResult{Status: domain.ToolSuccess, Content: report.String()}
}

// readLines 读一条消息内部的一个行区间。
//
// 这是"超大观察可完整阅读"的最后一环：视图层把它收成头 + 标注 + 尾，标注指向
// `read from=N`；有了行区间，模型就能一页页把完整原文翻出来。**每一行都是完整的
// 行，没有任何一行被裁**——分页不是截断。
//
// 报出总行数与总字节数，并在还有后续时直接给出下一页的参数：让模型知道自己在哪、
// 还剩多少、下一步怎么写，比让它自己推算可靠。
func (tool *HistoryTool) readLines(history []domain.Message, from int, spec string) domain.ToolResult {
	message := history[from-1]
	// 在**渲染后**的文本上按行切，而不是在原始 Content 上：视图渲染会加上
	// #N 序号头、工具调用行，超大观察还会带"共 N 字节 / N 行"的标注——
	// 模型要翻的就是它实际看到的那份文本，按原始内容切行会错位。

	//把消息转化为模型能读的一段文本
	text := renderHistoryMessage(from, message)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	first, last, err := parseLineRange(spec, len(lines))
	if err != nil {
		// 报错时带上总行数：模型不用再发一轮试探就知道合法范围。
		return errorObservation("lines %q 不合法：%v。这条消息共 %d 行，"+
			"写成 lines=\"1-200\" 这样的闭区间。", spec, err, len(lines))
	}

	// 头部报总行数与总字节数：模型才知道这条消息有多大、自己读到哪了。
	var report strings.Builder
	fmt.Fprintf(&report, "第 %d 条消息共 %d 行 / %d 字节，这是第 %d–%d 行：\n",
		from, len(lines), len(message.Content), first, last)
	for _, line := range lines[first-1 : last] {
		report.WriteString(line)
		report.WriteByte('\n')
	}
	// 下一页参数按本次窗口宽度算（last-first+1），而不是写死 200：
	// 模型要 50 行就是想小步翻，尊重它的节奏。
	if last < len(lines) {
		fmt.Fprintf(&report, "\n还有 %d 行，用 lines=\"%d-%d\" 继续。",
			len(lines)-last, last+1, min(last+(last-first+1), len(lines)))
	} else {
		// 读完的明确信号：不写这句，模型可能再发一次同样的请求确认。
		report.WriteString("\n这条消息已经读完。")
	}
	return domain.ToolResult{Status: domain.ToolSuccess, Content: report.String()}
}

// parseLineRange 解析 "1-200" 这样的闭区间，并夹到实际行数之内。
//
// 也接受单个数字（"7" 等价于 "7-7"）：模型只想看某一行时不必写两遍。
func parseLineRange(spec string, total int) (first, last int, err error) {
	spec = strings.TrimSpace(spec)
	// 用 Cut 而不是正则：格式就两种（"a-b" 和 "a"），不值得引入正则的出错面。
	// 没有横线就是单行区间——"7" 等价于 "7-7"，模型只想看某一行时不必写两遍。
	head, tail, found := strings.Cut(spec, "-")
	if !found {
		tail = head
	}
	if first, err = strconv.Atoi(strings.TrimSpace(head)); err != nil {
		return 0, 0, fmt.Errorf("起点不是整数")
	}
	if last, err = strconv.Atoi(strings.TrimSpace(tail)); err != nil {
		return 0, 0, fmt.Errorf("终点不是整数")
	}
	if first < 1 {
		return 0, 0, fmt.Errorf("起点要从 1 开始")
	}
	if first > total {
		return 0, 0, fmt.Errorf("起点超过了总行数")
	}
	if last < first {
		return 0, 0, fmt.Errorf("终点小于起点")
	}
	// 终点越界只夹回来，不报错：模型写 "1-1000" 想表达"从头读一大段"是合理的，
	// 为此让它先去数总行数是多余的往返。
	return first, min(last, total), nil
}

// searchableText 返回一条消息里可以被搜到的全部文字。
//
// 工具调用的参数也算进去：模型问"那条 ls 命令的完整参数是什么"时，要找的字符串
// 就在 arguments 的 JSON 里，而不在 Content 里。漏掉它，最常见的一类检索需求
// 直接失效。
// searchableText 拼出一条消息的可搜文本：正文 + 工具调用的名字和参数。
//
// 只搜正文不够。assistant 消息里的工具调用参数是模型发出过的命令、查过的路径，
// 后续"那条命令到底怎么写的"是最高频的回查需求之一，参数不进搜索范围的话
// 这些全都搜不到。拼接的接缝可能凑巧命中（正文结尾 + 参数开头恰好拼出查询词），
// matchingLines 对 total==0 的兜底处理的就是这种情况。
func searchableText(message domain.Message) string {
	if len(message.ToolCalls) == 0 {
		return message.Content
	}
	var text strings.Builder
	text.WriteString(message.Content)
	for _, call := range message.ToolCalls {
		fmt.Fprintf(&text, " %s %s", call.Name, string(call.Arguments))
	}
	return text.String()
}

// renderSearchHit 渲染一处命中，连同它前后各 around 条。
func renderSearchHit(history []domain.Message, hit, around int, needle string) string {
	var entry strings.Builder

	// ---- 命中那条 ----
	// 报出位置（1 基序号）、角色、体量、匹配行数——四样缺一不可：
	// 序号是 read 的入参，体量是"要不要读"的判断依据，
	// 匹配行让模型不用 read 就能确认"是不是我要的那条"。
	message := history[hit]
	text := searchableText(message)
	matched, total := matchingLines(text, needle)
	fmt.Fprintf(&entry, "\n→ #%d [%s] 共 %d 字节，%d 行匹配\n",
		hit+1, message.Role, len(text), total)
	for _, line := range matched {
		fmt.Fprintf(&entry, "     %s\n", line)
	}
	if total > len(matched) {
		// 匹配行最多列 5 行，剩下的必须如实报出并给出取回命令。
		// **模型必须知道自己漏了什么、以及怎么补**——只列不报总数，
		// 它会把前 5 行当成全部，这正是"有损而不自知"。
		fmt.Fprintf(&entry, "     （还有 %d 行匹配，用 read from=%d count=1 看完整原文）\n",
			total-len(matched), hit+1)
	}

	// 上下文那几条：只报导航信息（位置、体量、首行），不给正文。
	//
	// 这是 search 与 read 的分工：**search 说在哪，read 给内容**。带上体量，
	// 模型才能在 read 之前判断"这条值不值得读回来"——一条 2.5 MB 的观察读回来
	// 就是把上下文炸掉，而它此前无从预知。
	for index := max(hit-around, 0); index < min(hit+around+1, len(history)); index++ {
		if index == hit {
			continue
		}
		neighbour := searchableText(history[index])
		fmt.Fprintf(&entry, "   #%d [%s] 共 %d 字节：%s\n",
			index+1, history[index].Role, len(neighbour), firstLine(neighbour))
	}
	return entry.String()
}

// matchingLines 返回文本里包含 needle 的那些行，**每行完整**。
//
// # 为什么是"行"而不是"整条消息"
//
// 这是 grep 的语义，也是设计文档给 search 定的职责——"那个确切的字符串**在哪**"。
// 上一版把整条消息压成一行返回，于是一条 165 KB 的工具观察就是 165 KB 的"一行"。
//
// 真机跑出来的后果是级联爆炸：模型在终结性全塌之后丢了原文，用 search 去找回来，
// 一次 `search "ls -laR" role=tool` 返回了 2.53 MB。而且它**自我放大**——search 的
// 结果本身成为一条 tool 消息，正文里带着查询词，于是被下一次 search 匹配到并整条
// 捞回来，滚成更大的一条。最后连终结性全塌都发不出去了（它的源文本就是那 136 万
// token 的保留区，超过了模型的真实窗口）。
//
// 这不是"要不要截断"的问题：截断是承诺给内容却给残缺版本，而这里是把承诺从
// "整条消息"改回"匹配的位置"，位置给得完整——每一条匹配行都是完整的行。
//
// needle 已经是小写（调用方转过），因此这里对每行也转小写再比。
// matchingLines 返回命中的行：listed 是前列出的（至多 5 行），total 是全部行数。
//
// 两个返回值必须分开：total 参与"还有 N 行匹配"的如实报告，listed 参与"快速
// 确认是不是要的那条"。只返回 listed 的话，调用方无从知道漏了多少。
func matchingLines(text, needle string) (listed []string, total int) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		total++ // 先计数：哪怕超出了列出的上限，总数也要数全
		if len(listed) < maxLinesPerHit {
			listed = append(listed, strings.TrimRight(line, "\r"))
		}
	}
	// 一条也没有：说明命中落在被 searchableText 拼接起来的接缝上（正文与工具调用
	// 参数之间）。给出首行，至少让模型知道该 read 哪一条。
	if total == 0 {
		return []string{firstLine(text)}, 1
	}
	return listed, total
}

// firstLine 返回文本的第一行，用作导航条目里的一句提示。
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// snippetAround 截出包含 needle 的一段，前后各留一些。
// snippetAround 摘出摘要正文里命中处前后各 200 字符（共约 400）。
//
// 摘要本身是自然语言，命中行没有"行"的意义，所以按字符窗口摘录而不是按行列出。
// 窗口截断在这里不违反零截断：摘要正文完整存在库里，一次 read 就能拿到全文，
// 摘录是"命中在哪"的定位信息——区别在于原文是否可达。
func snippetAround(content, needle string) string {
	lower := strings.ToLower(content)
	at := strings.Index(lower, needle)
	if at < 0 {
		return truncateRunes(content, searchContextChars)
	}
	runes := []rune(content)
	// 按 rune 定位：Index 给的是字节偏移，中文一个字三个字节，直接拿来切会切碎。
	hitRune := len([]rune(content[:at]))
	from := max(hitRune-searchContextChars/2, 0)
	to := min(from+searchContextChars, len(runes))
	snippet := string(runes[from:to])
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(runes) {
		snippet += "…"
	}
	return snippet
}

// truncateRunes 截出摘要正文里命中处附近的一段，按 rune 切。
//
// 只用于 scope=summaries 的定位摘录。**它不是截断**：摘要正文完整存在库里，
// 一次 read 或 inspect 就能拿到全文，这里给的只是"命中在哪"。
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// singleLineText 把多行压成一行，让每处命中占一行、结构清楚。
func singleLineText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(strings.TrimSpace(text), "\n", " ⏎ ")
}

// roleLabel 把角色过滤写成一句人话，用在结果的开头。
func roleLabel(role string) string {
	switch role {
	case "user":
		return "用户"
	case "assistant":
		return "助手"
	case "tool":
		return "工具"
	default:
		return "全部"
	}
}

// inspect 返回一个节点的结构：它自己的信息，加上直接子节点。
func (tool *HistoryTool) inspect(batch domain.MemoryBatch) domain.ToolResult {
	var report strings.Builder
	fmt.Fprintf(&report, "摘要节点 %s\n", batch.ID)
	fmt.Fprintf(&report, "层级 %d，覆盖第 %d 到 %d 条消息（共 %d 条）\n",
		batch.Level, batch.StartMessageIndex+1, batch.EndMessageIndex, batch.Covers())
	fmt.Fprintf(&report, "摘要：%s\n", batch.Title())

	if len(batch.SourceBatchIDs) == 0 {
		// 叶子节点要说清楚"到底了"，否则模型可能继续往下找一层不存在的子节点。
		report.WriteString("\n这是叶子节点，没有子节点。它直接覆盖上面那段原始消息，" +
			"用 read 可以逐条读出来。")
		return domain.ToolResult{Status: domain.ToolSuccess, Content: report.String()}
	}

	report.WriteString("\n直接子节点：\n")
	for _, childID := range batch.SourceBatchIDs {
		child, found := tool.session.Memory.Batch(childID)
		if !found {
			fmt.Fprintf(&report, "  %s（读不出来）\n", childID)
			continue
		}
		fmt.Fprintf(&report, "  %s 层级 %d 覆盖第 %d 到 %d 条：%s\n",
			child.ID, child.Level, child.StartMessageIndex+1, child.EndMessageIndex, child.Title())
	}
	report.WriteString("\n只列出直接子节点。需要更细的内容就 inspect 其中一个，" +
		"或者对任意节点直接 read 它覆盖的原始消息。")

	return domain.ToolResult{Status: domain.ToolSuccess, Content: report.String()}
}

// read 分页返回一个摘要节点覆盖范围内的原始消息。
//
// 这是按节点读的入口（按绝对序号读走 readRange）。两个入口服务的场景不同：
// 模型 inspect 出一个节点之后想看"这段摘要底下到底是什么"，走这里；
// 拿着 search 给的序号想精确读某几条，走 readRange。
func (tool *HistoryTool) read(batch domain.MemoryBatch, arguments historyArguments) domain.ToolResult {
	history := tool.session.Messages()

	// 节点记录的是压缩当时的下标。历史只增不减，因此这些下标始终有效；
	// 但数据被外部改过时可能越界，夹一下比 panic 好——
	// 少读几条只是少一点上下文，整个工具崩掉是丢掉唯一的回查手段。
	start := min(max(batch.StartMessageIndex, 0), len(history))
	end := min(max(batch.EndMessageIndex, start), len(history))
	covered := history[start:end]

	if arguments.Offset < 0 || arguments.Offset > len(covered) {
		return errorObservation("offset %d 超出范围，该节点覆盖 %d 条消息",
			arguments.Offset, len(covered))
	}
	limit := arguments.Limit
	if limit <= 0 {
		limit = historyDefaultLimit // 10
	}
	limit = min(limit, historyMaxLimit) // 20：回查结果也要进上下文，一次读回
	// 五十条等于把刚压掉的东西原样装回去，压缩就白做了。

	var report strings.Builder
	fmt.Fprintf(&report, "摘要节点 %s 覆盖 %d 条消息，从第 %d 条开始读：\n",
		batch.ID, len(covered), arguments.Offset+1)

	read := 0
	for index := arguments.Offset; index < len(covered) && read < limit; index++ {
		// 序号用 start+index+1：节点覆盖范围在完整历史里是 [start, end)，
		// 渲染出的 #N 必须是它在**完整历史**里的序号——只有这样，这次读到的
		// 内容才能继续喂给 search 或 readRange，口径在全工具内一致。
		// 逐字全给。条数由 limit 控制，不再有字符上限——
		// 在一个专门取回原文的入口上截断是自相矛盾的。
		report.WriteString(renderHistoryMessage(start+index+1, covered[index]))
		read++
	}

	nextOffset := arguments.Offset + read
	if nextOffset < len(covered) {
		fmt.Fprintf(&report, "\n还有 %d 条未读，用 offset=%d 继续。",
			len(covered)-nextOffset, nextOffset)
	} else {
		report.WriteString("\n这个节点覆盖的消息已经读完。")
	}

	return domain.ToolResult{Status: domain.ToolSuccess, Content: report.String()}
}

// renderHistoryMessage 把一条历史消息渲染成回查结果里的一段。
func renderHistoryMessage(number int, message domain.Message) string {
	var entry strings.Builder
	fmt.Fprintf(&entry, "\n#%d [%s]", number, message.Role)
	if message.ToolCallID != "" {
		fmt.Fprintf(&entry, "（回应调用 %s）", message.ToolCallID)
	}
	entry.WriteString("\n")
	if message.Content != "" {
		entry.WriteString(message.Content)
		entry.WriteString("\n")
	}
	for _, call := range message.ToolCalls {
		fmt.Fprintf(&entry, "  调用 %s（id=%s）%s\n", call.Name, call.ID, string(call.Arguments))
	}
	return entry.String()
}

// parseHistoryArguments 解析并校验参数。
func parseHistoryArguments(raw json.RawMessage) (historyArguments, error) {
	var arguments historyArguments
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// 对应 Schema 里的 additionalProperties=false：模型写错字段名时立刻得到反馈，
	// 而不是让一条被忽略的参数造成难以理解的行为。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&arguments); err != nil {
		return arguments, fmt.Errorf("参数不符合 conversation_history 的定义：%w", err)
	}
	if arguments.Action == "" {
		return arguments, errors.New("action 必填，取值为 search、read 或 inspect")
	}
	// batch_id 不再是无条件必填：read 有 from/count 这个入口，search 根本不需要它。
	// 校验放到各个动作里去做，那里才知道这次到底缺了什么。
	if arguments.Action == "inspect" && strings.TrimSpace(arguments.BatchID) == "" {
		return arguments, errors.New("inspect 需要 batch_id")
	}
	if arguments.Action == "read" &&
		strings.TrimSpace(arguments.BatchID) == "" && arguments.From == nil {
		return arguments, errors.New(
			"read 需要 batch_id（读某个摘要节点覆盖的范围）或 from（按绝对序号读）")
	}
	return arguments, nil
}

// errorObservation 构造一条 error 观察。
//
// 配对字段由 Registry 填写，这里只负责说明发生了什么。
func errorObservation(format string, arguments ...any) domain.ToolResult {
	return domain.ToolResult{
		Status:  domain.ToolError,
		Content: fmt.Sprintf(format, arguments...),
	}
}
