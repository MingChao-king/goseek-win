// Package agent 编排一轮交互：接收用户消息、构建上下文视图、调用模型、执行模型
// 提出的工具，把观察交回模型，直到模型给出最终回复。
//
// Agent 只做编排，不做语义判断。用户输入原样进入历史，不做意图分类、文件名识别或
// URL 提取；调用哪个工具、什么时候收口由模型决定，程序只负责"这次调用能不能执行"
// 和"实际发生了什么"。
//
// 一轮交互中发生的每件事都会变成一个 RunEvent。事件是 Agent 对外唯一的过程表达：
// 终端、将来的 SSE 和事件重放消费的是同一份东西，因此事件描述的是"发生了什么"，
// 而不是"该怎么画"。
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"goseek/internal/contextmgr"
	"goseek/internal/domain"
)

// Model 是 Agent 对模型能力的要求：给一组消息和一组工具定义，拿回文字与工具调用。
//
// 接口定义在调用方而不是实现方，因为需求由 Agent 提出——供应商协议、认证、重试和
// 错误分类都属于实现细节，Agent 不感知。
//
// onDelta 接收流式产生的文字片段，只用于实时展示；它的失败不影响这次调用的结果。
type Model interface {
	Complete(ctx context.Context, request domain.ModelRequest, onDelta domain.DeltaFunc) (domain.ModelResponse, error)
}

// Tools 是 Agent 对可用工具集合的要求。
type Tools interface {
	// Specs 返回随请求发给模型的全部工具定义。
	Specs() []domain.ToolSpec
	// Describe 返回一次调用的一行人类可读标题，用于展示执行步骤。
	Describe(call domain.ToolCall) string
	// Execute 执行一次调用，返回与它配对的唯一观察。
	//
	// 它不返回 error：未知工具、参数非法和执行失败都是模型需要读到并自行纠正的
	// 事实。用签名堵住"工具失败即本轮失败"这种写法，比用注释提醒可靠。
	//
	// onOutput 接收执行过程中的输出片段，用于实时展示。
	Execute(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) domain.ToolResult
}

// Store 是 Agent 对持久化的要求。
//
// 只声明这一个方法。会话的创建、列举、加载和加锁都是入口装配时的事，Agent 不需要
// 知道它们，也就不该在接口里看到它们。
//
// 传入的事件与会话的新增消息必须在同一个事务里提交；返回的是已经分配好 sequence
// 的 durable 事件，供调用方在提交成功之后推送。
type Store interface {
	Save(session *domain.Session, events []domain.RunEvent) ([]domain.RunEvent, error)
}

// EventSink 接收一轮交互中产生的全部事件。
//
// Emit **不返回错误**，这是行为底线第 12 条的落点：事件推送失败不能改变 Agent 的
// 结果。用签名堵住"推送失败就中断本轮"这种写法。
//
// 与之相对，durable 事件的**持久化**失败必须终止本轮——但那条路走的是 Store.Save。
// "推送"和"持久化"是两件事，走两条路。
//
// 实现方不能假设 Emit 只被一个 goroutine 调用：工具输出的片段来自 os/exec 的拷贝
// goroutine。当前它与 Agent 自身的事件不会同时发生（执行工具期间 Agent 正阻塞在
// 里面），但这个前提不该被实现方依赖。
type EventSink interface {
	Emit(event domain.RunEvent)
}

// Injector 提供运行中排队的用户消息。
//
// 它让用户能**边跑边说**：一轮交互动辄几十秒，发现模型跑偏了补一句"不用管测试，
// 先把编译过了"，下一步就能采纳。这是主流 Agent 的常规能力，而"禁用输入框"
// 是在惩罚用户的耐心。
//
// 接口定义在消费方（本包），实现是 httpapi.Runner。**CLI 传 nil**——终端那边
// stdin 正阻塞在读取上，本来就没法边跑边输入。
type Injector interface {
	// Pending 取走并清空当前排队的消息，按提交顺序返回。
	//
	// 语义是"取走"而不是"查看"：取完就清空，同一条消息不会被注入两次。
	Pending() []PendingMessage
}

// PendingMessage 是运行中排队等待注入的一条用户消息，含可选图片。
type PendingMessage struct {
	Content        string
	Images         []domain.MessageImage
	AmbientContext string
}

