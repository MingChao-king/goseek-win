package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client 是一个已连接的 MCP server。
type Client struct {
	name   string
	client *mcp.ClientSession
	cmd    *exec.Cmd
}

// Config 是一个 MCP server 的启动配置。
type Config struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Connect 启动一个 stdio MCP server 并完成初始化握手。
func Connect(ctx context.Context, config Config) (*Client, error) {
	cmd := exec.Command(config.Command, config.Args...)
	if len(config.Env) > 0 {
		env := os.Environ()
		for key, value := range config.Env {
			env = append(env, key+"="+value)
		}
		cmd.Env = env
	}

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "goseek", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 MCP server %q 失败: %w", config.Name, err)
	}
	return &Client{name: config.Name, client: session, cmd: cmd}, nil
}

// ServerTool 描述 MCP server 暴露的一个工具。
type ServerTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ListTools 获取 server 暴露的工具列表。
func (c *Client) ListTools(ctx context.Context) ([]ServerTool, error) {
	response, err := c.client.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("获取工具列表失败: %w", err)
	}
	var tools []ServerTool
	for _, tool := range response.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		tools = append(tools, ServerTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return tools, nil
}

// ToolCallOutcome 是一次 MCP 工具调用的结构化观察。
//
// MCP 协议允许多 content；文本拼成一段，图片单独保留（base64 解码后的字节），
// 交给 adapter 落盘并构造成带图 ToolResult。IsError 按 MCP 语义透传。
type ToolCallOutcome struct {
	Text    string
	Images  []ToolImage
	IsError bool
}

// ToolImage 是一次 MCP 调用返回的一张图片。
type ToolImage struct {
	Data      []byte
	MediaType string
}

// CallTool 调用 server 上的一个工具，返回结构化观察。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolCallOutcome, error) {
	params := &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(arguments)}
	result, err := c.client.CallTool(ctx, params)
	if err != nil {
		return ToolCallOutcome{}, err
	}
	outcome := ToolCallOutcome{IsError: result.IsError}
	var content strings.Builder
	for _, item := range result.Content {
		if textContent, ok := item.(*mcp.TextContent); ok {
			content.WriteString(textContent.Text)
		}
		if imageContent, ok := item.(*mcp.ImageContent); ok {
			data, err := base64.StdEncoding.DecodeString(string(imageContent.Data))
			if err != nil {
				return ToolCallOutcome{}, fmt.Errorf("解码 MCP 图片内容失败: %w", err)
			}
			mediaType := imageContent.MIMEType
			if mediaType == "" {
				mediaType = "image/png"
			}
			outcome.Images = append(outcome.Images, ToolImage{Data: data, MediaType: mediaType})
		}
	}
	outcome.Text = content.String()
	return outcome, nil
}

// Close 关闭与 server 的连接。
func (c *Client) Close() error {
	return c.client.Close()
}
