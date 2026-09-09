package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"goseek/internal/agent"
	"goseek/internal/contextmgr"
	"goseek/internal/domain"
	"goseek/internal/mcp"
	"goseek/internal/model"
	"goseek/internal/plugin"
	"goseek/internal/store"
	"goseek/internal/tool"
)

// ModelClient 是 Runner 需要的全部模型能力。
//
// 它把两个窄接口合在一起：Agent 用 Complete 做决策，Compactor 用 Summarize 做
// 压缩。合成一个是因为它们打的是同一个供应商、共用同一份配置和连接池；但下层
// 仍然各自只依赖自己需要的那一半——agent 包不知道 Summarize 的存在，
// contextmgr 也不知道 Complete 的存在。
type ModelClient interface {
	agent.Model
	contextmgr.Summarizer
}

// ErrTurnInProgress 表示这个会话已经有一轮在跑。
//
// 一个会话同时只允许一轮：并行的两轮会向同一段历史交错追加消息，得到一段谁也
// 说不清顺序的对话，而且工具调用与观察的配对会乱。HTTP 层把它映射成 409。
var ErrTurnInProgress = errors.New("该会话已有一轮交互正在进行")

// ErrBatchNotActive 表示要修订的摘要节点不在当前生效的前沿上。
//
// 单独一个哨兵错误，让 HTTP 层能把它映射成 409 而不是 500：这不是程序出错，
// 是这次请求本身没有意义——改一个已经被合并进上层的节点，模型看到的东西一个字
// 都不会变，界面该明确告诉用户这一点。
var ErrBatchNotActive = errors.New("这段摘要不在当前生效的前沿上")

// ErrNoTurnRunning 表示请求中止时并没有正在跑的一轮。
//
// 它不是错误情况，只是没什么可做——界面据此说"当前没有正在进行的交互"，
// 而不是报一个失败。
var ErrNoTurnRunning = errors.New("当前没有正在进行的交互")

// ErrUnknownModel 表示请求的模型不在 GoSeek 支持目录中。
var ErrUnknownModel = errors.New("模型不在支持列表里")

// ErrUnresolvedToolCalls 表示历史里还有工具调用没有配对的观察。
//
// 这种会话不能换模型：恢复流程会先补观察，补完才具备继续请求模型的条件。
var ErrUnresolvedToolCalls = errors.New("这个会话还有未完成的工具调用")

// Runner 拥有一个已打开会话的写权限。
//
// 它是这个会话状态的唯一写者：Store 的独占锁、内存里的 *domain.Session、以及正在
// 跑的那一轮，都归它管。读路径（列表、快照、事件重放）完全不经过它，走的是
// store.Reader——见 5.6.4。
//
// # 为什么一轮要在子 goroutine 里跑
//
// 一轮交互动辄几十秒。如果直接在 select 循环里跑，循环这段时间就不响应了，连
// "停止服务"都做不到。因此循环只做两件很快的事——接收提交、接收停止——真正的
// 工作交给子 goroutine。
type Runner struct {
	dataDirectory string
	sessionID     domain.SessionID
	session       *domain.Session
	store         *store.Store
	// newModel 按模型名创建客户端。模型是会话属性，切换时需要重建客户端。
	newModel             func(string) ModelClient
	defaultModel         string
	defaultContextWindow int
	agent                *agent.Agent
	hub                  *Hub
	logger               *slog.Logger

	// ctx 覆盖这个 Runner 的整个生命周期。停止时取消它，正在跑的模型请求和
	// 命令会跟着尽快结束，而不是让服务停在一条 sleep 300 上。
	ctx    context.Context
	cancel context.CancelFunc

	// commands 是全部工作（提交、压缩）的入口。无缓冲：发送方要等循环真的收下，
	// 才知道是被接受了还是 Runner 已经停了。
	commands chan command
	// stopped 在 select 循环退出后关闭。
	stopped chan struct{}
	// turnDone 用来等待正在跑的那一轮结束。
	turnDone sync.WaitGroup
	// running 标记是否有一轮在跑。用原子量而不是循环内的普通变量，因为它由子
	// goroutine 在结束时清零。
	//
	// 它的含义在 M5.2 收窄了：原来是"拒绝一切"，现在是**"拒绝并发写记忆"**。
	// 提交用户消息在运行中不再被拒，而是排队注入；压缩和摘要修订仍然被拒，
	// 因为它们并发改写 session.Memory。
	running atomic.Bool

	// turnState 是当前这一轮进行到了哪一步（WAITING_MODEL / RUNNING_TOOL /
	// COMPRESSING），供侧栏与快照回答"哪个会话正在跑、跑到哪一步"。
	//
	// 它必须独立于事件流存在：思考阶段（reasoning delta）不产生 state.changed，
	// 只看事件流的话，页面切走再切回来就会把一个明明在思考的会话显示成"空闲"。
	// 取值是 domain.RunState，用原子量读写——写者是跑 Agent 的子 goroutine，
	// 读者是 HTTP 夹在请求里的任意 goroutine。空闲时取值为空串：running=false
	// 已经回答了"在不在跑"，这里只负责"跑到了哪一步"。
	turnState atomic.Value

	// turnMutex 保护 cancelTurn 与 pending：两者都被两侧访问——HTTP 处理器的
	// goroutine（取消、投递）和主循环／子 goroutine（设置、取走）。
	turnMutex sync.Mutex
	// cancelTurn 取消**当前这一轮**，而不是整个 Runner。
	//
	// Runner.Stop 用的是 runner.cancel（停整个 Runner、释放会话锁）；这里是从它
	// 派生出来的每轮 context 的取消函数。两者不能混：用户按"停止"是想中止这一轮，
	// 不是想关掉会话。
	cancelTurn context.CancelFunc
	// pending 是运行中排队等待注入的用户消息，按提交顺序。
	//
	// 它们会在 Agent 下一次请求模型之前被追加进历史（见 Pending）。
	pending []agent.PendingMessage
	// skillNames 是当前可用 skill 名称清单，注入 system prompt。
	skillNames []string
}