// Agent 持有一轮交互所需的依赖，本身不保存任何会话状态。
//
// 会话由调用方传入，因此同一个 Agent 可以服务多个会话。
type Agent struct {
	model Model
	tools Tools
	store Store
	sink  EventSink
	// summarizer 供压缩使用。nil 表示不启用压缩。
	//
	// 它和 model 是同一个供应商客户端，但走的是完全不同的一种请求：独立的 system
	// 指令、不带工具、不让模型回答用户。用一个窄接口把这件事表达出来，比传一个
	// 完整的 Model 再约定"记得别传 tools"要可靠。
	summarizer contextmgr.Summarizer
	// thresholds 是压缩的三条水位线，由上下文窗口算出。
	thresholds contextmgr.Thresholds
	// contextWindow 是模型一次调用的总上下文容量，由配置提供。
	//
	// 0 表示用户没有配置。此时仍然会统计输入 token（那个数字本身有意义），
	// 只是算不出占用比例——不猜一个窗口大小，因为假的窗口会让 M4.2 的压缩
	// 在错误的时机触发。
	contextWindow int
	// now 提供事件时间，测试用固定时钟替换。
	now func() time.Time
	// safetyFactor 是 token 估算的安全系数，见 domain.DefaultSafetyFactor。
	//
	// 它是**可变的**：估算的契约是"上界"，而上界一旦被实测值击穿，就必须立刻
	// 调高，否则压缩的终止性保证有洞。由 requestModel 在每次拿到供应商的
	// prompt_tokens 之后校验并调整。
	//
	// 用互斥量而不是原子量：读写的是 float64，而且要和 onEstimateBreach 的回调
	// 一起构成一次完整的"发现并修正"，不能被切开。
	factorMutex  sync.Mutex
	safetyFactor float64
	// onEstimateBreach 在上界被击穿时被调用，供装配方记日志。
	//
	// 用回调而不是给 Agent 塞一个 logger：编排层不该关心日志去哪，而击穿这件事
	// 必须被大声报出来（它意味着一个保证不成立），所以也不能只是静默调整。
	onEstimateBreach func(domain.EstimateBreach)
	// onCompactionDiagnosis 在压缩走到收尾诊断时被调用，供装配方记日志。
	//
	// 同样是回调而不是 logger，理由与 onEstimateBreach 相同。而它同样必须被大声
	// 报出来：走到收尾诊断意味着光用户原话、或者单独一轮原文就撑到了目标线，
	// 那是一个"该结束这个会话、或者窗口配小了"的信号，不是可以静默处理的偏差。
	onCompactionDiagnosis func(sessionID domain.SessionID, reason string)
	// injector 提供运行中排队的用户消息。nil 表示不支持注入。
	injector Injector
	// availableSkills 是启动时扫描到的 skill 名称列表，注入 system prompt。
	// 为空时不追加任何内容。
	availableSkills []string
	// parseWindowFromError 从模型错误里读出供应商声明的真实窗口，读不出返回 0。
	//
	// 用函数而不是接口：它只有一个方法，而且 agent 包不 import model 包。
	// nil 表示不解析（CLI 之外的装配路径可以不注入）。
	parseWindowFromError func(error) int
	// imageStoreDir 是工具产出图片的存储根目录。
	//
	// 非空时 Agent 在工具执行后将 ToolResult.Images 里的文件拷贝到
	// <imageStoreDir>/<sessionID>/ 下并登记，让图片成为会话事实的一部分。
	// 空表示不支持工具图片（CLI 模式、测试）。
	imageStoreDir string
	// imageRegistrar 把一张已落盘的图片登记进持久层。
	//
	// 与 imageStoreDir 成对出现；nil 表示不登记（测试场景）。
	imageRegistrar func(image domain.MessageImage, sessionID domain.SessionID, byteSize int64) error

	// futileMutex 保护 futileCompaction。一个 Agent 可以服务多个会话，
	// 而 HTTP 那边不同会话的轮次是并发跑的。
	futileMutex sync.Mutex
	// futileCompaction 记录每个会话"在可压缩区终点等于这个值时，压缩什么也
	// 压不出来"。
	//
	// 压缩的输入完全由（游标, 可压缩区终点）决定，而失败时游标不动，所以只需要
	// 记终点。同样的输入不会得到不同的结果——上次没压出东西，这次同样压不出。
	//
	// 为什么值得记：可压缩区的终点由**轮次**决定，一轮之内不变，而一轮里可能
	// 请求模型五六次（每次工具调用之后都要再问一次）。没有这个记录，一轮就要
	// 白花五六次摘要调用。等用户说下一句话，轮次增加、终点前移，记录自然失效，
	// 压缩会重新尝试。
	//
	// 它只是省钱的优化，不是正确性的一部分：进程重启后记录丢了，最多多花一次
	// 调用，所以不落库。
	// 值为，某个会话压缩注定白做是，当时可压缩区域的终点下标。
	// 即，在这个会话中，当可压缩终点为X时，压缩一点东西都压不出来
	futileCompaction map[domain.SessionID]int
}

// SetInjector 注入"运行中排队消息"的来源。
//
// 和另外两个 setter 一样是可选能力：不设就是不支持边跑边说（CLI 就是这样）。
func (agent *Agent) SetInjector(injector Injector) {
	agent.injector = injector
}

// injectPending 把排队的用户消息追加进历史，返回是否真的注入了。
//
// # 注入点为什么只能在请求模型之前
//
// 工具执行到一半注入没有意义——观察还没产生，模型也不在等。而这里恰好是**上下文
// 即将被重新组装**的时刻，注入的消息自然进入下一次请求；它同时也在压缩判断之前，
// 所以注入的内容会被正常计入占用。
//
// # 为什么沿用当前 TurnID
//
// TurnID 的含义是"一个工作单元"，不是"一次问答"。注入是对正在进行的这个单元的
// **引导**。这不是新情况：splitTurns 从 M3.1 起就按 TurnID 而不是"遇到 user
// 消息"划界，注释里写的理由正是"一轮里其实可能出现多条 user 消息"。
//
// 落库仍然守检查点纪律：追加、记事件、提交，然后才请求模型。
func (agent *Agent) injectPending(session *domain.Session, log *turnLog) ([]string, bool) {
	if agent.injector == nil {
		return nil, false
	}
	pending := agent.injector.Pending()
	if len(pending) == 0 {
		return nil, false
	}

	ambient := make([]string, len(pending))
	hasAmbient := false
	for _, content := range pending {
		session.Append(domain.Message{
			Role: domain.RoleUser, Content: content.Content, TurnID: log.turnID, Images: content.Images,
		})
		imageIDs := make([]string, 0, len(content.Images))
		for _, image := range content.Images {
			imageIDs = append(imageIDs, image.ID)
		}
		log.record(domain.EventUserMessage, domain.UserMessagePayload{Content: content.Content, ImageIDs: imageIDs})
	}
	for index, content := range pending {
		ambient[index] = content.AmbientContext
		hasAmbient = hasAmbient || strings.TrimSpace(content.AmbientContext) != ""
	}
	if !hasAmbient {
		return nil, true
	}
	return ambient, true
}

// SetCompactionDiagnosisReporter 注入"压缩走到收尾诊断"的报告口。
func (agent *Agent) SetCompactionDiagnosisReporter(report func(domain.SessionID, string)) {
	agent.onCompactionDiagnosis = report
}

// SetEstimateBreachReporter 注入"上界被击穿"的报告口。
//
// 和 SetWindowErrorParser 一样是可选的诊断能力，不是运行必需的依赖。
func (agent *Agent) SetEstimateBreachReporter(report func(domain.EstimateBreach)) {
	agent.onEstimateBreach = report
}

// currentSafetyFactor 读当前的安全系数。
func (agent *Agent) currentSafetyFactor() float64 {
	agent.factorMutex.Lock()
	defer agent.factorMutex.Unlock()
	return agent.safetyFactor
}

// checkEstimate 用供应商报的真值校验估算的上界性质。
//
// 这是整个系统里唯一能验证"估算 ≥ 实际"的地方——供应商的 prompt_tokens 是真值，
// 而它只在请求发出之后才有。
//
// 击穿时做两件事：调高系数让上界立刻重新成立，以及**大声报出来**。只调不报是
// 不够的：上界被击穿说明我们对内容构成的假设错了（比如出现了大量 base64），
// 那是需要人知道的事，不是一个可以自动咽下去的偏差。
func (agent *Agent) checkEstimate(estimated, actual int) {
	if estimated <= 0 || actual <= estimated {
		return
	}

	agent.factorMutex.Lock()
	breach := domain.EstimateBreach{
		Estimated: estimated, Actual: actual, UsedFactor: agent.safetyFactor,
	}
	agent.safetyFactor = domain.RaisedSafetyFactor(agent.safetyFactor, breach)
	agent.factorMutex.Unlock()

	if agent.onEstimateBreach != nil {
		agent.onEstimateBreach(breach)
	}
}

