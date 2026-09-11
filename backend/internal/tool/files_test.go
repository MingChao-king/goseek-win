package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goseek/internal/domain"
)

func TestReadFileAcceptsAbsolutePathOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(target, []byte("outside content"), 0o600); err != nil {
		t.Fatalf("准备外部文件失败: %v", err)
	}

	call := domain.ToolCall{
		ID: "call-read", Name: "read_file",
		Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, target)),
	}
	result, err := (&ReadFileTool{workspace: workspace}).Run(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess || result.Content != "outside content" {
		t.Fatalf("读取 workspace 外文件失败: %+v", result)
	}
}

func TestWriteFileAcceptsAbsolutePathOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "nested", "out.txt")

	call := domain.ToolCall{
		ID: "call-write", Name: "write_file",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"path":%q,"content":"written outside"}`, target)),
	}
	result, err := (&WriteFileTool{workspace: workspace}).Run(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess {
		t.Fatalf("写入 workspace 外文件失败: %+v", result)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取写入结果失败: %v", err)
	}
	if string(data) != "written outside" {
		t.Errorf("写入内容 = %q; want %q", data, "written outside")
	}
}

func TestRelativePathsResolveInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	relative := filepath.Join("nested", "rel.txt")

	writeCall := domain.ToolCall{
		ID: "call-write", Name: "write_file",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"path":%q,"content":"inside"}`, relative)),
	}
	result, err := (&WriteFileTool{workspace: workspace}).Run(context.Background(), writeCall, nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess {
		t.Fatalf("写入相对路径失败: %+v", result)
	}

	readCall := domain.ToolCall{
		ID: "call-read", Name: "read_file",
		Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, relative)),
	}
	result, err = (&ReadFileTool{workspace: workspace}).Run(context.Background(), readCall, nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess || result.Content != "inside" {
		t.Fatalf("读取相对路径失败: %+v", result)
	}
}

func TestSearchAcceptsAbsolutePathOutsideWorkspace(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg 不可用")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(target, []byte("needle here"), 0o600); err != nil {
		t.Fatalf("准备外部文件失败: %v", err)
	}

	call := domain.ToolCall{
		ID: "call-search", Name: "search",
		Arguments: json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q}`, outside)),
	}
	result, err := (&SearchTool{workspace: workspace}).Run(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess {
		t.Fatalf("搜索 workspace 外文件失败: %+v", result)
	}
	if !strings.Contains(result.Content, target) {
		t.Errorf("搜索结果里没有外部文件路径 %q:\n%s", target, result.Content)
	}
}

func TestSearchDefaultsToWorkspace(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg 不可用")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(workspace, "in.txt")
	outsideTarget := filepath.Join(outside, "out.txt")
	if err := os.WriteFile(inside, []byte("needle"), 0o600); err != nil {
		t.Fatalf("准备工作区文件失败: %v", err)
	}
	if err := os.WriteFile(outsideTarget, []byte("needle"), 0o600); err != nil {
		t.Fatalf("准备外部文件失败: %v", err)
	}

	call := domain.ToolCall{
		ID: "call-search", Name: "search",
		Arguments: json.RawMessage(`{"pattern":"needle"}`),
	}
	result, err := (&SearchTool{workspace: workspace}).Run(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if result.Status != domain.ToolSuccess {
		t.Fatalf("默认搜索失败: %+v", result)
	}
	if !strings.Contains(result.Content, inside) {
		t.Errorf("搜索结果里没有工作区文件 %q:\n%s", inside, result.Content)
	}
	if strings.Contains(result.Content, outsideTarget) {
		t.Errorf("默认搜索越过了工作区边界:\n%s", result.Content)
	}
}
