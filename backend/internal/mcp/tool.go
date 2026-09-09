package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"goseek/internal/domain"
)

// mcpToolArgs 是调用 MCP 工具时的通用参数结构。
type mcpToolArgs struct {
	// Tool 是 MCP server 侧的原始工具名（不带 mcp:server: 前缀）。
	Tool string `json:"tool"`
	// Arguments 是要传给 server 的参数 JSON。
	Arguments json.RawMessage `json:"arguments"`
}

// Tool 是 MCP server 工具在 GoSeek 注册表里的适配器。
type Tool struct {
	// serverName 是 MCP server 的名称，用作工具名前缀。
	serverName string
	serverTool ServerTool
	client     *Client
}

var _ interface {
	Spec() domain.ToolSpec
	DescribeCall(domain.ToolCall) string
	Run(context.Context, domain.ToolCall, domain.OutputFunc) (domain.ToolResult, error)
} = (*Tool)(nil)

// Spec 返回带 mcp:server: 前缀的工具定义。
func (adapter *Tool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        adapter.prefixedName(),
		Description: fmt.Sprintf("[MCP:%s] %s", adapter.serverName, adapter.serverTool.Description),
		Parameters:  adapter.serverTool.InputSchema,
	}
}

// DescribeCall 返回调用的一行标题。
func (adapter *Tool) DescribeCall(call domain.ToolCall) string {
	return fmt.Sprintf("MCP %s → %s", adapter.serverName, adapter.serverTool.Name)
}

// Run 把调用转发给 MCP server。
//
// 返回的图片先写进系统临时目录，由 Agent 的 registerToolImages 拷贝进会话图片
// 存储并登记；临时文件由 Agent 删除（见 mcpDeleteTempImage）。
func (adapter *Tool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	outcome, err := adapter.client.CallTool(ctx, adapter.serverTool.Name, call.Arguments)
	if err != nil {
		return domain.ToolResult{
			Status:  domain.ToolError,
			Content: fmt.Sprintf("MCP 工具调用失败: %v", err),
		}, nil
	}
	status := domain.ToolSuccess
	if outcome.IsError {
		status = domain.ToolError
	}
	result := domain.ToolResult{Status: status, Content: outcome.Text}
	for _, image := range outcome.Images {
		file, err := os.CreateTemp("", "goseek-mcp-*.png")
		if err != nil {
			result.Status = domain.ToolError
			result.Content += "\n[MCP] 保存图片失败: " + err.Error()
			continue
		}
		if _, err := file.Write(image.Data); err != nil {
			file.Close()
			os.Remove(file.Name())
			result.Status = domain.ToolError
			result.Content += "\n[MCP] 写入图片失败: " + err.Error()
			continue
		}
		file.Close()
		ext := ".png"
		if image.MediaType == "image/jpeg" {
			ext = ".jpg"
		} else if image.MediaType == "image/webp" {
			ext = ".webp"
		}
		finalPath := file.Name()[:len(file.Name())-len(".png")] + ext
		if err := os.Rename(file.Name(), finalPath); err != nil {
			os.Remove(file.Name())
			result.Status = domain.ToolError
			result.Content += "\n[MCP] 重命名图片失败: " + err.Error()
			continue
		}
		result.Images = append(result.Images, domain.MessageImage{
			FilePath:  finalPath,
			MediaType: image.MediaType,
		})
	}
	if len(result.Images) > 0 && result.Content == "" {
		result.Content = "[MCP] 返回了 " + fmt.Sprint(len(result.Images)) + " 张图片，已附在本条观察中。"
	}
	return result, nil
}

// prefixedName 返回带命名空间前缀的工具名。
func (adapter *Tool) prefixedName() string {
	return "mcp_" + adapter.serverName + "_" + adapter.serverTool.Name
}

// Manager 管理全部 MCP server 连接。
type Manager struct {
	clients []*Client
	tools   []*Tool
}

// NewManager 按配置列表连接所有 server 并拉取工具列表。
func NewManager(ctx context.Context, configs []Config) (*Manager, error) {
	manager := &Manager{}
	for _, config := range configs {
		client, err := Connect(ctx, config)
		if err != nil {
			continue
		}
		manager.clients = append(manager.clients, client)
		serverTools, err := client.ListTools(ctx)
		if err != nil {
			continue
		}
		for _, serverTool := range serverTools {
			manager.tools = append(manager.tools, &Tool{
				serverName: config.Name,
				serverTool: serverTool,
				client:     client,
			})
		}
	}
	return manager, nil
}

// Tools 返回所有 server 的适配器工具。
func (manager *Manager) Tools() []*Tool {
	return manager.tools
}

// Close 关闭全部 server 连接。
func (manager *Manager) Close() {
	for _, client := range manager.clients {
		client.Close()
	}
}