// commandKind 区分投递给主循环的两种工作。
//
// 两者共用同一条队列和同一个 running 标记，因此天然互斥。这不是限制而是正确性：
// 压缩改的是下一次请求要用的视图，和一轮交互并发做，结果无从定义。
type commandKind int

const (
	// commandTurn 是提交一条用户消息，开始一轮交互。
	commandTurn commandKind = iota
	// commandCompact 是用户明确要求的一次压缩（面板上的 /compact）。
	commandCompact
	// commandEditMemory 是人工修订一段活跃摘要。
	//
	// 它也要走这条队列，理由和压缩一样：它写 session.Memory，而一轮交互正在跑时
	// Agent 也在读写同一个对象。区别是它很快（一次校验加一次 UPDATE，没有模型
	// 调用），因此**就在主循环里同步做完**，不派子 goroutine——见 start。
	commandEditMemory
	// commandSetModel 是切换会话模型。
	//
	// 它同步完成：校验、落库、重建 Agent 都是毫秒级操作。走命令队列是为了与
	// turn/compact/editMemory 互斥，避免一轮正在跑时重建 Agent。
	commandSetModel
)

// command 是 HTTP 层向 Runner 主循环投递的一件工作。
//
// 它带一条 reply 通道：主循环通过它回传"这件工作是否被接受"。被接受只表示开始，
// 不表示最终成功；真正结果走事件流。
type command struct {
	kind commandKind
	// content 对 commandTurn 是用户消息，对 commandEditMemory 是修订后的摘要正文
	// （空串表示撤销修订，回到模型原始的那一版）。
	content string
	// batchID 只对 commandEditMemory 有意义。
	batchID domain.MemoryBatchID
	// model 只对 commandSetModel 有意义。
	model string
	// images 只对 commandTurn 有意义：随消息一起发送的图片列表。
	images []domain.MessageImage
	// ambientContext 是只对下一次模型请求可见的界面状态，不进入历史。
	ambientContext string
	// ctx 只约束“接受命令”的握手；真正轮次使用 Runner 派生的 turnCtx。
	ctx context.Context
	// reply 容量为 1，主循环只需写一次，不会阻塞在等待读取者上。
	reply chan error
}

