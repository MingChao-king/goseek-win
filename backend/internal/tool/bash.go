package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"goseek/internal/domain"
)

const (
	// defaultBashTimeout 是单条命令的默认超时，与 E3 使用的 environment.timeout 一致。
	// 用户可配置的 wall time 限额属于策略限额，在 M5 引入。
	defaultBashTimeout = 60 * time.Second

	// 这里曾经有一个 maxOutputBytes = 10000 的输出上限。**它已经被移除。**
	//
	// 后端不再对任何东西做截断：截断损害模型的判断，而且损害方式很隐蔽——
	// 模型拿到的是一段读起来连续、实际中间被挖掉的文本，于是它开始从可见的
	// 元数据里推断，而推断出来的东西和事实长得一模一样（见 output.go 里那个
	// "1173 个 man 页面"的实例）。展示层的折叠仍然保留，那不影响模型。

	// waitDelay 是进程组被杀之后等待输出管道关闭的宽限时间。
	// 没有它，一个仍然持有管道的残留进程会让 Wait 永久阻塞。
	waitDelay = 5 * time.Second

	// maxTitleRunes 限制展示标题的长度，避免模型写出的 purpose 撑破一行。
	maxTitleRunes = 120
)

// BashTool 在用户本机运行 Bash 命令。
//
// 它不是沙箱：命令以启动 GoSeek 的用户身份执行，只是被限定在一个工作目录里开始。
// 工作目录边界的校验（拒绝越界访问）和审批策略在 M5 引入。
type BashTool struct {
	// workspace 是命令的起始目录，等于会话创建时所在的目录。
	//
	// 会话恢复后命令仍在这里执行，而不是恢复时所处的目录：历史里全是关于这个
	// 目录的事实。为空时继承进程的当前目录。
	workspace string
	// timeout 是单条命令的超时。默认取 defaultBashTimeout，
	// 测试用更短的值验证超时路径；M5 会让它可配置。
	timeout time.Duration
	// extraPath 是插件 bin 目录列表，启动时追加到 PATH 末尾（用户自己的
	// PATH 优先，插件补充在内）。
	extraPath []string
	// env 是追加到子进程的环境变量（如 GOSEEK_SESSION_ID）。
	env []string
}

// NewBash 创建一个在给定工作目录下执行命令、使用默认超时的 Bash 工具。
// SetEnv 追加子进程的环境变量。
func (tool *BashTool) SetEnv(key, value string) {
	tool.env = append(tool.env, key+"="+value)
}

func NewBash(workspace string) *BashTool {
	return &BashTool{workspace: workspace, timeout: defaultBashTimeout}
}

// SetExtraPath 注入插件 bin 目录列表。
func (tool *BashTool) SetExtraPath(dirs []string) {
	tool.extraPath = dirs
}

// bashArguments 是 bash 工具接受的参数。
type bashArguments struct {
	// Command 是要执行的命令，必填。
	Command string `json:"command"`
	// Purpose 是模型对这条命令意图的自述，可选。
	// 它只用于生成展示标题，不传给 shell，也不作为命令成功的证据。
	Purpose string `json:"purpose"`
}

