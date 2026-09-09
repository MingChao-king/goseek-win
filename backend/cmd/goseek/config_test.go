package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTemporaryConfig 把配置目录指向临时目录，避免测试写到真实配置。
func useTemporaryConfig(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	return filepath.Join(directory, applicationName, configFileName)
}

// clearSettingEnvironment 清掉可能从外部继承来的配置环境变量。
func clearSettingEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{apiKeyKey, baseURLKey, modelKey} {
		t.Setenv(key, "")
	}
}

func TestReadEnvFileParsesCommentsBlankLinesQuotesAndEmbeddedEquals(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFileName)
	content := strings.Join([]string{
		"# 这是注释",
		"",
		"   ",
		`GOSEEK_API_KEY = "sk-带引号"`,
		"GOSEEK_BASE_URL='https://example.com/v1?a=1&b=2'",
		"GOSEEK_MODEL=  裸值  ",
		"没有等号的一行",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), configFilePerm); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}

	values, err := readEnvFile(path)
	if err != nil {
		t.Fatalf("readEnvFile 返回错误: %v", err)
	}

	if values[apiKeyKey] != "sk-带引号" {
		t.Errorf("%s = %q; want 去掉引号后的值", apiKeyKey, values[apiKeyKey])
	}
	// 取值里的等号必须保留，否则查询参数会被截断。
	if values[baseURLKey] != "https://example.com/v1?a=1&b=2" {
		t.Errorf("%s = %q", baseURLKey, values[baseURLKey])
	}
	if values[modelKey] != "裸值" {
		t.Errorf("%s = %q; want 去掉首尾空白", modelKey, values[modelKey])
	}
	if len(values) != 3 {
		t.Errorf("解析出 %d 个键: %v", len(values), values)
	}
}

// 首次运行时配置文件还不存在，这不是错误。
func TestReadEnvFileTreatsMissingFileAsEmpty(t *testing.T) {
	values, err := readEnvFile(filepath.Join(t.TempDir(), "不存在.env"))
	if err != nil {
		t.Fatalf("文件不存在被当成了错误: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("返回了 %d 个键", len(values))
	}
}

// 环境变量必须能覆盖配置文件，这样临时换 key 或模型不用改文件。
func TestLoadSettingsPrefersEnvironmentOverFile(t *testing.T) {
	path := useTemporaryConfig(t)
	clearSettingEnvironment(t)
	if err := os.MkdirAll(filepath.Dir(path), configDirPerm); err != nil {
		t.Fatalf("创建配置目录失败: %v", err)
	}
	content := "GOSEEK_API_KEY=文件里的key\nGOSEEK_MODEL=文件里的模型\n"
	if err := os.WriteFile(path, []byte(content), configFilePerm); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}
	t.Setenv(modelKey, "环境变量里的模型")

	active, err := loadSettings()
	if err != nil {
		t.Fatalf("loadSettings 返回错误: %v", err)
	}

	if active.Model != "环境变量里的模型" {
		t.Errorf("Model = %q; want 环境变量优先", active.Model)
	}
	if active.APIKey != "文件里的key" {
		t.Errorf("APIKey = %q; want 来自配置文件", active.APIKey)
	}
	if active.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL = %q; want 内置默认值 %q", active.BaseURL, defaultBaseURL)
	}
}