// NewRunner 打开一个会话并启动它的 Runner。
//
// 这里完成一次"把会话变成可写状态"的装配：
//
//	打开 Store -> 加载会话并抢锁 -> 按会话 workspace 装配 Bash
//	-> 创建事件广播 Hub -> 注入依赖构造 Agent
//	-> 修复上次中断 -> 启动 loop 主循环 -> 返回控制句柄
//
// 返回的是 *Runner 指针，不是执行结果：真正的模型与命令调用发生在此后主循环派发
// 的子 goroutine 里。启动主循环与返回句柄不冲突——前者让 Runner 开始接收消息，
// 后者让上层继续调用 Submit / Hub / Stop。
//
// 会话被别的进程持有时，Store.Load 会返回 store.ErrSessionBusy，这里原样往上传，
// HTTP 层映射成 409。
func NewRunner(
	dataDirectory string,
	sessionID domain.SessionID,
	newModel func(string) ModelClient,
	defaultModel string,
	defaultContextWindow int,
	logger *slog.Logger,
) (*Runner, error) {
	//返回的store本质上就是一个指定数据库地址的connection
	sessions, err := store.New(dataDirectory)
	if err != nil {
		return nil, err
	}
	// 按 id 加载会话，并取得该会话的独占文件锁。加载失败（例如已被 CLI 占锁）
	// 必须关闭刚打开的 Store，避免泄漏数据库句柄。
	session, err := sessions.Load(sessionID)
	if err != nil {
		_ = sessions.Close()
		return nil, err
	}

	// 工具在会话自己的工作目录下执行，而不是服务进程的启动目录：历史里全是关于
	// 那个目录的事实，换地方执行会让模型收到一连串无法解释的"文件不存在"。
	tools, skillNames, err := sessionTools(session, dataDirectory)
	if err != nil {
		_ = sessions.Close()
		return nil, err
	}
	// 每个会话一个 Hub：后续 SSE （Server-Sent Events）连接订阅它，实时收到这个会话产生的事件。
	// 同一个客户端同时充当决策模型和摘要器：它们打的是同一个供应商，只是请求
	// 形态不同（见 model.Client.Summarize）。
	modelName := session.Model
	if modelName == "" {
		modelName = defaultModel
	}
	contextWindow := defaultContextWindow
	if entry, found := model.Lookup(modelName); found {
		contextWindow = entry.ContextWindow
	}
	client := newModel(modelName)

	hub := NewHub(logger)
	ctx, cancel := context.WithCancel(context.Background())
	// 装配 Runner。newModel() 在这里才真正创建该会话自己的模型客户端，而不是
	// 服务启动时共享一个；tools、sessions、hub 都作为运行一"轮"所需的依赖注入。
	runner := &Runner{
		dataDirectory:        dataDirectory,
		skillNames:           skillNames,
		sessionID:            sessionID,
		session:              session,
		store:                sessions,
		newModel:             newModel,
		defaultModel:         defaultModel,
		defaultContextWindow: defaultContextWindow,
		hub:                  hub,
		logger:               logger.With("session_id", string(sessionID)),
		ctx:                  ctx,
		cancel:               cancel,
		commands:             make(chan command),
		stopped:              make(chan struct{}),
	}
	// agent 的 sink 用 turnStateSink 包装 hub：它转发全部事件，额外把
	// state.changed 记进 runner.turnState（见该类型注释）。必须在 runner
	// 构造完成之后再装配——包装器要拿着 runner 的指针才能写入它的字段。
	runner.agent = agent.New(client, tools, sessions, runner.turnStateSink(hub), client, contextWindow)

	runner.agent.SetWindowErrorParser(model.ContextWindowFromError)
	// 工具产出的图片落进会话图片存储：截图等观察成为会话事实，前端可以直接渲染。
	runner.agent.SetImageStore(
		filepath.Join(dataDirectory, "images"),
		sessions.SaveImage,
	)
	// 让用户能边跑边说：Runner 自己就是那个队列。
	runner.agent.SetInjector(runner)
	// 走到收尾诊断不是正常工作状态，记 warn。界面那边由 completed 事件的 reason
	// 字段负责显示——两处都要有：日志是给回头查问题的人看的，界面是给此刻在用的人看的。
	runner.agent.SetCompactionDiagnosisReporter(func(id domain.SessionID, reason string) {
		runner.logger.Warn("压缩走到了收尾诊断", "session", id, "reason", reason)
	})
	// 估算的契约是"上界"，被击穿要大声报出来——它意味着压缩的终止性保证有洞。
	runner.agent.SetEstimateBreachReporter(func(breach domain.EstimateBreach) {
		runner.logger.Error("token 估算被击穿",
			"estimated", breach.Estimated, "actual", breach.Actual,
			"ratio", breach.Ratio(), "used_factor", breach.UsedFactor)
	})

	// 上一次运行可能被中断，留下没有观察的工具调用。带着这样的历史无法再请求
	// 模型，因此在接受任何提交之前先修好它。
	repaired, err := runner.agent.Restore(session)
	if err != nil {
		cancel()
		_ = sessions.Close()
		return nil, err
	}
	if len(repaired) > 0 {
		// Restore 不重放命令，只补写"结果未知"的观察；这条日志是给运维看的记录。
		runner.logger.Info("修复了上次中断留下的工具调用", "count", len(repaired))
	}
	// 启动常驻主循环。它只负责收提交和收停止，真正的轮次由它派发到子 goroutine。
	go runner.loop()
	// 返回句柄。此后上层对 runner.Submit 的调用，会经 commands 通道改变 loop 的行为。
	return runner, nil
}