// SetWindowErrorParser 注入"从错误里读真实窗口"的解析器。
//
// 单独一个 setter 而不是加进 New 的参数表：它是可选的诊断能力，不是运行必需的
// 依赖，塞进构造函数会让每个调用方（包括测试）都被迫关心它。
func (agent *Agent) SetWindowErrorParser(parse func(error) int) {
	agent.parseWindowFromError = parse
}

// SetAvailableSkills 注入 skill 名称列表，供 system prompt 展示。
func (agent *Agent) SetAvailableSkills(names []string) {
	agent.availableSkills = names
}

// SetImageStore 注入工具产出图片的存储能力。
//
// storeDir 是图片根目录（通常为 <dataDir>/images）；registrar 把登记信息写入
// 持久层。两者都提供时，工具返回的 ToolResult.Images 会成为会话事实的一部分。
func (agent *Agent) SetImageStore(storeDir string, registrar func(domain.MessageImage, domain.SessionID, int64) error) {
	agent.imageStoreDir = storeDir
	agent.imageRegistrar = registrar
}

// registerToolImages 把工具产出的图片落进会话图片存储，返回可随 tool 消息保存的引用。
//
// 工具（如浏览器截图脚本）只负责把文件写到临时路径；这里把它拷贝到
// <imageStoreDir>/<sessionID>/，生成图片 ID，探测尺寸并登记进持久层。
// 没有配置图片存储时返回原样引用，工具自己决定那些文件的命运。
//
// 返回的 error 只在"登记失败"时非 nil——那意味着图片事实无法持久化，本轮必须
// 终止（与消息保存失败同级），而不是把一张丢了的图伪装成成功观察。
func (agent *Agent) registerToolImages(result *domain.ToolResult, sessionID domain.SessionID) error {
	if len(result.Images) == 0 {
		return nil
	}
	if agent.imageStoreDir == "" || agent.imageRegistrar == nil {
		return nil
	}
	dir := filepath.Join(agent.imageStoreDir, string(sessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建工具图片目录失败: %w", err)
	}
	registered := make([]domain.MessageImage, 0, len(result.Images))
	for index, source := range result.Images {
		if source.FilePath == "" {
			continue
		}
		data, err := os.ReadFile(source.FilePath)
		if err != nil {
			return fmt.Errorf("读取工具图片 %s 失败: %w", source.FilePath, err)
		}
		id, err := newImageID()
		if err != nil {
			return fmt.Errorf("生成图片 ID 失败: %w", err)
		}
		mediaType := source.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		ext := filepath.Ext(source.FilePath)
		if ext == "" {
			ext = ".png"
		}
		filePath := filepath.Join(dir, id+ext)
		if err := os.WriteFile(filePath, data, 0o600); err != nil {
			return fmt.Errorf("写入工具图片失败: %w", err)
		}
		width, height := decodeImageSize(data)
		image := domain.MessageImage{
			ID:        id,
			FilePath:  filePath,
			MediaType: mediaType,
			Width:     width,
			Height:    height,
		}
		if err := agent.imageRegistrar(image, sessionID, int64(len(data))); err != nil {
			os.Remove(filePath)
			return fmt.Errorf("登记工具图片失败: %w", err)
		}
		registered = append(registered, image)
		_ = index
	}
	result.Images = registered
	return nil
}

// extractImageMarkers 从工具输出中提取 [goseek-image:路径] 标记，
// 并把它们构造成 ToolResult.Images。
//
// 这是 bash 工具与 Agent 之间的轻量协议：bash 只知道 stdout 文本，不知道
// 命令是否产出了图片；由命令脚本输出标记、Agent 解析，保持 bash 工具的
// 通用性。路径必须存在且可读，否则标记被静默忽略——那张图本来就不存在，
// 伪装成功比忽略更糟。
func extractImageMarkers(result *domain.ToolResult) {
	if !strings.Contains(result.Content, "[goseek-image:") {
		return
	}
	const marker = "[goseek-image:"
	images := make([]domain.MessageImage, 0)
	searchStart := 0
	for {
		start := strings.Index(result.Content[searchStart:], marker)
		if start < 0 {
			break
		}
		start += searchStart + len(marker)
		end := strings.Index(result.Content[start:], "]")
		if end < 0 {
			break
		}
		end += start
		filePath := strings.TrimSpace(result.Content[start:end])
		if filePath != "" {
			if _, err := os.Stat(filePath); err == nil {
				mediaType := "image/png"
				if filepath.Ext(filePath) == ".jpg" || filepath.Ext(filePath) == ".jpeg" {
					mediaType = "image/jpeg"
				}
				images = append(images, domain.MessageImage{
					FilePath:  filePath,
					MediaType: mediaType,
				})
			}
		}
		searchStart = end
	}
	result.Images = append(result.Images, images...)
}

// newImageID 生成一个带 img_ 前缀的随机图片 ID，与上传图片共用同一种命名。
func newImageID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "img_" + hex.EncodeToString(random), nil
}

// decodeImageSize 用标准库解析 PNG/JPEG/GIF 头部拿像素尺寸。
// 解析失败返回 0，只影响 token 估算精度，不阻塞图片展示。
func decodeImageSize(data []byte) (int, int) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
}

// New 构造一个 Agent。
//
// 四个依赖都是装配期决定的通道，因此在这里注入；会话是每次调用的数据，走参数。
// contextWindow 同属装配期配置。
func New(
	model Model,
	tools Tools,
	store Store,
	sink EventSink,
	summarizer contextmgr.Summarizer,
	contextWindow int,
) *Agent {
	return &Agent{
		model:            model,
		tools:            tools,
		store:            store,
		sink:             sink,
		summarizer:       summarizer,
		thresholds:       contextmgr.NewThresholds(contextWindow),
		contextWindow:    contextWindow,
		now:              time.Now,
		futileCompaction: make(map[domain.SessionID]int),
		safetyFactor:     domain.DefaultSafetyFactor,
	}
}

