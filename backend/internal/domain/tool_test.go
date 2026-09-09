package domain_test

import (
	"encoding/json"
	"testing"

	"goseek/internal/domain"
)

// tool 消息保存的是整个 ToolResult 的 JSON。这条测试锁住它的意义：
// "成功但没有输出"和"失败"必须能被模型区分开。
func TestEncodeContentDistinguishesEmptySuccessFromError(t *testing.T) {
	zero := 0
	success := domain.ToolResult{
		ToolCallID: "call-1", Name: "bash", Status: domain.ToolSuccess, ExitCode: &zero,
	}
	failure := domain.ToolResult{
		ToolCallID: "call-1", Name: "bash", Status: domain.ToolError, Content: "命令没有执行",
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(success.EncodeContent()), &decoded); err != nil {
		t.Fatalf("成功结果不是合法 JSON: %v", err)
	}
	if decoded["status"] != "success" {
		t.Errorf("status = %v; want success", decoded["status"])
	}
	if decoded["content"] != "" {
		t.Errorf("content = %v; want 空字符串", decoded["content"])
	}
	if decoded["exit_code"] != float64(0) {
		t.Errorf("exit_code = %v; want 0", decoded["exit_code"])
	}
	if success.EncodeContent() == failure.EncodeContent() {
		t.Error("成功但无输出与失败被编码成了同一个内容")
	}
}

// 没有退出码时该字段整个消失，模型不会看到一个含义可疑的 0。
func TestEncodeContentOmitsExitCodeWhenCommandNeverRan(t *testing.T) {
	result := domain.ToolResult{
		ToolCallID: "call-1", Name: "python", Status: domain.ToolError, Content: "不存在这个工具",
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.EncodeContent()), &decoded); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if _, present := decoded["exit_code"]; present {
		t.Errorf("未执行的调用带了 exit_code: %v", decoded["exit_code"])
	}
}

// 观察里**不再有 truncated 这个字段**。
//
// 后端不截断任何东西，所以"你看到的不是全部"这种状态不存在了。留一个恒为 false
// 的字段比删掉更糟：它会让模型在提示词里继续被告知一件不会发生的事，也会让读代码
// 的人以为截断还可能发生。
func TestEncodeContentHasNoTruncationFlag(t *testing.T) {
	result := domain.ToolResult{
		ToolCallID: "call-1", Name: "bash", Status: domain.ToolSuccess,
		Content: "很长的输出",
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.EncodeContent()), &decoded); err != nil {
		t.Fatalf("结果不是合法 JSON: %v", err)
	}
	if _, present := decoded["truncated"]; present {
		t.Errorf("观察里仍然带着 truncated 字段: %v", decoded)
	}
}