// sessionTools 按一个会话装配它的工具集。
//
// 抽出来是因为它有**两个**调用方：Runner（真正执行工具）和快照处理器（只要
// Specs() 去估算上下文占用）。两处各写一遍的话，M5 加一个新工具而漏改其中一处，
// 快照里报的占用就会和实际请求对不上——而那种偏差不会报错，只会让仪表盘长期
// 少算一截，很难发现。
//
// 工具在会话自己的工作目录下执行，而不是服务进程的启动目录：历史里全是关于那个
// 目录的事实，换地方执行会让模型收到一连串无法解释的"文件不存在"。
// conversation_history 绑定的是会话**指针**，因此它看到的永远是最新历史。
func sessionTools(session *domain.Session, dataDirectory string) (*tool.Registry, []string, error) {
	skillsDir := filepath.Join(dataDirectory, "skills")

	// 插件加载：扫描 pluginsRoot，把每个插件的 skills/ 和 bin/ 汇进来。
	// 插件的 skill 用"插件名:skill名"作命名空间前缀，避免和用户 skill 撞名。
	pluginsRoot := filepath.Join(dataDirectory, "plugins")
	ensureBuiltinPlugins(dataDirectory, pluginsRoot)
	plugins, err := plugin.LoadAll(pluginsRoot)
	if err != nil {
		plugins = nil
	}
	var pluginSkillDirs []string
	var pluginBinDirs []string
	var skillNames []string
	for _, p := range plugins {
		for _, skillName := range p.SkillNames {
			pluginSkillDirs = append(pluginSkillDirs, p.SkillsDir())
			skillNames = append(skillNames, p.Name+":"+skillName)
			break // LoadAll 已按目录扫描，每个插件只需加一次 skillsDir
		}
		pluginBinDirs = append(pluginBinDirs, filepath.Join(p.Dir, "bin"))
	}

	// 用户自己的 skill 也进清单（无前缀）。
	userSkills, _ := os.ReadDir(skillsDir)
	for _, entry := range userSkills {
		if entry.IsDir() {
			skillNames = append(skillNames, entry.Name())
		}
	}

	bash := tool.NewBash(session.Workspace)
	bash.SetEnv("GOSEEK_SESSION_ID", string(session.ID))
	bash.SetExtraPath(pluginBinDirs)
	coreTools := []tool.Tool{
		bash,
		tool.NewReadFile(session.Workspace),
		tool.NewWriteFile(session.Workspace),
		tool.NewSearch(session.Workspace),
		tool.NewListSkills(append([]string{skillsDir}, pluginSkillDirs...)...),
		tool.NewLoadSkill(append([]string{skillsDir}, pluginSkillDirs...)...),
		tool.NewHistory(session),
	}
	// MCP server 接入：读取 dataDir/mcp.json，连接每个配置的 server，
	// 把它们的工具追加进注册表。连接失败静默跳过（server 不在就正常降级）。
	coreTools = append(coreTools, connectMCPServers(dataDirectory)...)

	registry, err := tool.NewRegistry(coreTools...)
	if err != nil {
		return nil, nil, err
	}
	return registry, skillNames, nil
}

// connectMCPServers 按 dataDir/mcp.json 的配置连接 MCP server，
// 返回它们的工具适配器。没有配置文件时返回 nil。
func connectMCPServers(dataDirectory string) []tool.Tool {
	configPath := filepath.Join(dataDirectory, "mcp.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var configs []mcp.Config
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil
	}
	manager, err := mcp.NewManager(context.Background(), configs)
	if err != nil {
		return nil
	}
	var adapters []tool.Tool
	for _, adapter := range manager.Tools() {
		adapters = append(adapters, adapter)
	}
	return adapters
}

// Hub 返回这个会话的事件广播器，供 SSE 处理器订阅。
func (runner *Runner) Hub() *Hub {
	// 对runner来说，hub是私有 private的
	// 因此使用该方法对外暴露hub，类似java的Get
	return runner.hub
}

// Submit 把一条用户消息投递给主循环，并报告它是否被接受。
//
// 它**不等待这一轮跑完**：返回 nil 只表示这一轮已经开始。真实结果由事件流观察，
// 这正是 HTTP 层返回 202 而不是 200 的原因。
//
// 工作方式：现场构造一个带 reply 通道的 submitRequest，经 commands 交给 loop。
// commands 是无缓冲通道，因此这一行会阻塞到 loop 真正接收为止，天然实现了
// "确认提交已进入调度"的握手。
// 语义在 M5.2 变了：**空闲则开一轮，运行中则排队注入**，两者都返回 nil。原来
// 运行中返回 ErrTurnInProgress，那是在惩罚用户的耐心——主流 Agent 允许边跑边说，
// 发现模型跑偏了补一句就能纠正。排队的消息会在 Agent 下一次请求模型之前被追加
// 进历史（见 Pending）。
func (runner *Runner) Submit(
	ctx context.Context,
	content string,
	ambientContext string,
	images ...domain.MessageImage,
) error {
	// 先看是不是有一轮在跑。这个判断可能立刻过期，但两个方向都不会出错：
	//   · 判成"在跑"而其实刚结束 → 消息进队列，那一轮的收尾会发现队列非空
	//     并立刻开新的一轮（drainPendingIntoNewTurn）；
	//   · 判成"空闲"而其实刚开始 → dispatch 走到 start，CAS 失败返回
	//     ErrTurnInProgress，此时再入队。
	if runner.running.Load() {
		runner.enqueue(content, ambientContext, images...)
		return nil
	}
	if err := runner.dispatchContext(ctx, command{
		kind: commandTurn, content: content, images: images, ambientContext: ambientContext,
	}); err != nil {
		if errors.Is(err, ErrTurnInProgress) {
			runner.enqueue(content, ambientContext, images...)
			return nil
		}
		return err
	}
	return nil
}