// Handle 处理一条用户消息，循环调用模型与工具，直到模型给出最终回复。
//
// 循环的终止条件由模型决定：只有"非空文字 + 没有工具调用"的响应才结束本轮。程序
// 不判断"任务是否完成"，只判断这次调用能不能执行、实际发生了什么。
//
// 用户消息在调用模型之前就已追加，且在后续失败时不会被撤回：它是已经发生的事实，
// 历史 append-only。因此一次失败之后重试，历史里可能出现相邻的两条 user 消息。
//
// 本轮中途失败时，已经完成的工具调用及其观察都留在历史里——它们真的发生过。失败
// 只保证不会留下没有观察的工具调用，否则下一次请求会因为悬空调用而无法发出。
//
// 每一次外部调用之前，历史和事件都必须已经落库。保存失败即本轮终止：继续请求模型
// 或者执行命令，会产生数据库里没有记录的事实，而进程随时可能在下一刻消失。
func (agent *Agent) Handle(ctx context.Context, session *domain.Session, userInput string, images ...domain.MessageImage) (string, error) {
	return agent.HandleWithAmbient(ctx, session, userInput, "", images...)
}

// HandleWithAmbient 处理一条用户消息，并让本次模型请求额外看到临时界面上下文。
// ambient 不进入历史、事件或摘要；HTTP 面板用它传递发送瞬间的侧栏状态。
func (agent *Agent) HandleWithAmbient(
	ctx context.Context,
	session *domain.Session,
	userInput string,
	ambient string,
	images ...domain.MessageImage,
) (string, error) {
	// 生成这一轮的 TurnID。本轮内所有消息和事件都带它，前端据此把片段归成一组。
	turnID, err := domain.NewTurnID()
	if err != nil {
		return "", err
	}
	// 为这一轮创建事件收集器：durable 事件攒着等落库，transient 事件立刻推给 sink。
	log := newTurnLog(agent.store, agent.sink, turnID, agent.now)

	// 用户原文原样进历史，不做意图分类、文件名识别或 URL 提取。
	session.Append(domain.Message{Role: domain.RoleUser, Content: userInput, TurnID: turnID, Images: images})
	// 以下三个 durable 事件组成这一轮的开场：轮次开始、用户消息、状态切到等待模型。
	log.record(domain.EventTurnStarted, nil)
	imageIDs := make([]string, 0, len(images))
	for _, image := range images {
		imageIDs = append(imageIDs, image.ID)
	}
	log.record(domain.EventUserMessage, domain.UserMessagePayload{Content: userInput, ImageIDs: imageIDs})
	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateWaitingModel})
	// 第一个检查点：用户消息与开场事件在同一事务落库。失败则本轮不请求模型，
	// 因为继续调用会产生数据库里没有记录的事实。
	if err := log.commit(session); err != nil {
		return "", fmt.Errorf("保存用户消息失败，本轮没有请求模型: %w", err)
	}

	// 编排循环：请求模型 -> 判断响应 -> 直接收口或执行工具后再请求，直到最终回复。
	var requestAmbient []string
	if strings.TrimSpace(ambient) != "" {
		requestAmbient = []string{ambient}
	}
	for {
		response, err := agent.requestModel(ctx, session, log, requestAmbient)
		requestAmbient = nil
		if err != nil {
			return "", log.fail(session, err)
		}

		// 先校验再写入历史。顺序不能反：一旦带 ToolCalls 的 assistant 消息落了库，
		// 本轮就必须为其中每个调用产生观察，而 id 为空或重复的调用根本无法配对，
		// 写进去就成了永久的悬空调用。
		if err := validateNewToolCalls(session, response.ToolCalls); err != nil {
			return "", log.fail(session, fmt.Errorf("模型返回的工具调用无法使用: %w", err))
		}

		if response.IsFinal() {
			// 无工具调用且文字非空：这是给用户的最终回复，保存并收口本轮。
			reply, err := agent.completeTurn(session, log, response)
			if err != nil {
				return "", err
			}
			return reply, nil
		}

		// 有工具调用：执行它们并把观察交回模型，循环回到顶部再次请求。
		if err := agent.runToolCalls(ctx, session, log, response); err != nil {
			return "", log.fail(session, err)
		}
	}
}

