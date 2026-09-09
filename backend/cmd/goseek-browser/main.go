// goseek-browser 是隔离浏览器插件的 CLI 工具。
//
// 它不启动浏览器进程，而是调用 GoSeek HTTP API（http://127.0.0.1:8765），
// 由服务端持有常驻的隔离浏览器实例——这样 agent 和侧栏看到的是同一个浏览器，
// 页面不会因为 CLI 进程退出而消失。多页面（tab）按调用顺序在同一个浏览器里打开。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const baseURL = "http://127.0.0.1:8765"

// sessionID 从环境变量 GOSEEK_SESSION_ID 取（由 BashTool 注入）；
// 没有该变量时回退到 "default"。
func sessionID() string {
	if v := os.Getenv("GOSEEK_SESSION_ID"); v != "" {
		return v
	}
	return "default"
}

func browserAPIPath(path string) string {
	return path + "?sessionId=" + url.QueryEscape(sessionID())
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "screenshot":
		cmdScreenshot(os.Args[2:])
	case "read-page":
		cmdReadPage()
	case "click":
		cmdClick(os.Args[2:])
	case "type":
		cmdType(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法: goseek-browser <command> [args]

命令:
  screenshot <output_path> [url] [wait_seconds]
      截取当前页面（或先打开 URL 再截屏），输出图片标记。
  read-page
      列出当前页面的可交互元素（编号、标签、文本、位置）。
  click <编号>
      按编号点击元素（自动滚到可见位置）。
  type <编号> <文本>
      向指定输入框填入文本。`)
}

func apiRequest(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GoSeek 服务不可用（goseek serve 是否在跑？）: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var errBody struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &errBody) == nil && errBody.Error.Message != "" {
			return nil, fmt.Errorf("%s", errBody.Error.Message)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func cmdScreenshot(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "错误: 缺少 output_path")
		os.Exit(1)
	}
	outputPath := args[0]
	url := ""
	waitSeconds := 3
	if len(args) > 1 {
		url = args[1]
	}
	if len(args) > 2 {
		if n, err := strconv.Atoi(args[2]); err == nil && n > 0 {
			waitSeconds = n
		}
	}

	// 有 URL 时先 navigate（在隔离浏览器里开新页面）
	if url != "" {
		_, err := apiRequest("POST", browserAPIPath("/api/v1/browser/navigate"), map[string]any{
			"url": url, "waitSeconds": waitSeconds,
		})
		if err != nil {
			fatal(err)
		}
	}

	// 截图
	data, err := apiRequest("GET", browserAPIPath("/api/v1/browser/screenshot"), nil)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(outputPath, data, 0o600); err != nil {
		fatal(fmt.Errorf("写入截图失败: %w", err))
	}
	fmt.Printf("[goseek-image:%s]\n", outputPath)
}

func cmdReadPage() {
	data, err := apiRequest("GET", browserAPIPath("/api/v1/browser/read-page"), nil)
	if err != nil {
		fatal(err)
	}
	var result struct {
		Page     string `json:"page"`
		Elements []struct {
			Index int    `json:"index"`
			Tag   string `json:"tag"`
			Text  string `json:"text"`
			X     int    `json:"x"`
			Y     int    `json:"y"`
			W     int    `json:"w"`
			H     int    `json:"h"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		fatal(err)
	}
	fmt.Printf("页面: %s\n", result.Page)

	// 同时输出所有打开的页面列表（agent 能感知全部 tab）。
	tabsData, tabsErr := apiRequest("GET", browserAPIPath("/api/v1/browser/tabs"), nil)
	if tabsErr == nil {
		var tabsResult struct {
			Tabs []struct {
				ID    string `json:"id"`
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"tabs"`
		}
		if json.Unmarshal(tabsData, &tabsResult) == nil && len(tabsResult.Tabs) > 1 {
			fmt.Println("\n当前打开的所有页面:")
			for i, t := range tabsResult.Tabs {
				fmt.Printf("  [%d] %s (%s)\n", i, t.Title, t.URL)
			}
		}
	}

	fmt.Println("\n可交互元素:")
	fmt.Println("编号 | 标签 | 文本 | 位置")
	for _, el := range result.Elements {
		fmt.Printf("%d | %s | %s | (%d,%d) %dx%d\n", el.Index, el.Tag, el.Text, el.X, el.Y, el.W, el.H)
	}
}

func cmdClick(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "错误: 缺少元素编号")
		os.Exit(1)
	}
	index, err := strconv.Atoi(args[0])
	if err != nil {
		fatal(fmt.Errorf("元素编号必须是整数: %w", err))
	}
	_, err = apiRequest("POST", browserAPIPath("/api/v1/browser/click-element"), map[string]int{"index": index})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("已点击元素 %d\n", index)
}

func cmdType(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "错误: 需要 <编号> <文本>")
		os.Exit(1)
	}
	index, err := strconv.Atoi(args[0])
	if err != nil {
		fatal(fmt.Errorf("元素编号必须是整数: %w", err))
	}
	_, err = apiRequest("POST", browserAPIPath("/api/v1/browser/type"), map[string]any{"index": index, "text": args[1]})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("已向元素 %d 填入文本\n", index)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	os.Exit(1)
}
