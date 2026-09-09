// Command goseek 是 GoSeek 的命令行入口。
//
// 它负责读取配置、选择或创建会话、装配依赖，并驱动读取-回复循环。业务编排在
// agent 包里，这里只处理进程边界：命令行参数、配置文件、标准输入输出和退出码。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"goseek/internal/agent"
	"goseek/internal/domain"
	"goseek/internal/model"
	"goseek/internal/store"
	"goseek/internal/tool"
)

// exitCommand 是退出交互循环的输入。
const exitCommand = "/exit"

// version 是当前 GoSeek 版本号（M6.6 起引入）。
const version = "1.0.0"

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "goseek:", err)
		os.Exit(1)
	}
}

// dispatch 按第一个参数分派到子命令。
//
// 只有 serve 一个子命令，其余情况一律走 CLI——这样 `goseek --session` 这类既有
// 用法不受影响，不必在每个参数前面再加一个 `chat` 之类的动词。
func dispatch(arguments []string) error {
	if len(arguments) > 0 && arguments[0] == "serve" {
		return runServe(arguments[1:])
	}
	if len(arguments) > 0 && (arguments[0] == "--version" || arguments[0] == "version") {
		fmt.Printf("goseek %s\n", version)
		return nil
	}
	//review：第四阶段彻底移除终端
	return run(arguments)
}

// newFlagSet 创建一个出错时不自动打印用法的参数集。
//
// flag 包默认会把用法打到 stderr 再返回错误，于是同一个问题被报告两次。
// 这里统一由 main 打印。
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// options 是命令行参数。
type options struct {
	// listSessions 表示列出最近会话并按序号选择。
	listSessions bool
	// resumeID 直接指定要恢复的会话。
	resumeID string
}

// parseOptions 解析命令行参数。
func parseOptions(arguments []string) (options, error) {
	var parsed options
	//全局参数 -goseek
	flags := newFlagSet("goseek")
	flags.BoolVar(&parsed.listSessions, "session", false, "列出最近会话并按序号选择一个恢复")
	flags.StringVar(&parsed.resumeID, "resume", "", "直接恢复指定 ID 的会话")
	//解析参数
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}
	if parsed.listSessions && parsed.resumeID != "" {
		return options{}, errors.New("--session 和 --resume 不能同时使用")
	}
	return parsed, nil
}

