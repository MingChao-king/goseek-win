// Package tool 提供模型可以调用的工具，以及按名称查找并执行它们的注册表。
//
// 本包的核心约定是：一次工具调用无论走到哪一步，都必须产生一条模型可读的观察。
// 未知工具、参数非法、命令失败都不是程序错误，而是模型据以决定下一步的事实。
package tool

import (
	"context"
	"fmt"
	"strings"

	"goseek/internal/domain"
)

// Tool 是一个模型可以调用的能力。
type Tool interface {
	// Spec 返回发送给模型的工具定义。
	Spec() domain.ToolSpec

	// DescribeCall 返回这次调用的一行人类可读标题。
	//
	// 由工具而不是调用方生成：只有工具认识自己的参数结构。参数不可解析时
	// 应当回退到一个中性文案，而不是报错——描述失败不该影响执行。
	DescribeCall(call domain.ToolCall) string

	// Run 执行调用并返回结构化观察。
	//
	// 非零退出、超时、进程无法启动都是预期的执行事实，要构造成 ToolResult 并
	// 返回 nil error。只有无法可靠构造执行事实的内部错误才返回非空 error，
	// 由 Registry 兜底成一条 error 观察。
	//
	// 返回值中的 ToolCallID 和 Name 由 Registry 填写，工具不必设置：配对是
	// 注册表的责任，工具只负责观察内容。
	//
	// onOutput 接收执行过程中产生的输出片段，让长命令不必等到结束才有动静。
	// Registry 保证它不为 nil。它接到的是完整的输出流，而最终落进观察的仍然是
	// 按上限截断后的内容——展示的实时性和存储的有界性是两件事。
	Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error)
}

// Registry 按名称保存工具，并保证每个调用都得到配对的观察。
type Registry struct {
	// tools 按名称索引已注册的工具。
	tools map[string]Tool
	// names 保持注册顺序，让 Specs 的输出稳定，便于测试和请求复现。
	names []string
}

// NewRegistry 注册一组工具，名称重复时返回错误。
//
// 重名会让"模型调用哪个工具"变得不确定，因此在装配期就拒绝，而不是运行时才发现。
func NewRegistry(tools ...Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, tool := range tools {
		name := tool.Spec().Name
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("工具名称不能为空")
		}
		if _, exists := registry.tools[name]; exists {
			return nil, fmt.Errorf("工具名称 %q 重复注册", name)
		}
		registry.tools[name] = tool
		registry.names = append(registry.names, name)
	}
	return registry, nil
}

// Specs 按注册顺序返回全部工具定义，供组装模型请求使用。
func (registry *Registry) Specs() []domain.ToolSpec {
	specs := make([]domain.ToolSpec, 0, len(registry.names))
	for _, name := range registry.names {
		specs = append(specs, registry.tools[name].Spec())
	}
	return specs
}

// Lookup 按名称返回一个工具，未注册返回 nil。
func (registry *Registry) Lookup(name string) Tool {
	return registry.tools[name]
}

// Describe 返回这次调用的一行人类可读标题，用于展示执行步骤。
//
// 未知工具也要有描述：这一步同样会展示给用户，只是随后会得到一条 error 观察。
func (registry *Registry) Describe(call domain.ToolCall) string {
	tool, known := registry.tools[call.Name]
	if !known {
		return fmt.Sprintf("调用未知工具 %s", call.Name)
	}
	return tool.DescribeCall(call)
}

// Execute 执行一次调用，并返回与它配对的唯一观察。
//
// onOutput 接收执行过程中的输出片段，可以为 nil。
//
// 该方法不返回 error。未知工具、参数非法和执行失败全部转换成 status=error 的
// 观察交回模型——让它返回 error 会诱导调用方写出"工具失败即本轮失败"的分支，
// 而这些情况恰恰是模型需要读到并自行纠正的事实。
func (registry *Registry) Execute(
	ctx context.Context,
	call domain.ToolCall,
	onOutput domain.OutputFunc,
) domain.ToolResult {
	// 统一在这里兜住 nil，工具实现里就不必每个都判一次。
	if onOutput == nil {
		onOutput = func(string) {}
	}

	tool, known := registry.tools[call.Name]
	if !known {
		return registry.pair(call, domain.ToolResult{
			Status: domain.ToolError,
			Content: fmt.Sprintf("不存在名为 %q 的工具，本次调用没有执行。当前可用的工具：%s。",
				call.Name, strings.Join(registry.names, "、")),
		})
	}

	result, err := tool.Run(ctx, call, onOutput)
	if err != nil {
		return registry.pair(call, domain.ToolResult{
			Status:  domain.ToolError,
			Content: fmt.Sprintf("工具 %q 出现内部错误：%v。这次调用是否产生了副作用无法确定。", call.Name, err),
		})
	}
	return registry.pair(call, result)
}

// pair 补齐观察与调用之间的配对字段。
//
// 由注册表统一覆写而不是信任工具填写：即使某个工具实现有 bug，也不可能出现
// ID 对不上或名称错位的观察，供应商协议因此不会被破坏。
func (registry *Registry) pair(call domain.ToolCall, result domain.ToolResult) domain.ToolResult {
	result.ToolCallID = call.ID
	result.Name = call.Name
	return result
}
