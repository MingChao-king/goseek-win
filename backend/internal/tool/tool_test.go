package tool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goseek/internal/domain"
)

// fakeTool 是一个可以精确控制返回值的工具，用于验证注册表的行为。
type fakeTool struct {
	name   string
	result domain.ToolResult
	err    error
	calls  int
}

// Spec 返回只含名称的最小定义。
func (fake *fakeTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{Name: fake.name, Description: fake.name + " 的说明"}
}

// DescribeCall 返回一个可辨认的固定标题。
func (fake *fakeTool) DescribeCall(domain.ToolCall) string {
	return "执行 " + fake.name
}

// Run 返回预设的结果，并记录被调用次数。
func (fake *fakeTool) Run(context.Context, domain.ToolCall, domain.OutputFunc) (domain.ToolResult, error) {
	fake.calls++
	return fake.result, fake.err
}

func TestNewRegistryRejectsDuplicateAndEmptyNames(t *testing.T) {
	if _, err := NewRegistry(&fakeTool{name: "bash"}, &fakeTool{name: "bash"}); err == nil {
		t.Error("重名注册没有报错")
	}
	if _, err := NewRegistry(&fakeTool{name: "  "}); err == nil {
		t.Error("空名称注册没有报错")
	}
}

func TestSpecsFollowRegistrationOrder(t *testing.T) {
	registry, err := NewRegistry(&fakeTool{name: "first"}, &fakeTool{name: "second"})
	if err != nil {
		t.Fatalf("NewRegistry 返回错误: %v", err)
	}

	specs := registry.Specs()
	if len(specs) != 2 || specs[0].Name != "first" || specs[1].Name != "second" {
		t.Errorf("Specs = %+v; want 按注册顺序的 first、second", specs)
	}
}

// 未知工具是模型可读的观察，不是程序错误：它必须得到配对的 error 结果。
func TestExecuteTurnsUnknownToolIntoPairedErrorResult(t *testing.T) {
	registry, err := NewRegistry(&fakeTool{name: "bash"})
	if err != nil {
		t.Fatalf("NewRegistry 返回错误: %v", err)
	}

	call := domain.ToolCall{ID: "call-1", Name: "python"}
	result := registry.Execute(context.Background(), call, nil)

	if result.Status != domain.ToolError {
		t.Errorf("status = %q; want %q", result.Status, domain.ToolError)
	}
	if result.ToolCallID != "call-1" || result.Name != "python" {
		t.Errorf("配对字段 = %q/%q; want call-1/python", result.ToolCallID, result.Name)
	}
	if result.ExitCode != nil {
		t.Errorf("未执行的调用带了退出码 %d", *result.ExitCode)
	}
	if !strings.Contains(result.Content, "bash") {
		t.Errorf("错误说明里没有列出可用工具: %q", result.Content)
	}
}

// 工具返回内部错误时，注册表兜底成一条观察，而不是把错误抛给调用方。
func TestExecuteTurnsToolInternalErrorIntoErrorResult(t *testing.T) {
	registry, err := NewRegistry(&fakeTool{name: "bash", err: errors.New("管道断了")})
	if err != nil {
		t.Fatalf("NewRegistry 返回错误: %v", err)
	}

	result := registry.Execute(context.Background(), domain.ToolCall{ID: "call-2", Name: "bash"}, nil)

	if result.Status != domain.ToolError {
		t.Errorf("status = %q; want %q", result.Status, domain.ToolError)
	}
	if result.ToolCallID != "call-2" {
		t.Errorf("tool_call_id = %q; want call-2", result.ToolCallID)
	}
	if !strings.Contains(result.Content, "管道断了") {
		t.Errorf("观察里没有保留原始错误: %q", result.Content)
	}
}

// 配对字段由注册表覆写：工具即使填错，协议也不会被破坏。
func TestExecuteOverwritesPairingFieldsFromTheCall(t *testing.T) {
	misbehaving := &fakeTool{
		name: "bash",
		result: domain.ToolResult{
			ToolCallID: "错误的-id",
			Name:       "错误的名字",
			Status:     domain.ToolSuccess,
			Content:    "输出",
		},
	}
	registry, err := NewRegistry(misbehaving)
	if err != nil {
		t.Fatalf("NewRegistry 返回错误: %v", err)
	}

	result := registry.Execute(context.Background(), domain.ToolCall{ID: "call-3", Name: "bash"}, nil)

	if result.ToolCallID != "call-3" || result.Name != "bash" {
		t.Errorf("配对字段 = %q/%q; want call-3/bash", result.ToolCallID, result.Name)
	}
	if result.Content != "输出" {
		t.Errorf("观察正文被改动: %q", result.Content)
	}
}

func TestDescribeFallsBackForUnknownTool(t *testing.T) {
	registry, err := NewRegistry(&fakeTool{name: "bash"})
	if err != nil {
		t.Fatalf("NewRegistry 返回错误: %v", err)
	}

	if got := registry.Describe(domain.ToolCall{Name: "bash"}); got != "执行 bash" {
		t.Errorf("已知工具的标题 = %q; want %q", got, "执行 bash")
	}
	if got := registry.Describe(domain.ToolCall{Name: "python"}); !strings.Contains(got, "python") {
		t.Errorf("未知工具的标题 = %q; want 包含工具名", got)
	}
}