// requestModel 请求一次模型，并把流式片段作为 transient 事件推出去。
func (agent *Agent) requestModel(
	ctx context.Context,
	session *domain.Session,
	log *turnLog,
	ambient []string,
) (domain.ModelResponse, error) {
	// 模型流式输出的每一片文字都会进这里。思考与正文分成两种事件，都只实时展示、
	// 不落库；最终完整回复由 Complete 归一化后返回。
	// onDelta就负责流式处理消息
	onDelta := func(delta domain.TextDelta) {
		eventType := domain.EventAssistantDelta
		// 部分模型把思考过程与正文分开输出，用 Reasoning 标记区分。
		if delta.Reasoning {
			eventType = domain.EventAssistantReasoningDelta
		}
		log.record(eventType, domain.TextDeltaPayload{Text: delta.Text})
	}

	// 注入点：把用户在这一轮进行中发来的消息追加进历史。必须在组装视图之前——
	// 否则这一次请求看不到它，用户会觉得自己那句话被吞了。
	injectedAmbient, injected := agent.injectPending(session, log)
	if injected {
		// 和普通用户消息一样守检查点纪律：先落库再请求模型。失败则本轮终止，
		// 继续下去会产生数据库里没有记录的事实。
		if err := log.commit(session); err != nil {
			return domain.ModelResponse{}, fmt.Errorf("保存注入的消息失败: %w", err)
		}
	}
	ambient = append(ambient, injectedAmbient...)

	// 上下文视图由 contextmgr 生成（system 指令 + 活跃摘要 + 游标之后的原文），
	// 工具定义由 Agent 直接提供。工具定义也参与 token 计数——它们每次请求都完整
	// 发送，一个带 Schema 的定义可能上百 token，漏算会让估算系统性偏低，
	// 而偏低会让请求被供应商拒。
	tools := agent.tools.Specs()
	// 摘要，工具，未被摘要化的消息、系统提示词
	view := contextmgr.BuildWithAmbient(
		session, session.Memory, tools, agent.contextWindow,
		agent.currentSafetyFactor(), agent.availableSkills, ambient,
	)

	// 达到触发线、且这次压缩不是注定白做，就先压缩再请求。压缩改的是
	// session.Memory，因此之后要重新构建视图——用压缩前的视图去请求，等于白压。

	// 1.上下文超限
	// 2.压缩是有意义的
	// 3.压缩无意义的情况：
	// 4.对话轮次较少，产生的信息较少，如果摘要比原文还长，那么就产生了负收益，那么此点就被记录为压缩无用点，没有继续压缩的意义
	// 在之后的对话中，随着轮次增加，相应情况也会得到解决
	// 5.压无可压了，无论怎么压缩，都不会产生新的摘要，几乎不会发生，对ds来说，除非某个对话的最后一轮可以产生近1M的上下文
	if agent.thresholds.ShouldCompact(view.Usage.InputTokens) && agent.compactionMayHelp(session) {
		// 压缩完紧接着就是模型请求，因此下一个状态是 WAITING_MODEL。
		if err := agent.compact(
			ctx, session, log, tools, view.Usage.InputTokens, domain.StateWaitingModel,
		); err != nil {
			return domain.ModelResponse{}, err
		}
		// 压缩完了要重新构建上下文
		view = contextmgr.BuildWithAmbient(
			session, session.Memory, tools, agent.contextWindow,
			agent.currentSafetyFactor(), agent.availableSkills, ambient,
		)
	}

	// 先报一次估算值。请求可能要跑几十秒，用户在这段时间里就该看到"这一次占了
	// 多少"，而不是等结果回来才知道。
	log.record(domain.EventContextUsageUpdated, domain.NewContextUsagePayload(view.Usage))

	// 空响应重试：思考模型（GLM 5.3 等）偶尔会把输出预算全花在思考上，
	// 正文一个字不写就 finish_reason=stop。这不是用户能修复的配置错误，
	// 直接判死整轮非常伤——自动重试，最多 2 次；重试前在末尾补一条
	// **临时** user 消息点模型"请给出回复"。它只影响请求，不进会话历史，
	// 因此重试成功后的对话流里看不到这条提示。
	const emptyRetryLimit = 2
	var response domain.ModelResponse
	var lastErr error
	for attempt := 0; ; attempt++ {
		messages := view.Messages
		if attempt > 0 {
			messages = append(messages, domain.ModelMessage{
				Role: domain.ModelRoleUser,
				Content: "上一次响应没有任何正文或工具调用。" +
					"请直接给出回复，或调用工具继续任务——不要只输出思考过程。",
			})
		}
		response, lastErr = agent.model.Complete(ctx, domain.ModelRequest{
			Messages: messages,
			Tools:    tools,
		}, onDelta)
		if lastErr == nil && !response.IsEmpty() {
			break
		}
		if lastErr != nil {
			// 超窗等可诊断错误不重试，原样上抛（错误信息里可能带真实窗口大小）。
			if actual := agent.windowFromError(lastErr); actual > 0 && actual != agent.contextWindow {
				return domain.ModelResponse{}, fmt.Errorf(
					"模型调用失败: %w\n配置的上下文窗口是 %d，而供应商说这个模型实际是 %d。"+
						"用 GOSEEK_CONTEXT_WINDOW=%d 改过来",
					lastErr, agent.contextWindow, actual, actual)
			}
			return domain.ModelResponse{}, fmt.Errorf("模型调用失败: %w", lastErr)
		}
		// 空响应且已到重试上限：带上 finish_reason 让诊断有据可查。
		if attempt >= emptyRetryLimit {
			return domain.ModelResponse{}, fmt.Errorf(
				"模型连续 %d 次返回空响应（finish_reason=%q，供应商%d）——本轮无法继续。"+
					"通常是模型把输出预算花在了思考过程上；稍后重试或换一个模型",
				attempt+1, response.FinishReason, response.PromptTokens)
		}
	}
	// 供应商报了真实输入量就用它覆盖估算值，来源标成 provider——界面据此告诉
	// 用户这个数字有多可信。不据此反向校准估算系数：一个偶然的长回复会永久
	// 扭曲后续估算，而估算只需要"大致对"。
	if response.PromptTokens > 0 {
		// 先校验上界：估算的契约是"估算 ≥ 实际"，而这里是唯一能验证它的地方。
		agent.checkEstimate(view.Usage.InputTokens, response.PromptTokens)
		log.record(domain.EventContextUsageUpdated,
			domain.NewContextUsagePayload(view.Usage.WithProviderTokens(
				response.PromptTokens, response.CacheHitTokens, response.CacheMissTokens)))
	}
	return response, nil
}

// completeTurn 保存最终回复并收口本轮。
func (agent *Agent) completeTurn(
	session *domain.Session,
	log *turnLog,
	response domain.ModelResponse,
) (string, error) {
	// 最终回复作为 assistant 消息进历史，并记三个收口事件。
	session.Append(domain.Message{
		Role:    domain.RoleAssistant,
		Content: response.Content,
		TurnID:  log.turnID,
	})
	log.record(domain.EventAssistantMessage, domain.AssistantMessagePayload{
		Content: response.Content,
		Final:   true,
	})
	log.record(domain.EventTurnCompleted, nil)
	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateIdle})

	// 最终检查点：保存失败要如实报告，不能假装这轮成功。
	if err := log.commit(session); err != nil {
		return "", fmt.Errorf("最终回复已经生成，但保存失败，这条回复没有被记录: %w", err)
	}
	return response.Content, nil
}

