package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"goseek/internal/domain"
)

// ListSkillsTool 返回所有可用 skill 的名称和描述。
type ListSkillsTool struct {
	// skillsDirs 按优先级排列的 skill 目录：第一个是用户目录，其后是各插件的。
	skillsDirs []string
}

func NewListSkills(skillsDirs ...string) *ListSkillsTool {
	return &ListSkillsTool{skillsDirs: skillsDirs}
}

func (tool *ListSkillsTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        "list_skills",
		Description: "列出当前可用的 skill（技能指令），返回每个 skill 的名称和描述。用 load_skill 读取某个 skill 的完整正文。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (tool *ListSkillsTool) DescribeCall(call domain.ToolCall) string {
	return "列出可用 skills"
}

type SkillSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (tool *ListSkillsTool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	var summaries []SkillSummary
	for _, skillsDir := range tool.skillsDirs {
		entries, err := os.ReadDir(skillsDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(skillsDir, entry.Name(), "SKILL.md"))
			if err != nil {
				continue
			}
			summary := SkillSummary{Name: entry.Name()}
			for _, line := range strings.SplitN(string(data), "\n", 4) {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "# ") {
					summary.Name = strings.TrimPrefix(line, "# ")
				} else if line != "" && !strings.HasPrefix(line, "#") {
					summary.Description = line
					break
				}
			}
			summaries = append(summaries, summary)
		}
	}
	if len(summaries) == 0 {
		return domain.ToolResult{Status: domain.ToolSuccess, Content: "当前没有安装任何 skill。"}, nil
	}
	encoded, err := json.Marshal(summaries)
	if err != nil {
		return errorResult(fmt.Sprintf("编码 skill 列表失败: %v", err)), nil
	}
	return domain.ToolResult{Status: domain.ToolSuccess, Content: string(encoded)}, nil
}

// LoadSkillTool 读取一个 skill 的完整正文。
type LoadSkillTool struct {
	skillsDirs []string
}

func NewLoadSkill(skillsDirs ...string) *LoadSkillTool {
	return &LoadSkillTool{skillsDirs: skillsDirs}
}

func (tool *LoadSkillTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        "load_skill",
		Description: "读取指定 skill 的完整指令正文。先通过 list_skills 查看有哪些 skill 可用。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "skill 名称（即 skills 目录下的子目录名）"
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`),
	}
}

func (tool *LoadSkillTool) DescribeCall(call domain.ToolCall) string {
	args, err := parseArgs[loadSkillArgs](call.Arguments)
	if err != nil {
		return "读取 skill"
	}
	return fmt.Sprintf("读取 skill %s", args.Name)
}

type loadSkillArgs struct {
	Name string `json:"name"`
}

func (tool *LoadSkillTool) Run(ctx context.Context, call domain.ToolCall, onOutput domain.OutputFunc) (domain.ToolResult, error) {
	args, err := parseArgs[loadSkillArgs](call.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	// 防路径穿越：只允许最后一段作为目录名。
	clean := filepath.Base(filepath.Clean(args.Name))
	if clean == "." || clean == ".." || clean == string(filepath.Separator) {
		return errorResult(fmt.Sprintf("skill 名称 %q 无效", args.Name)), nil
	}
	for _, skillsDir := range tool.skillsDirs {
		path := filepath.Join(skillsDir, clean, "SKILL.md")
		data, err := os.ReadFile(path)
		if err == nil {
			return domain.ToolResult{Status: domain.ToolSuccess, Content: string(data)}, nil
		}
	}
	return errorResult(fmt.Sprintf("skill %q 不存在或无法读取", args.Name)), nil
}