// enqueue 把一条消息放进注入队列。
func (runner *Runner) enqueue(content, ambientContext string, images ...domain.MessageImage) {
	runner.turnMutex.Lock()
	defer runner.turnMutex.Unlock()
	runner.pending = append(runner.pending, agent.PendingMessage{
		Content: content, Images: images, AmbientContext: ambientContext,
	})
}

// Pending 实现 agent.Injector：取走并清空当前排队的消息。
//
// Agent 在每次请求模型之前调它。取走即清空，因此同一条消息不会被注入两次。
func (runner *Runner) Pending() []agent.PendingMessage {
	runner.turnMutex.Lock()
	defer runner.turnMutex.Unlock()
	if len(runner.pending) == 0 {
		return nil
	}
	taken := runner.pending
	runner.pending = nil
	return taken
}

// CancelTurn 中止当前这一轮，Runner 继续活着。
//
// 和 Stop 的区别是本质的：Stop 关掉整个会话（释放文件锁、关 Hub），而这里只取消
// 这一轮的 context——命令的进程组被杀、模型请求被中断，然后走已有的失败路径：
// turn.failed 落库，已完成的工具调用和观察都保留（它们真的发生过），不留悬空调用。
//
// 没有正在跑的一轮时返回 ErrNoTurnRunning，让界面能说清楚"没什么可停的"。
func (runner *Runner) CancelTurn() error {
	runner.turnMutex.Lock()
	cancel := runner.cancelTurn
	runner.turnMutex.Unlock()

	if cancel == nil || !runner.running.Load() {
		return ErrNoTurnRunning
	}
	cancel()
	return nil
}

// Compact 请求压缩一次上下文，并报告它是否被接受。
//
// 和 Submit 走同一条队列、同一个 running 标记，因此一轮在跑时它得到
// ErrTurnInProgress，反过来也一样。它同样**不等待压缩跑完**——压缩要几十秒，
// 过程和结果通过事件流观察。
func (runner *Runner) Compact() error {
	return runner.dispatch(command{kind: commandCompact})
}

// EditMemory 把一段活跃摘要换成人工修订版，并报告是否成功。
//
// 和 Submit / Compact 不同，它**等到真正做完才返回**：这件事只是一次校验加一次
// UPDATE，毫秒级，而调用方（HTTP 的 PATCH）需要知道结果——改没改成、为什么没改成，
// 都要立刻告诉用户。压缩和一轮交互要几十秒，那才需要"接受即返回 + 事件流看结果"。
//
// 它同样受 running 标记约束：一轮在跑时返回 ErrTurnInProgress。
func (runner *Runner) EditMemory(batchID domain.MemoryBatchID, content string) error {
	return runner.dispatch(command{
		kind: commandEditMemory, batchID: batchID, content: content,
	})
}

// SetModel 切换会话模型，并报告是否成功。
//
// 和 EditMemory 一样同步返回：调用方需要立刻知道切换是否生效；模型切换本身没有
// 模型调用，毫秒级即可完成。
func (runner *Runner) SetModel(model string) error {
	return runner.dispatch(command{kind: commandSetModel, model: model})
}

// dispatch 把一件工作投递给主循环，并等它回报是否被接受。
//
// commands 是无缓冲通道，因此这一行会阻塞到 loop 真正接收为止，天然实现了
// "确认已进入调度"的握手。
// runner.command收到内容，则会触发loop的select的case work，这个命令就会传入runner.start方法进行实际的执行
// 产生reply，返回数据
func (runner *Runner) dispatch(work command) error {
	return runner.dispatchContext(context.Background(), work)
}