// runToolCalls 保存带工具调用的 assistant 消息，然后按顺序串行执行它们。
func (agent *Agent) runToolCalls(
	ctx context.Context,
	session *domain.Session,
	log *turnLog,
	response domain.ModelResponse,
) error {
	// 走到这里说明响应带有工具调用，随附的文字只是过程说明，不是给用户的回复。
	// 先把它作为 assistant 消息连同 ToolCalls 一起进历史。
	session.Append(domain.Message{
		Role:      domain.RoleAssistant,
		Content:   response.Content,
		ToolCalls: response.ToolCalls,
		TurnID:    log.turnID,
	})
	// 依旧是用于给前端展示
	log.record(domain.EventAssistantMessage, domain.AssistantMessagePayload{
		Content:   response.Content,
		ToolCalls: response.ToolCalls,
		Final:     false,
	})
	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateRunningTool})

	// 把"刚发生的事"和"下一步要做什么"合并成同一次保存：每次落库后数据库里都是
	// 一个自洽的检查点，而 N 个调用的一轮只需要 N+2 次写而不是 2N+2 次。
	//
	// "下一步要做什么"有两种表达，它们说的是同一件事，因此必须在同一个事务里：
	// PendingToolCallID 给崩溃恢复看（区分"可能已执行"和"根本没开始"），
	// tool.started 事件给重放看（让界面显示"正要执行这个命令"）。分开提交的话，
	// 进程可以死在中间，留下两者互相矛盾的状态。
	//
	// 取 [0] 是本批里马上要执行的那一个，循环里再逐个往后推。下标不会越界：
	// IsEmpty 与 IsFinal 同时为 false 时，若 ToolCalls 为空，就要求这次响应的
	// 文字既非空（否则 IsEmpty 成立）又为空（否则 IsFinal 成立），自相矛盾。
	// 声明第一个调用即将执行，并把它和上面的 assistant 消息合并成同一次保存。
	agent.announceToolCall(log, session, response.ToolCalls[0])
	if err := log.commit(session); err != nil {
		return fmt.Errorf("保存模型响应失败，工具没有执行: %w", err)
	}

	// 按模型返回的顺序串行执行：并发执行本机副作用会产生竞态，
	// 也无法在界面上表达步骤之间的因果。
	for index, call := range response.ToolCalls {
		// 命令输出的每一片都作为 transient 事件实时推出去，长命令不必等结束。
		onOutput := func(chunk string) {
			log.record(domain.EventToolOutputDelta, domain.ToolOutputDeltaPayload{
				ToolCallID: call.ID,
				Chunk:      chunk,
			})
		}
		result := agent.tools.Execute(ctx, call, onOutput)

		// bash 工具的输出可能包含 [goseek-image:路径] 标记（截图脚本等），
		// 提取并构造成 ToolResult.Images。
		extractImageMarkers(&result)

		// 工具产出的图片先落进会话图片存储并登记，再随 tool 消息绑定。
		// 登记失败与消息保存失败同级：本轮终止，不让一张丢了的图伪装成成功。
		if err := agent.registerToolImages(&result, session.ID); err != nil {
			return log.fail(session, err)
		}

		// write_file 成功后发 file.changed 事件，供前端侧边栏展示。
		if call.Name == "write_file" && result.Status == domain.ToolSuccess {
			var args struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(call.Arguments, &args) == nil && args.Path != "" {
				log.record(domain.EventFileChanged, domain.FileChangedPayload{
					Path:   args.Path,
					TurnID: string(log.turnID),
				})
			}
		}

		// 观察作为 tool 消息进历史，ToolCallID 与调用配对。
		session.Append(domain.Message{
			Role:       domain.RoleTool,
			Content:    result.EncodeContent(),
			ToolCallID: result.ToolCallID,
			TurnID:     log.turnID,
			Images:     result.Images,
		})
		imageIDs := make([]string, 0, len(result.Images))
		for _, image := range result.Images {
			imageIDs = append(imageIDs, image.ID)
		}
		log.record(domain.EventToolResolved, domain.ToolResolvedPayload{Result: result, ImageIDs: imageIDs})

		if next := index + 1; next < len(response.ToolCalls) {
			// 还有下一个调用：把它声明出去，和这条观察一起提交。
			agent.announceToolCall(log, session, response.ToolCalls[next])
		} else {
			// 本批执行完了，下一步是再次请求模型，清掉 pending 并切回等待模型。
			session.PendingToolCallID = ""
			log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateWaitingModel})
		}

		// 每个调用执行完都是一个检查点：观察和"下一步要做什么"在同一事务落库。
		if err := log.commit(session); err != nil {
			return fmt.Errorf("保存工具观察失败，本轮终止: %w", err)
		}
	}
	return nil
}

// announceToolCall 声明"下一个要执行的是这个调用"。
//
// 它只改状态、记事件，不提交——调用方负责把它和刚发生的事合并进同一次保存。
func (agent *Agent) announceToolCall(log *turnLog, session *domain.Session, call domain.ToolCall) {
	// PendingToolCallID 在执行前落盘，崩溃恢复据此区分"可能已执行"和"根本没开始"。
	session.PendingToolCallID = call.ID
	log.record(domain.EventToolStarted, domain.ToolStartedPayload{
		Call:  call,
		Title: agent.tools.Describe(call),
	})
}

// Restore 让一个刚从数据库读入的会话重新变得可以继续。
//
// 进程可能正好死在"带工具调用的 assistant 消息已落库"和"观察已落库"之间，
// 留下没有配对观察的调用。带着这样的历史无法再请求模型：供应商要求每个工具调用
// 都有对应的观察。因此这里为每个悬空调用补上一条诚实的观察。
//
// 补什么取决于那条命令到底跑没跑，而这只能靠执行前落下的 PendingToolCallID 区分。
// 无论哪种情况都**不重新执行**：命令可能是 rm -rf，重放的代价不可逆，而"结果未知"
// 是可以如实告诉模型、让它自己先去确认当前状态的。
//
// 返回补上的观察，供入口向用户说明上次运行发生了什么。会话本来就自洽时返回 nil，
// 也不产生任何写入。
func (agent *Agent) Restore(session *domain.Session) ([]domain.ToolResult, error) {
	// 找出历史里模型已提出、但还没有配对观察的悬空工具调用。
	open := session.UnresolvedToolCalls()
	if len(open) == 0 {
		return nil, nil
	}

	// 补出来的观察属于被中断的那一轮，事件也挂在它上面。取最后一条消息的 turn，
	// 因为悬空调用必然出现在历史末尾——它之后不可能再有别的轮次开始。
	history := session.Messages()
	log := newTurnLog(agent.store, agent.sink, history[len(history)-1].TurnID, agent.now)

	repaired := make([]domain.ToolResult, 0, len(open))
	for _, call := range open {
		result := interruptedResult(call, call.ID == session.PendingToolCallID)
		// 这里不重放命令：只为悬空调用补一条"结果未知/没有执行"的诚实观察。
		repaired = append(repaired, result)
		session.Append(domain.Message{
			Role:       domain.RoleTool,
			Content:    result.EncodeContent(),
			ToolCallID: result.ToolCallID,
			TurnID:     log.turnID,
		})
		log.record(domain.EventToolResolved, domain.ToolResolvedPayload{Result: result})
	}
	session.PendingToolCallID = ""

	// 那一轮确实没有正常收口，事件序列必须如实反映——否则重放出来的历史里会有
	// 一轮永远停在"正在执行工具"。
	log.record(domain.EventTurnFailed, domain.TurnFailedPayload{Reason: "上次运行被中断"})
	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateIdle})

	if err := log.commit(session); err != nil {
		return nil, fmt.Errorf("保存中断修复结果失败: %w", err)
	}
	return repaired, nil
}

