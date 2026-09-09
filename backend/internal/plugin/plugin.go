package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Plugin 描述一个已安装的插件。
type Plugin struct {
	// Name 是插件目录名，也是命名空间前缀。
	Name string `json:"name"`
	// Manifest 是 plugin.json 的内容。
	Manifest Manifest `json:"manifest"`
	// Dir 是插件目录的绝对路径。
	Dir string `json:"dir"`
	// Enabled 表示插件当前是否启用。
	Enabled bool `json:"enabled"`
	// SkillNames 是该插件提供的 skill 名称列表。
	SkillNames []string `json:"skill_names"`
	// CommandNames 是该插件提供的斜杠命令名列表。
	CommandNames []string `json:"command_names"`
}

// Manifest 对应 plugin.json。
type Manifest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
}

// Load 从插件目录读取 manifest 和组件清单。
//
// 目录里必须有 .goseek-plugin/plugin.json；缺它不算插件，Load 返回错误——
// 那个目录可能是用户随手放的杂物，不应当被当成能力注入。
func Load(dir string) (*Plugin, error) {
	manifestPath := filepath.Join(dir, ".goseek-plugin", "plugin.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", manifestPath, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", manifestPath, err)
	}
	if manifest.Name == "" {
		return nil, fmt.Errorf("%s 缺少 name 字段", manifestPath)
	}

	plugin := &Plugin{Name: filepath.Base(dir), Manifest: manifest, Dir: dir, Enabled: true}
	plugin.SkillNames = listSubdirs(filepath.Join(dir, "skills"))
	if plugin.SkillNames == nil {
		plugin.SkillNames = []string{}
	}
	plugin.CommandNames = listFiles(filepath.Join(dir, "commands"), ".md")
	if plugin.CommandNames == nil {
		plugin.CommandNames = []string{}
	}
	return plugin, nil
}

// SkillsDir 返回插件提供的 skill 目录。
func (plugin *Plugin) SkillsDir() string {
	return filepath.Join(plugin.Dir, "skills")
}

// CommandsDir 返回插件提供的斜杠命令目录。
func (plugin *Plugin) CommandsDir() string {
	return filepath.Join(plugin.Dir, "commands")
}

// listSubdirs 返回 dir 下的子目录名。目录不存在返回 nil——没有 skills/ 或
// commands/ 是正常的，只有 manifest 是必需的。
func listSubdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

// listFiles 返回 dir 下具有给定扩展名的文件名（去掉扩展名）。
func listFiles(dir, ext string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ext {
			names = append(names, entry.Name()[:len(entry.Name())-len(ext)])
		}
	}
	return names
}

// LoadAll 扫描 pluginsRoot 下的每个子目录，返回全部有效插件。
// 无效目录被静默跳过（记录到返回值之外），不阻塞其余插件的加载。
func LoadAll(pluginsRoot string) ([]*Plugin, error) {
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var plugins []*Plugin
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(pluginsRoot, entry.Name())
		plugin, err := Load(dir)
		if err != nil {
			continue
		}
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}