func (runner *Runner) dispatchContext(ctx context.Context, work command) error {
	if ctx == nil {
		ctx = context.Background()
	}
	work.ctx = ctx
	work.reply = make(chan error, 1)
	select {
	case runner.commands <- work:
		// 发送成功后，阻塞等待 loop 把启动结果写回 reply。
		select {
		case err := <-work.reply:
			return err
		case <-runner.stopped:
			return errors.New("会话已关闭")
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-runner.stopped:
		// loop 已退出（Runner 停止），无人再接收，不能再傻等 commands 发送。
		return errors.New("会话已关闭")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 停止 Runner：取消正在跑的一轮，等它收尾，然后释放会话锁。
//
// 先 cancel 再等待，顺序不能反：不取消就等，会一直等到那一轮自然结束——而它可能
// 正卡在一条长命令上。
func (runner *Runner) Stop() {
	runner.cancel()
	<-runner.stopped
	runner.turnDone.Wait()
	runner.hub.Close()
	if err := runner.store.Close(); err != nil {
		runner.logger.Error("关闭会话失败", "error", err)
	}
	runner.logger.Info("会话已关闭")
}

// editMemory 校验并写入一段摘要的人工修订版。
//
// 只在主循环里被调用，因此不需要额外加锁——那正是让它走这条队列的原因。
//
// 顺序是**先落库、再改内存**。反过来的话，一次失败的写入会留下"内存里改了、库里
// 没改"的状态，而下一次保存也不会把它带过去（Save 只插入新增节点，不重写已有的），
// 于是刷新页面修订就消失了，可界面刚刚显示过"已保存"。
func (runner *Runner) editMemory(batchID domain.MemoryBatchID, content string) error {
	// 领域规则在前：只有活跃前沿上的节点可以改。改一个已经被合并进上层的节点，
	// 模型看到的东西一个字都不会变。
	// 此处拿到的就是edit content被修改后的摘要内容
	// 当然，原content是不变的
	updated, err := runner.session.Memory.EditBatch(batchID, content)
	if err != nil {
		// 包成哨兵错误让 HTTP 层能映射成 409，但**不再套一层前缀**——领域那句话
		// 本身就说清楚了，前缀只会让用户读到两遍同样的意思。
		return fmt.Errorf("%w（%s）", ErrBatchNotActive, err)
	}
	// 落库，进行一个数据库的update操作
	if err := runner.store.EditMemoryBatch(runner.sessionID, batchID, content); err != nil {
		return err
	}
	runner.session.Memory = updated
	runner.logger.Info("摘要已修订", "batch_id", string(batchID), "reverted", content == "")
	return nil
}

// setModel 校验并切换模型。它只在主循环里被调用，因此不需要额外加锁。
func (runner *Runner) setModel(modelName string) error {
	if !model.Contains(modelName) {
		return fmt.Errorf("%w: %s", ErrUnknownModel, modelName)
	}
	if len(runner.session.UnresolvedToolCalls()) > 0 {
		return ErrUnresolvedToolCalls
	}
	if err := runner.store.SetModel(runner.sessionID, modelName); err != nil {
		return err
	}

	entry, _ := model.Lookup(modelName)
	client := runner.newModel(modelName)
	tools, _, err := sessionTools(runner.session, runner.dataDirectory)
	if err != nil {
		return err
	}
	runner.agent = agent.New(client, tools, runner.store, runner.turnStateSink(runner.hub), client, entry.ContextWindow)
	runner.agent.SetImageStore(
		filepath.Join(runner.dataDirectory, "images"),
		runner.store.SaveImage,
	)
	runner.agent.SetAvailableSkills(runner.skillNames)
	runner.agent.SetWindowErrorParser(model.ContextWindowFromError)
	runner.agent.SetInjector(runner)
	runner.session.Model = modelName
	runner.logger.Info("模型已切换", "model", modelName)
	return nil
}

// drainPendingIntoNewTurn 在一轮收尾时把还没被注入的消息开成新的一轮。
//
// 竞态是这样发生的：用户在模型吐最终回复的同时按了发送，Agent 的循环随即退出，
// 队列里那条消息永远等不到下一个注入点。**不能丢**——对用户来说他明明发出去了。
//
// 用 go 而不是直接 dispatch：这里是在子 goroutine 的 defer 里，而 dispatch 要等
// 主循环接收，主循环此刻可能正等着这个子 goroutine 结束（Stop 的路径），
// 直接调会死锁。
func (runner *Runner) drainPendingIntoNewTurn() {
	runner.turnMutex.Lock()
	waiting := len(runner.pending)
	runner.turnMutex.Unlock()
	if waiting == 0 {
		return
	}

	go func() {
		// 取走时再拼一次：这中间可能又多了几条。
		taken := runner.Pending()
		if len(taken) == 0 {
			return
		}
		// 多条合成一轮：它们本来就是连着发的，分成多轮只会让模型多跑几遍。
		// 图片各自归属原消息，第一条带第一批图片，其余依次追加。
		var content strings.Builder
		var allImages []domain.MessageImage
		for index, message := range taken {
			if index > 0 {
				content.WriteString("\n\n")
			}
			content.WriteString(message.Content)
			allImages = append(allImages, message.Images...)
		}
		var ambient strings.Builder
		for index, message := range taken {
			if index > 0 {
				ambient.WriteString("\n\n")
			}
			ambient.WriteString(message.AmbientContext)
		}
		if err := runner.dispatch(command{
			kind: commandTurn, content: content.String(), images: allImages,
			ambientContext: ambient.String(),
		}); err != nil {
			runner.logger.Warn("排队的消息没能开启新的一轮", "error", err, "count", len(taken))
		}
	}()
}

// loop 是 Runner 的常驻调度循环，也是每个会话唯一的命令接收者。
//
// 它只做两件很快的事，因此始终可响应：
//
//  1. 从 commands 收到一次提交 -> 交给 startTurn，把启动结果写回这次提交的 reply；
//  2. ctx 被取消 -> 退出循环。
//
// 真正的模型与命令调用不在这个循环里，而是由 startTurn 派发到子 goroutine。
// 正因如此，一轮跑几十秒也不会卡住 loop 去接收停止信号或下一次提交。
//
// defer close(runner.stopped) 保证只要 loop 退出，所有正在 Submit 等待的调用会
// 立刻从"会话已关闭"分支收到通知，而不是永久阻塞。
func (runner *Runner) loop() {
	defer close(runner.stopped)

	for {
		select {
		case work := <-runner.commands:
			// 把"是否被接受"写回 reply 后立刻回到循环，不等待它真正跑完。
			work.reply <- runner.start(work)
		case <-runner.ctx.Done():
			return
		}
	}
}

// start 尝试启动一件工作，并在子 goroutine 里跑完它。
//
// 它本身只做三件很快的事：占据 running 标记、登记等待计数、开子 goroutine。
// agent.Handle / agent.Compact 那几十秒的工作发生在子 goroutine 里，所以本函数能
// 立刻返回，loop 不需要跟着阻塞。
//
// 两种工作共用同一个 running 标记，因此一轮在跑时压缩会被拒，反之亦然。
func (runner *Runner) start(work command) error {
	if work.ctx != nil {
		select {
		case <-work.ctx.Done():
			return work.ctx.Err()
		default:
		}
	}
	// CompareAndSwap 保证"检查是否空闲"和"占住"是一步完成的。虽然当前只有循环
	// 这一个 goroutine 会调它，但清零发生在子 goroutine 里，用原子量表达这层
	// 跨 goroutine 的关系比用普通变量诚实。
	// 当前是 false（空闲）就原子地改成 true（占住）；已经是 true 说明已有轮在跑。
	if !runner.running.CompareAndSwap(false, true) {
		return ErrTurnInProgress
	}
	// turnDone 计数加一：Stop 通过 Wait 等这一轮跑完再释放会话锁。sync.waitgroup
	// 修订摘要是同步的：一次校验加一次 UPDATE，毫秒级。放进子 goroutine 反而要
	// 多一条回传结果的通道，而调用方本来就在等这个结果。做完立刻放开 running。
	if work.kind == commandEditMemory {
		defer runner.running.Store(false)
		//摘要编辑
		return runner.editMemory(work.batchID, work.content)
	}
	if work.kind == commandSetModel {
		defer runner.running.Store(false)
		return runner.setModel(work.model)
	}

	// 为这一轮派生一个可单独取消的 context，取消函数存起来供 CancelTurn 用。
	// 派生自 runner.ctx，因此 Stop 仍然能连带取消它。
	turnCtx, cancelTurn := context.WithCancel(runner.ctx)
	runner.turnMutex.Lock()
	runner.cancelTurn = cancelTurn
	runner.turnMutex.Unlock()

	runner.turnDone.Add(1)
	// 真正执行 Agent 编排的子 goroutine。两个 defer 就是这一轮的收尾：
	//   1. running 清回 false，放开下一轮提交；
	//   2. turnDone 减一，通知等待者这一轮结束了。
	go func() {
		defer runner.turnDone.Done()
		// 这一轮的 context 用完就释放，避免泄漏。
		defer cancelTurn()
		// 收尾时队列里若还有东西，说明用户在这一轮结束前发了消息而它没来得及被
		// 注入。不能丢——立刻用它开新的一轮。
		//
		// defer 是后进先出，所以它在 running 被清零**之后**执行，新一轮的 CAS
		// 才不会因为标记还占着而失败。
		defer runner.drainPendingIntoNewTurn()
		// 运行结束（无论成功或失败）都要让 running 回到空闲。
		defer runner.running.Store(false)

		// 过程和结果全部通过事件流出去，因此这里只记失败——返回值没有别的去处。
		// 失败本身已经作为事件推给了订阅者，日志是给运维看的第二份记录。
		switch work.kind {
		case commandTurn:
			if _, err := runner.agent.HandleWithAmbient(
				turnCtx, runner.session, work.content, work.ambientContext, work.images...,
			); err != nil {
				runner.logger.Warn("一轮交互失败", "error", err)
			}
		//适用于手动要求执行压缩指令
		case commandCompact:
			if err := runner.agent.Compact(turnCtx, runner.session); err != nil {
				runner.logger.Warn("手动压缩失败", "error", err)
			}
		case commandEditMemory:
			// 不会走到这里：修订在 start 里就同步做完了，不派子 goroutine。
			// 留一个分支是为了让 switch 覆盖全部取值——新增一种工作时，
			// 漏掉这里会得到一个静默什么都不做的 goroutine。
		}
	}()
	return nil
}

// BuiltinPluginsFS 由 cmd 层注入：go:embed 只能在包目录使用，而内置插件打包在
// cmd/goseek/plugins 下，由启动代码赋值。nil 表示没有内置插件。
var BuiltinPluginsFS fs.FS

// ensureBuiltinPlugins 把编译进二进制的内置插件解压到数据目录。
//
// Skill 和 manifest 只复制缺失文件，不覆盖用户改动；bin/ 是随程序构建的生成物，
// 嵌入版本变化时必须原子更新，否则 App 升级后仍会执行旧工具。
func ensureBuiltinPlugins(dataDirectory, pluginsRoot string) {
	if BuiltinPluginsFS == nil {
		return
	}
	entries, err := fs.Sub(BuiltinPluginsFS, "plugins")
	if err != nil {
		return
	}
	err = fs.WalkDir(entries, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return nil
		}
		target := filepath.Join(pluginsRoot, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		executable := strings.Contains(filepath.ToSlash(path), "/bin/")
		if _, statErr := os.Stat(target); statErr == nil && !executable {
			return nil // 已存在，不覆盖
		}
		data, readErr := fs.ReadFile(entries, path)
		if readErr != nil {
			return nil
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		// bin/ 目录下的文件是可执行脚本，必须保留执行位；
		// 其余（manifest、skill、命令文档）用普通文件权限。
		if executable {
			return replaceBuiltinExecutable(target, data)
		}
		return os.WriteFile(target, data, 0o644)
	})
	_ = err
}

func replaceBuiltinExecutable(target string, data []byte) error {
	if current, err := os.ReadFile(target); err == nil && bytes.Equal(current, data) {
		return os.Chmod(target, 0o755)
	}

	file, err := os.CreateTemp(filepath.Dir(target), ".goseek-builtin-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o755); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, target)
}

// turnStateSink 把 hub 包成会维护 runner.turnState 的 EventSink。
//
// 为什么要拦一道：事件流是"发生过什么"的流水，而侧栏和快照需要的是"此刻在做什么"。
// state.changed 经过这里时顺带把答案记进 turnState，之后任何时刻来问（running
// 端点、快照）都能拿到与真实执行同步的答案，不需要翻事件历史去推断。
//
// 包装只在转发之外多一次原子写，不改变事件的顺序、内容与失败语义——事件推送
// 失败不能影响 Agent 的结果，这条底线由 hub 本身保证，这里同样不破坏它。
func (runner *Runner) turnStateSink(sink agent.EventSink) agent.EventSink {
	return &turnStateRecorder{runner: runner, wrapped: sink}
}

// turnStateRecorder 是 turnStateSink 的实现。
type turnStateRecorder struct {
	runner  *Runner
	wrapped agent.EventSink
}

// Emit 转发事件给底层 sink；state.changed 事件额外更新 runner.turnState。
func (recorder *turnStateRecorder) Emit(event domain.RunEvent) {
	if event.Type == domain.EventStateChanged {
		var payload domain.StateChangedPayload
		if err := json.Unmarshal(event.Payload, &payload); err == nil {
			recorder.runner.turnState.Store(payload.State)
		}
	}
	recorder.wrapped.Emit(event)
}

// TurnState 返回当前轮进行到的步骤。没有轮在跑时返回空串——"在不在跑"由
// running 回答，这里只回答"跑到了哪一步"。
func (runner *Runner) TurnState() domain.RunState {
	if !runner.running.Load() {
		return ""
	}
	if state, ok := runner.turnState.Load().(domain.RunState); ok {
		return state
	}
	// 轮已经占住但第一条 state.changed 还没到：它一开跑就会写入 WAITING_MODEL，
	// 这个间隙按"等待模型"报告是诚实且无歧义的。
	return domain.StateWaitingModel
}