// run 装配依赖并进入交互循环，返回导致进程退出的错误。
func run(arguments []string) error {
	//先解析命令行参数
	parsed, err := parseOptions(arguments)
	if err != nil {
		return err
	}

	input := bufio.NewReader(os.Stdin)
	//加载配置，比如说API Key
	active, err := resolveSettings(input, os.Stdout)
	if err != nil {
		return err
	}
	///会话目录
	directory, err := dataDir()
	if err != nil {
		return err
	}

	sessions, err := store.New(directory)
	if err != nil {
		return err
	}
	// 释放会话锁并关闭数据库。放在这里而不是依赖进程退出，是为了让"会话已经关闭"
	// 这件事在正常退出路径上是确定的。
	defer sessions.Close()

	//下一阶段直接移除该历史提醒
	reportLegacySessions(os.Stdout)
	//加会话锁的点，在该方法中，要么打开历史对话加锁，要么创建新对话加锁
	session, resumed, err := openSession(parsed, sessions, input, os.Stdout)
	if errors.Is(err, errNoSelection) {
		// 用户主动放弃选择不是错误：安静退出，不打印错误也不给非零退出码。
		return nil
	}
	if err != nil {
		return err
	}

	// 命令在会话自己的工作目录下执行，而不是 goseek 的启动目录：历史里全是关于
	// 那个目录的事实，换地方执行会让模型收到一连串无法解释的"文件不存在"。
	//
	// conversation_history 绑定到这个会话对象（指针，因此看到的永远是最新历史），
	// 让模型在上下文被压缩之后还能回查原文。
	tools, err := tool.NewRegistry(
		tool.NewBash(session.Workspace),
		tool.NewHistory(session),
	)
	if err != nil {
		return fmt.Errorf("装配工具失败: %w", err)
	}

	client := model.New(model.Config{
		BaseURL: active.BaseURL,
		APIKey:  active.APIKey,
		Model:   active.Model,
	})
	// 同一个客户端既做决策也做摘要：它们打的是同一个供应商，只是请求形态不同。
	assistant := agent.New(client, tools, sessions, newStepPrinter(os.Stdout), client, active.ContextWindow)
	// 供应商拒绝超窗请求时会在错误里说出真实窗口，注入解析器让那句话变成
	// 一条可以照做的建议。
	assistant.SetWindowErrorParser(model.ContextWindowFromError)
	// 估算的契约是"上界"。一旦被实测值击穿就必须让人知道——它意味着压缩的
	// 终止性保证有洞，而不只是一个数字不准。
	assistant.SetEstimateBreachReporter(func(breach domain.EstimateBreach) {
		fmt.Fprintf(os.Stderr,
			"\n⚠ token 估算被击穿：估 %d，实际 %d（%.2fx）。已把安全系数从 %.2f 调高。\n"+
				"  这说明这段对话的内容构成和校准时差得远，压缩可能在错误的时机触发。\n\n",
			breach.Estimated, breach.Actual, breach.Ratio(), breach.UsedFactor)
	})

	// 同样要大声说：终端里没有事件流，只有这一处能告诉用户"压缩已经在动用降级手段了"。
	assistant.SetCompactionDiagnosisReporter(func(_ domain.SessionID, reason string) {
		fmt.Fprintf(os.Stderr,
			"\n⚠ 压缩走到了收尾诊断：%s\n"+
				"  这不是正常工作状态。要么这个会话该收尾了，要么上下文窗口配小了。\n\n",
			reason)
	})

	printBanner(os.Stdout, active, session, resumed)
	//如果上次运行突然中断，会提醒有一条命令可能修改了用户文件，而程序无法确定。
	if err := reportInterruptedCalls(assistant, session, os.Stdout); err != nil {
		return err
	}
	return interact(assistant, session, input)
}

// resolveSettings 取得本次运行的配置，必要时向用户索取 API key。
func resolveSettings(input *bufio.Reader, output io.Writer) (settings, error) {
	active, err := loadSettings()
	if err != nil {
		return settings{}, err
	}
	if active.APIKey != "" {
		return active, nil
	}

	key, err := promptForAPIKey(input, output)
	if err != nil {
		return settings{}, err
	}
	active.APIKey = key
	return active, nil
}

// openSession 按命令行参数创建或恢复一个会话，并说明它是哪一种。
func openSession(parsed options, sessions *store.Store, input *bufio.Reader, output io.Writer) (*domain.Session, bool, error) {
	switch {
	case parsed.resumeID != "":
		//resumeID非空则根据该ID还原对话。
		session, err := sessions.Load(domain.SessionID(parsed.resumeID))
		if err != nil {
			return nil, false, err
		}
		return session, true, nil

	case parsed.listSessions:
		//如果是要求列出最近会话的形式
		// 选择列表默认只列未归档的——归档的意思就是"从眼前拿走"。
		summaries, err := sessions.List(false)
		if err != nil {
			return nil, false, err
		}
		//根据输入的id，对应查处的列表会话id
		id, err := chooseSession(summaries, input, output, time.Now())
		if err != nil {
			return nil, false, err
		}
		session, err := sessions.Load(id)
		if err != nil {
			return nil, false, err
		}
		return session, true, nil

	default:
		//默认就是创建新对话，直接讲当前的窗口打开目录作为工作目录
		workspace, err := os.Getwd()
		if err != nil {
			return nil, false, fmt.Errorf("获取当前目录失败: %w", err)
		}
		//创建一个以当前目录为工作目录的会话
		session, err := sessions.Create(workspace, "")
		if err != nil {
			return nil, false, err
		}
		return session, false, nil
	}
}