// 写入 key 不能抹掉用户已经配好的其他键。
func TestSaveAPIKeyKeepsOtherSettings(t *testing.T) {
	path := useTemporaryConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), configDirPerm); err != nil {
		t.Fatalf("创建配置目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte("GOSEEK_MODEL=我选的模型\n"), configFilePerm); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}

	if err := saveAPIKey("sk-新的"); err != nil {
		t.Fatalf("saveAPIKey 返回错误: %v", err)
	}

	values, err := readEnvFile(path)
	if err != nil {
		t.Fatalf("readEnvFile 返回错误: %v", err)
	}
	if values[apiKeyKey] != "sk-新的" {
		t.Errorf("%s = %q", apiKeyKey, values[apiKeyKey])
	}
	if values[modelKey] != "我选的模型" {
		t.Errorf("已有的 %s 被抹掉了: %q", modelKey, values[modelKey])
	}
}

// 配置文件里是 API key，权限必须限制到本用户。
func TestSaveAPIKeyWritesOwnerOnlyFile(t *testing.T) {
	path := useTemporaryConfig(t)

	if err := saveAPIKey("sk-秘密"); err != nil {
		t.Fatalf("saveAPIKey 返回错误: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != configFilePerm {
		t.Errorf("配置文件权限 = %o; want %o", permissions, configFilePerm)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat 目录失败: %v", err)
	}
	if permissions := directory.Mode().Perm(); permissions != configDirPerm {
		t.Errorf("配置目录权限 = %o; want %o", permissions, configDirPerm)
	}
}

func TestPromptForAPIKeySavesWhatTheUserTyped(t *testing.T) {
	path := useTemporaryConfig(t)
	var output strings.Builder

	key, err := promptForAPIKey(bufio.NewReader(strings.NewReader("  sk-输入的  \n")), &output)
	if err != nil {
		t.Fatalf("promptForAPIKey 返回错误: %v", err)
	}
	if key != "sk-输入的" {
		t.Errorf("返回的 key = %q", key)
	}

	values, err := readEnvFile(path)
	if err != nil {
		t.Fatalf("readEnvFile 返回错误: %v", err)
	}
	if values[apiKeyKey] != "sk-输入的" {
		t.Errorf("落盘的 key = %q", values[apiKeyKey])
	}
	// 提示里必须说明输入会显示出来，用户才能自己决定要不要现在输入。
	if !strings.Contains(output.String(), "会显示") {
		t.Errorf("提示没有说明输入会回显:\n%s", output.String())
	}
	if !strings.Contains(output.String(), path) {
		t.Errorf("提示没有告诉用户配置写到哪里:\n%s", output.String())
	}
}

func TestPromptForAPIKeyRejectsEmptyInput(t *testing.T) {
	useTemporaryConfig(t)
	var output strings.Builder

	if _, err := promptForAPIKey(bufio.NewReader(strings.NewReader("\n")), &output); err == nil {
		t.Error("空输入被接受了")
	}
}

// XDG 变量必须是绝对路径，否则配置位置会随启动目录漂移——而 goseek 就是要在
// 任意目录启动的。
func TestUserDirIgnoresRelativeXDGPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "相对路径")

	directory, err := configDir()
	if err != nil {
		t.Fatalf("configDir 返回错误: %v", err)
	}
	if strings.HasPrefix(directory, "相对路径") {
		t.Errorf("configDir = %q; want 忽略相对路径的 XDG 取值", directory)
	}
	if !filepath.IsAbs(directory) {
		t.Errorf("configDir = %q; want 绝对路径", directory)
	}
}

// 窗口按"用户配置 > 模型表 > 兜底"确定，而且要说得出来源。
//
// 来源必须显式，是因为这个值曾经悄悄错了 8 倍（128000 对 1048576）而没人发现——
// 界面上只显示"128K"，看不出它是猜的。
func TestResolveContextWindow(t *testing.T) {
	cases := []struct {
		name       string
		raw, model string
		wantWindow int
		wantSource contextWindowSource
	}{
		{"显式配置优先", "200000", "deepseek-v4-flash", 200000, windowFromConfig},
		{"没配置就查模型表", "", "deepseek-v4-flash", 1048576, windowFromModelTable},
		{"模型不认识就兜底", "", "某个没见过的模型", fallbackContextWindow, windowFromFallback},
		{"显式配 0 表示未知", "0", "deepseek-v4-flash", 0, windowUnknown},
		{"配了非数字也按未知", "很大", "deepseek-v4-flash", 0, windowUnknown},
		{"配了负数也按未知", "-1", "deepseek-v4-flash", 0, windowUnknown},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			window, source := resolveContextWindow(testCase.raw, testCase.model)
			if window != testCase.wantWindow {
				t.Errorf("窗口 = %d; want %d", window, testCase.wantWindow)
			}
			if source != testCase.wantSource {
				t.Errorf("来源 = %q; want %q", source, testCase.wantSource)
			}
		})
	}
}

// 兜底来源要在横幅上显式提示——它意味着这个数多半是错的，而错了的后果不是
// "显示不准"，是压缩在完全不必要的时候反复触发。
func TestBannerWarnsWhenWindowIsGuessed(t *testing.T) {
	guessed := describeWindow(settings{
		Model: "某个没见过的模型", ContextWindow: fallbackContextWindow,
		ContextWindowSource: windowFromFallback,
	})
	if !strings.Contains(guessed, "GOSEEK_CONTEXT_WINDOW") {
		t.Errorf("兜底时没有提示怎么改:\n%s", guessed)
	}

	known := describeWindow(settings{
		Model: "deepseek-v4-flash", ContextWindow: 1048576,
		ContextWindowSource: windowFromModelTable,
	})
	if strings.Contains(known, "⚠") {
		t.Errorf("查到了模型表却还在警告:\n%s", known)
	}
	if !strings.Contains(known, "1048576") {
		t.Errorf("没有显示窗口大小:\n%s", known)
	}

	unknown := describeWindow(settings{ContextWindowSource: windowUnknown})
	if !strings.Contains(unknown, "不会自动压缩") {
		t.Errorf("未知窗口没有说明后果:\n%s", unknown)
	}
}