// interruptedResult 为一个被中断的调用构造观察。
//
// started 表示这个调用已经被声明为"即将执行"，也就是进程死在执行期间。此时程序
// 无法知道命令有没有真的跑完，因此如实说明结果未知，并提示模型先确认当前状态——
// 而不是替它断定成功或失败。
func interruptedResult(call domain.ToolCall, started bool) domain.ToolResult {
	content := fmt.Sprintf("GoSeek 进程在这次调用之前中断，工具 %q 没有执行。", call.Name)
	if started {
		content = fmt.Sprintf(
			"GoSeek 进程在工具 %q 执行期间中断。这条命令可能已经执行并产生了副作用，"+
				"也可能根本没有开始，结果无法确定。不要假设它成功或失败；"+
				"如果后续判断依赖它的结果，请先重新检查当前状态。", call.Name)
	}
	return domain.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Status:     domain.ToolError,
		Content:    content,
	}
}

// validateNewToolCalls 确认这批调用的 id 非空，且在整个会话中没有出现过。
//
// 供应商协议靠 id 把观察和调用对上，重复 id 会让配对产生歧义。会话内唯一比响应内
// 唯一更严格，而只有这里能看到完整历史。
func validateNewToolCalls(session *domain.Session, calls []domain.ToolCall) error {
	if len(calls) == 0 {
		return nil
	}

	used := make(map[string]struct{})
	for _, message := range session.Messages() {
		for _, call := range message.ToolCalls {
			used[call.ID] = struct{}{}
		}
	}

	for index, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("第 %d 个工具调用没有 id", index+1)
		}
		if _, duplicate := used[call.ID]; duplicate {
			return fmt.Errorf("工具调用 id %q 在本会话中已经出现过", call.ID)
		}
		used[call.ID] = struct{}{}
	}
	return nil
}

// nextPendingToolCall 返回本批里下一个待执行调用的 ID；已经是最后一个时返回空串。
func nextPendingToolCall(calls []domain.ToolCall, current int) string {
	if current+1 < len(calls) {
		return calls[current+1].ID
	}
	return ""
}

// Compact 按用户的明确要求压缩一次上下文。
//
// 它和自动压缩（requestModel 里那次）走的是**同一套机制**，只有两处不同：
//
//   - **不看触发线**。用户明确要求了，占用只有 30% 也照压。压完发现已经低于目标线、
//     无事可做时如实报告，而不是假装做了什么——CompactionResult.Changed() 为 false，
//     界面据此显示"无需压缩"。
//   - **绕过"注定白做"的记忆**。那个判断是为自动触发省钱用的；用户手动要求时，
//     让他白花一次调用也比"点了没反应"好。压完之后按真实结果重新记录。
//
// 它发一整套轮次事件（turn.started → COMPRESSING → compaction.* → IDLE →
// turn.completed），用一个新的 TurnID。复用轮次的信封而不是新造一种，是因为 TurnID
// 在这个系统里的含义本来就是"一次工作单元"而不是"一次问答"——这样 Runner 的
// running 标记、前端的分组、事件重放三处都不用改。
//
// 调用方必须保证它与普通轮次互斥（httpapi.Runner 用同一个 running 标记做到这点）：
// 压缩改的是下一次请求要用的视图，和一轮交互并发做，结果无从定义。
func (agent *Agent) Compact(ctx context.Context, session *domain.Session) error {
	if agent.summarizer == nil {
		return errors.New("没有配置摘要器，无法压缩")
	}

	turnID, err := domain.NewTurnID()
	if err != nil {
		return err
	}
	//事件收集器
	log := newTurnLog(agent.store, agent.sink, turnID, agent.now)
	//记录状态为turnStarted
	log.record(domain.EventTurnStarted, nil)

	// 先算一次当前占用，压缩事件要把它报出去（"为什么现在压"——手动压缩的答案是
	// "用户要求的"，但当前占用仍然是用户想知道的信息）。
	tools := agent.tools.Specs()
	//当前的消息，包含会话消息，system prompt，摘要，工具，另外当前的上下文情况也会传入，作为判断
	view := contextmgr.BuildWith(session, session.Memory, tools, agent.contextWindow, agent.currentSafetyFactor(), agent.availableSkills)

	// 手动压缩必然要试一次，因此先清掉"注定白做"的记录，让 compact 里那套记录
	// 逻辑从干净状态重新判断。
	//根据设计，压缩的会话，为压缩游标和最后的两轮对话之间的对话
	// 如果压缩游标根本没变，代表上一次压缩根本没有结果，那么这一轮也就不用再压缩，压了也是白压缩，所以是注定白做
	agent.forgetCompactionOutcome(session)

	// 手动压缩之后什么都不做，因此下一个状态直接是 IDLE——中间不该出现
	// WAITING_MODEL，那会让界面闪一下一次根本不存在的模型请求。
	if err := agent.compact(
		ctx, session, log, tools, view.Usage.InputTokens, domain.StateIdle,
	); err != nil {
		return err
	}

	// 压缩之后重新报一次占用：用户点这个按钮就是想看到这个数字变小。
	after := contextmgr.BuildWith(session, session.Memory, tools, agent.contextWindow, agent.currentSafetyFactor(), agent.availableSkills)
	//记录现在的上下文大小
	log.record(domain.EventContextUsageUpdated, domain.NewContextUsagePayload(after.Usage))
	log.record(domain.EventTurnCompleted, nil)
	return log.commit(session)
}

// windowFromError 从模型返回的错误里读出供应商声明的真实上下文窗口。
//
// 它是一个字段而不是直接调 model 包的函数：`agent` 只依赖自己声明的接口，
// 不 import `model`（见包注释）。装配时由 cmd 注入，测试里可以留空。
func (agent *Agent) windowFromError(err error) int {
	if agent.parseWindowFromError == nil {
		return 0
	}
	return agent.parseWindowFromError(err)
}