// printBanner 打印启动信息。
//
// 工作目录一定要显示：恢复会话时它可能不是用户当前所在的目录，不说清楚会让人
// 以为命令跑在别处。完整会话 ID 也要给出，那是 --resume 需要的。
func printBanner(output io.Writer, active settings, session *domain.Session, resumed bool) {
	state := "新建"
	if resumed {
		state = fmt.Sprintf("恢复，%d 条历史", len(session.Messages()))
	}
	// 把窗口和它的来源一起打出来。这个数曾经悄悄错了 8 倍（128000 对
	// 1048576），而界面上只显示"128K"，看不出它是猜的——所以来源要写在旁边。
	fmt.Fprintf(output, "GoSeek (model: %s, 上下文窗口: %s)\n",
		active.Model, describeWindow(active))
	fmt.Fprintf(output, "会话 %s（%s） 工作目录 %s\n", session.ID.Short(), state, session.Workspace)
	fmt.Fprintf(output, "继续这个会话: goseek --resume %s\n", session.ID)
	fmt.Fprintf(output, "输入 %s 退出。\n\n", exitCommand)
}

// describeWindow 把窗口大小连同它的来源渲染成一行。
//
// 兜底来源要额外提示：那意味着模型名不在表里，这个数多半是错的，而错了的后果
// 不是"显示不准"——压缩会在完全不必要的时候反复触发。
func describeWindow(active settings) string {
	if active.ContextWindow <= 0 {
		return "未知（不会自动压缩）"
	}
	line := fmt.Sprintf("%d tokens（%s）", active.ContextWindow, active.ContextWindowSource)
	if active.ContextWindowSource == windowFromFallback {
		line += fmt.Sprintf("\n  ⚠ 模型 %s 不在已知窗口表里，用的是保守兜底值。"+
			"若不对，用 GOSEEK_CONTEXT_WINDOW 指定真实窗口。", active.Model)
	}
	return line
}

// reportLegacySessions 提醒用户 M2 留下的会话文件已经不再被读取。
//
// 不做自动导入：导入器跑完一次就是死代码，而这些文件是可读的 JSON，需要的话
// 用户自己就能看。等到不再需要这条提醒时，连同 legacySessionsDir 一起删掉。review意见：M3.2直接删
func reportLegacySessions(output io.Writer) {
	count, directory := countLegacySessionFiles()
	if count == 0 {
		return
	}
	fmt.Fprintf(output, "注意：%s 下还有 %d 个旧版会话文件。\n", directory, count)
	fmt.Fprintf(output, "会话现在保存在数据库里，这些文件不再被读取，可以自行查看或删除。\n\n")
}

// reportInterruptedCalls 修复上次中断留下的悬空工具调用，并如实告诉用户。
//
// 用户必须知道这件事：有一条命令可能已经改动了他的文件，而程序无法确认。
func reportInterruptedCalls(assistant *agent.Agent, session *domain.Session, output io.Writer) error {
	repaired, err := assistant.Restore(session)
	if err != nil {
		return err
	}
	if len(repaired) == 0 {
		return nil
	}

	fmt.Fprintf(output, "上次运行被中断，%d 个工具调用没有留下结果：\n", len(repaired))
	for _, result := range repaired {
		fmt.Fprintf(output, "  · %s\n", result.Content)
	}
	fmt.Fprintf(output, "这些情况已作为观察写入会话，模型会看到它们。\n\n")
	return nil
}

// interact 反复读取一行输入并交给 Agent 处理，直到用户退出或输入结束。
//
// 单次交互失败只报告错误并继续，因为会话本身仍然有效；只有输入流本身出问题才结束循环。
// 一轮中的执行步骤由 stepPrinter 实时打印，这里只负责最终回复。
func interact(assistant *agent.Agent, session *domain.Session, reader *bufio.Reader) error {
	for {
		fmt.Print("> ")

		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("读取输入失败: %w", readErr)
		}
		// EOF 之前的最后一行可能没有换行符，因此先处理内容再决定是否退出。
		input := strings.TrimSpace(line)
		endOfInput := errors.Is(readErr, io.EOF)

		if input == exitCommand {
			return nil
		}
		if input != "" {
			// 忽略返回的回复：它已经随事件流式打印出来了，再打一遍就是同一句话
			// 出现两遍。Handle 仍然返回它，因为 M3.2 的 HTTP 入口需要这个值。
			if _, err := assistant.Handle(context.Background(), session, input); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n\n", err)
			}
		}
		if endOfInput {
			fmt.Println()
			return nil
		}
	}
}
