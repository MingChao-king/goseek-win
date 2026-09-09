package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"goseek/internal/domain"
)

// ReadFileTool 读取一个文件的内容。
type ReadFileTool struct {
	workspace string
}

func NewReadFile(workspace string) *ReadFileTool {
	return &ReadFileTool{workspace: workspace}
}

type readFileArgs struct {
	Path string `json:"path"`
}

func (tool *ReadFileTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        "read_file",
		Description: "读取指定路径的文件内容，返回完整文本。路径相对于当前工作目录，必须是 workspace 内的文件。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "要读取的文件路径（相对于 workspace 或绝对路径）"
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`),
	}
}

func (tool *ReadFileTool) DescribeCall(call domain.ToolCall) string {
	args, err := parseArgs[readFileArgs](call.Arguments)
	if err != nil {
		return "读取文件"
	}
	return fmt.Sprintf("读取 %s", args.Path)
}

func (tool *ReadFileTool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	args, err := parseArgs[readFileArgs](call.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	resolved, err := resolvePath(tool.workspace, args.Path)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return errorResult(fmt.Sprintf("读取 %s 失败: %v", args.Path, err)), nil
	}
	return domain.ToolResult{Status: domain.ToolSuccess, Content: string(data)}, nil
}

// WriteFileTool 写一个文件（全量覆盖）。
type WriteFileTool struct {
	workspace string
}

func NewWriteFile(workspace string) *WriteFileTool {
	return &WriteFileTool{workspace: workspace}
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (tool *WriteFileTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        "write_file",
		Description: "写入指定路径的文件（全量覆盖），自动创建父目录。路径必须在 workspace 内。写入后如需确认可再用 read_file 或 bash 查看。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "要写入的文件路径"
    },
    "content": {
      "type": "string",
      "description": "文件完整内容"
    }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
	}
}

func (tool *WriteFileTool) DescribeCall(call domain.ToolCall) string {
	args, err := parseArgs[writeFileArgs](call.Arguments)
	if err != nil {
		return "写入文件"
	}
	return fmt.Sprintf("写入 %s", args.Path)
}

func (tool *WriteFileTool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	args, err := parseArgs[writeFileArgs](call.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	resolved, err := resolvePath(tool.workspace, args.Path)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errorResult(fmt.Sprintf("创建目录 %s 失败: %v", dir, err)), nil
	}
	if err := os.WriteFile(resolved, []byte(args.Content), 0o644); err != nil {
		return errorResult(fmt.Sprintf("写入 %s 失败: %v", args.Path, err)), nil
	}
	return domain.ToolResult{
		Status:  domain.ToolSuccess,
		Content: fmt.Sprintf("已写入 %s (%d bytes)", args.Path, len(args.Content)),
	}, nil
}

// SearchTool 在 workspace 内做文本搜索（用 rg 子进程）。
type SearchTool struct {
	workspace string
}

func NewSearch(workspace string) *SearchTool {
	return &SearchTool{workspace: workspace}
}

type searchArgs struct {
	Pattern string `json:"pattern"`
	Glob    string `json:"glob,omitempty"`
}

func (tool *SearchTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        "search",
		Description: "在 workspace 内做文本搜索。用正则表达式匹配，返回文件名、行号和匹配行。适用于查找代码、配置和文本中的特定内容。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "要搜索的正则表达式"
    },
    "glob": {
      "type": "string",
      "description": "可选的文件名过滤模式，如 *.go"
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`),
	}
}

func (tool *SearchTool) DescribeCall(call domain.ToolCall) string {
	args, err := parseArgs[searchArgs](call.Arguments)
	if err != nil {
		return "搜索文件"
	}
	return fmt.Sprintf("搜索 %q", args.Pattern)
}

func (tool *SearchTool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	args, err := parseArgs[searchArgs](call.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	// Build rg command.
	argsList := []string{"--color=never", "--max-count=20"}
	if args.Glob != "" {
		argsList = append(argsList, "--glob", args.Glob)
	}
	argsList = append(argsList, args.Pattern, tool.workspace)
	cmd := exec.Command("rg", argsList...)
	cmd.Dir = tool.workspace
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// rg exit 1 = no matches, not an error.
			return domain.ToolResult{Status: domain.ToolSuccess, Content: "没有匹配结果"}, nil
		}
		return errorResult(fmt.Sprintf("搜索失败: %v\n%s", err, out.String())), nil
	}
	return domain.ToolResult{Status: domain.ToolSuccess, Content: out.String()}, nil
}

// resolvePath 把一个用户/模型给的路径限制到 workspace 内。
func resolvePath(workspace, path string) (string, error) {
	if filepath.IsAbs(path) {
		// Allow absolute paths inside workspace only.
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(abs, workspace+string(filepath.Separator)) && abs != workspace {
			return "", fmt.Errorf("路径 %s 超出工作目录 %s 的范围", path, workspace)
		}
		return abs, nil
	}
	return filepath.Join(workspace, path), nil
}

// errorResult 构造一条 error 状态的 ToolResult。
func errorResult(message string) domain.ToolResult {
	return domain.ToolResult{Status: domain.ToolError, Content: message}
}

// parseArgs 把原始 JSON 参数解析到给定的结构体。
func parseArgs[T any](raw json.RawMessage) (*T, error) {
	var args T
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("参数解析失败: %w", err)
	}
	return &args, nil
}