// compactionMayHelp 判断这次压缩是不是注定白做。
//
// 上一次压缩什么也没压出来、而可压缩区从那以后没有变化，就说明输入完全相同，
// 结果也必然相同。此时跳过，省下一次摘要调用——这在一轮里要请求模型五六次的
// 场景下不是小数目。
func (agent *Agent) compactionMayHelp(session *domain.Session) bool {
	agent.futileMutex.Lock()
	defer agent.futileMutex.Unlock()
	//获取压缩无用点
	recorded, seen := agent.futileCompaction[session.ID]
	// 没有压缩无用点，或者说压缩无用点不等于可压缩终点
	return !seen || recorded != contextmgr.CompactableEnd(session)
}

// rememberCompactionOutcome 记下"压缩有没有产出"，供 compactionMayHelp 判断。
//
// 有产出就删掉记录：情况已经变了，下次该重新尝试。
func (agent *Agent) rememberCompactionOutcome(session *domain.Session, changed bool) {
	agent.futileMutex.Lock()
	defer agent.futileMutex.Unlock()

	if changed {
		delete(agent.futileCompaction, session.ID)
		return
	}
	// 记录压缩无用点
	agent.futileCompaction[session.ID] = contextmgr.CompactableEnd(session)
}

// forgetCompactionOutcome 清掉某个会话"压缩注定白做"的记录。
//
// 只在用户手动要求压缩时调用：那条记录是给自动触发省钱用的，不该挡住明确的指令。
func (agent *Agent) forgetCompactionOutcome(session *domain.Session) {
	agent.futileMutex.Lock()
	defer agent.futileMutex.Unlock()
	delete(agent.futileCompaction, session.ID)
}

// compact 把上下文压到目标线以下。
//
// # 失败为什么不算本轮失败
//
// 压缩是一项优化，不是必要步骤。它失败时，只要当前视图仍在硬边界之内，带着未压缩
// 的上下文继续请求是完全可行的——用户得到的仍是正确的回答，只是这一轮贵一些。
// 反过来，因为"总结失败"就让用户的问题得不到回答，是把手段当成了目的。
//
// 因此这里几乎不返回错误：压缩失败会记进 completed 事件的 Failed 字段，界面据此
// 提示，然后继续往下走。唯一会返回错误的是**保存失败**——那意味着新生成的摘要
// 节点没能落库，而内存里的 Memory 已经变了，两边不一致，继续下去会写出错乱的历史。
func (agent *Agent) compact(
	ctx context.Context,
	session *domain.Session,
	log *turnLog,
	tools []domain.ToolSpec,
	inputTokens int,
	nextState domain.RunState,
) error {
	if agent.summarizer == nil {
		return nil
	}

	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: domain.StateCompressing})
	//状态为压缩开始
	log.record(domain.EventContextCompactionStarted, domain.CompactionStartedPayload{
		InputTokens: inputTokens,
		Threshold:   agent.thresholds.CompactAt,
	})
	// 先把"开始压缩"发出去再动手：压缩可能要几十秒，界面得先知道它在做什么。
	// 落库，以及状态告知给浏览器
	if err := log.commit(session); err != nil {
		return fmt.Errorf("保存压缩起始检查点失败: %w", err)
	}

	// 每生成一个节点就报一次进度。一次压缩可能产生好几个节点，每个都是一次模型
	// 调用，全程没有反馈的话界面会看起来卡住。这些进度只推送不落库（transient），
	// 因为最终的 completed 事件已经完整记录了结果。
	compactor := contextmgr.NewCompactor(agent.summarizer, agent.thresholds, tools, nil)
	// 压缩内部的估算必须和真正发请求时用同一个系数，否则"压到目标线以下"这个
	// 结论是假的。
	// 一个上下文计算系数，用于调整系统上下文的预估值
	// 即当模型返回的实际上下文大于预估的上下文时，新系数=旧系数✖️（实际/预估）*1.05
	compactor.SetSafetyFactor(agent.currentSafetyFactor())
	//传入的memory即为摘要
	result, compactErr := compactor.Compact(ctx, session, session.Memory)

	// 记下这次有没有产出。压不出东西时，只要可压缩区不变，下次就直接跳过。
	// 失败（compactErr != nil）也算没产出——重试同样的输入没有意义。
	agent.rememberCompactionOutcome(session, result.Changed())

	// 无论成功与否，已经生成的节点都要保留：它们是真实的模型输出，扔掉等于白花
	// 那几次调用。Compact 保证返回的 Memory 始终满足不变量。
	// 这个memory，包含现有的所有摘要，以及实际在使用的摘要active
	session.Memory = result.Memory

	// 收尾诊断（第 3 步）动过手就记 warn。它不是正常工作状态：走到那里意味着
	// 光用户原话或单独一轮原文就撑到了目标线，而那是数以千计的用户消息、或者一轮
	// 里几十次大输出命令。这条日志是给人看的信号——该结束这个会话，或者窗口配小了。
	if result.Reason != "" && agent.onCompactionDiagnosis != nil {
		agent.onCompactionDiagnosis(session.ID, result.Reason)
	}

	completed := domain.CompactionCompletedPayload{
		BeforeTokens:      result.BeforeTokens,
		AfterTokens:       result.AfterTokens,
		TargetUnreachable: result.TargetUnreachable,
		Reason:            result.Reason,
		// 显式建空切片：nil 会序列化成 null，消费端就得在每个用到它的地方先判空。
		// 快照里的空前沿也是这么处理的，两处保持一致。
		Batches: make([]domain.MemoryBatchSummary, 0, len(result.NewBatches)),
	}
	// 把本次压缩完得到的摘要
	for _, batch := range result.NewBatches {
		completed.Batches = append(completed.Batches, domain.NewMemoryBatchSummary(batch))
	}
	if compactErr != nil {
		completed.Failed = compactErr.Error()
	}
	// 传递给前端展示
	log.record(domain.EventContextCompactionCompleted, completed)
	// 压缩之后进入哪个状态由**调用方**决定，因为那取决于接下来要做什么：自动压缩
	// 之后紧接着就是模型请求（WAITING_MODEL），而用户手动压缩之后什么都不做
	// （IDLE）。写死成 WAITING_MODEL 的话，手动压缩会在界面上闪一下"正在请求
	// 模型"——一次根本不存在的请求。
	log.record(domain.EventStateChanged, domain.StateChangedPayload{State: nextState})
	if err := log.commit(session); err != nil {
		return fmt.Errorf("保存压缩结果失败: %w", err)
	}
	return nil
}