// Spec 返回发送给模型的 bash 工具定义。
func (tool *BashTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name: "bash",
		Description: "在用户本机运行一条 Bash 命令，返回合并后的 stdout、stderr 和退出码。" +
			"用它查看目录与文件、搜索内容、运行程序。命令在非交互环境中执行，不要使用需要人工输入的命令。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "要执行的 Bash 命令"
    },
    "purpose": {
      "type": "string",
      "description": "用一句话说明这条命令想达成什么，用于向用户展示当前步骤"
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`),
	}
}

// DescribeCall 返回这次调用的一行标题。
//
// 优先使用模型自述的 purpose；缺失或参数不可解析时回退到中性文案。标题只是展示，
// 真正的事实是随后展示的命令原文和退出码，因此这里不因参数错误而报错。
func (tool *BashTool) DescribeCall(call domain.ToolCall) string {
	arguments, err := parseBashArguments(call.Arguments)
	if err != nil {
		return "运行 Bash 命令"
	}
	purpose := singleLine(arguments.Purpose, maxTitleRunes)
	if purpose == "" {
		return "运行 Bash 命令"
	}
	return purpose
}

// Run 执行命令并把结果转换成模型可读的观察，同时把输出逐片交给 onOutput。
//
// # 返回的 error 恒为 nil
//
// 参数非法、非零退出、超时、进程根本没起来——这些都是**可以如实表达的执行事实**，
// 它们作为 error 状态的观察交回模型，由模型决定下一步（改命令、换个思路、告诉
// 用户）。返回 Go 的 error 则意味着"这一轮进行不下去了"，会让整轮交互失败。
// 一条 `ls 不存在的目录` 不该让用户的整轮提问作废。
//
// 唯一会返回 error 的情况这里没有：那要留给"程序自身出了问题"，比如工具注册表
// 找不到这个工具名——那不是命令执行的事实，是装配错了。
//
// # 一次执行的四种结局
//
// 下面的 switch 按**判断的可靠性**排序，不是按常见程度：先看 ctx 为什么结束
// （超时/取消），再看进程有没有正常跑完，最后才区分"跑完但非零"和"压根没启动"。
// 顺序反过来的话，一个被超时杀掉的进程会先落进 ExitError 分支，报出一个具有
// 误导性的"退出码 -1"，而真正的原因（超时）就丢了。
func (tool *BashTool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	arguments, err := parseBashArguments(call.Arguments)
	if err != nil {
		return domain.ToolResult{
			Status:  domain.ToolError,
			Content: fmt.Sprintf("参数不合法：%v。命令没有执行。", err),
		}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, tool.timeout)
	defer cancel()

	output := newCommandOutput(onOutput)

	// 使用非登录 shell，直接继承 GoSeek 进程已经准备好的 PATH。登录 shell 会
	// 执行 /etc/profile 和用户 profile；正式 App 中曾实测卡在 path_helper，
	// 导致连 echo 都耗满整条命令的 60 秒上限。
	// -c 表示"命令从参数里读"，而不是进入交互式 REPL。
	//
	// 整条命令作为**一个参数**交给 bash，不做任何切词：管道、重定向、&&、引号
	// 都要由 shell 自己解析。自己切词的话，`echo "a b"` 会变成两个参数。
	command := exec.CommandContext(runCtx, shellPath(), "-c", arguments.Command)
	command.Dir = tool.workspace
	env := os.Environ()
	if len(tool.extraPath) > 0 {
		path := strings.Join(tool.extraPath, pathListSeparator)
		if existing := os.Getenv("PATH"); existing != "" {
			path = existing + pathListSeparator + path
		}
		env = append(env, "PATH="+path)
	}
	env = append(env, tool.env...)
	command.Env = env

	// stdout 和 stderr 指向同一个 Writer，os/exec 会复用同一个管道和拷贝
	// goroutine，因此两路输出天然按时间顺序合并，也不需要额外加锁。
	command.Stdout = output
	command.Stderr = output

	// 让命令独立成一个进程组（Setpgid），并在取消时按进程组杀。
	//
	// 默认行为只杀 bash 自己这一个进程。但 `sleep 300 &` 这样的命令会让 bash
	// 立刻退出、把 sleep 留给系统托管——那个孙进程会一直活着占用资源，而我们
	// 连它的 PID 都不知道。设成独立进程组之后，整棵进程树共享一个 PGID，
	// 一次信号就能全覆盖。
	configureProcessGroup(command)
	// 杀掉进程组之后再等这么久，管道还没关就强行放弃等待。
	//
	// 没有它的话：某个残留进程仍然持有 stdout 管道的写端，os/exec 的拷贝
	// goroutine 就永远读不到 EOF，Run() 会一直阻塞——命令明明已经被杀了，
	// 这一轮却卡死在这里。
	command.WaitDelay = waitDelay

	runErr := command.Run()
	content := output.Result()

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return domain.ToolResult{
			Status: domain.ToolError,
			Content: fmt.Sprintf("命令超过 %s 仍未结束，整个进程组已被终止。已经产生的输出：\n%s",
				tool.timeout, content),
		}, nil

	// 不是超时而 ctx 结束了，说明是上层取消（用户按了 Ctrl-C、HTTP 请求断开）。
	// 措辞上必须说清"副作用无法确定"：命令可能已经删了文件才被杀掉，让模型
	// 以为什么都没发生，它下一步的判断就建立在错误的前提上。
	case runCtx.Err() != nil:
		return domain.ToolResult{
			Status: domain.ToolError,
			Content: fmt.Sprintf("命令被取消，整个进程组已被终止；已经发生的副作用无法确定。已经产生的输出：\n%s",
				content),
		}, nil

	// 正常跑完且退出码为 0。这是唯一的 success——"命令跑完了"不等于"做成了"，
	// 但退出码是这一层能得到的唯一客观信号，再往上的判断交给模型。
	case runErr == nil:
		exitCode := 0
		return domain.ToolResult{
			Status:   domain.ToolSuccess,
			Content:  content,
			ExitCode: &exitCode,
		}, nil
	}

	// 跑完了但退出码非零。这**不是**程序出错，是命令如实报告了失败
	// （grep 没匹配到、测试没通过、文件不存在……），所以照样是一条正常的观察，
	// 只是状态为 error，并且把退出码一并交给模型。
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode := exitErr.ExitCode()
		return domain.ToolResult{
			Status:   domain.ToolError,
			Content:  content,
			ExitCode: &exitCode,
		}, nil
	}

	// 走到这里说明进程根本没有启动成功（bash 不存在、工作目录不存在、
	// 权限不足……），没有退出码可言，只能把系统给的原因如实转述。
	return domain.ToolResult{
		Status:  domain.ToolError,
		Content: fmt.Sprintf("命令无法启动：%v。", runErr),
	}, nil
}

// parseBashArguments 解析并校验 bash 工具的参数。
//
// DisallowUnknownFields 对应 Schema 里的 additionalProperties=false：模型写错
// 字段名时立刻得到明确反馈，而不是让一条被忽略的参数造成难以理解的行为。
func parseBashArguments(raw json.RawMessage) (bashArguments, error) {
	var arguments bashArguments
	//流式解码
	decoder := json.NewDecoder(bytes.NewReader(raw))
	//严格模式，有未知字段直接报错
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&arguments); err != nil {
		return arguments, fmt.Errorf("参数不符合 bash 工具的定义：%w", err)
	}
	if strings.TrimSpace(arguments.Command) == "" {
		return arguments, errors.New("command 必须是非空字符串")
	}
	return arguments, nil
}

// singleLine 把文本压成一行并限制长度，用于展示标题。
//
// purpose 由模型生成，可能包含换行或很长的段落，直接打印会打乱步骤视图。
func singleLine(text string, limit int) string {
	//按空白切词 “你好   世界”切为“你好”、“世界”
	fields := strings.Fields(text)
	//单个空格拼回去
	joined := strings.Join(fields, " ")
	//使用rune，数的是字符数，防止数字节数，导致中文字符截断
	runes := []rune(joined)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return joined
}
