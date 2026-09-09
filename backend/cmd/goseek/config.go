// 本文件负责一件事：确定这次运行用哪个 API key、哪个端点、哪个模型、多大的
// 上下文窗口。
//
// 取值顺序固定为**环境变量 > 配置文件 > 内置默认值**，而且键名三处完全同名。
// 同名是有意的：想临时换一个模型跑一次，`GOSEEK_MODEL=xxx goseek` 就行，不用
// 改文件、不用记第二套名字。
//
// 配置里有 API key，所以文件 0600、目录 0700，并且这个值在任何地方都不打印——
// 报错信息里也不带，否则它会随着错误日志被复制到各种地方。
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"goseek/internal/contextmgr"

	"goseek/internal/model"
)

const (
	// configFileName 是配置文件名。用 .env 的形式而不是 YAML：当前只有四个键，
	// 引入一套配置结构和解析器的收益是负的。而且 .env 是所有人都认得的形式，
	// 用文本编辑器改一行就能生效。
	configFileName = "config.env"

	// configFilePerm 让配置文件只有当前用户可读写——里面是 API key。
	//
	// 0600 = 属主读写，同组和其他人什么都不能做。默认的 0644 会让同机器上的
	// 任何账号都能读到这个 key。
	configFilePerm = 0o600
	// configDirPerm 同理限制目录：0700 = 只有属主能进入和列出。
	//
	// 目录也要管：文件权限管住"能不能读内容"，目录权限管住"能不能看到有哪些
	// 文件、能不能把文件换掉"。
	configDirPerm = 0o700

	// 配置项的键名与环境变量完全同名，因此取值逻辑只有一句话：
	// 环境变量 > 配置文件 > 内置默认值。临时换 key 或模型不用改文件。
	apiKeyKey        = "GOSEEK_API_KEY"
	baseURLKey       = "GOSEEK_BASE_URL"
	modelKey         = "GOSEEK_MODEL"
	contextWindowKey = "GOSEEK_CONTEXT_WINDOW"

	// defaultBaseURL 是未配置时使用的 OpenAI-compatible 端点前缀。
	//
	// 与 E3 保持一致。DeepSeek 同时接受带 /v1 和不带 /v1 的前缀，两种都实测可用。
	defaultBaseURL = "https://api.deepseek.com"
	// defaultModel 是未配置时使用的模型名称，与 E3 使用的模型一致。
	defaultModel = "deepseek-v4-flash"

	// fallbackContextWindow 是**连模型名都不认识**时的兜底窗口。
	//
	// 取一个明显偏小的保守值：估小了只会让压缩提前触发（多花点钱、早一点变成
	// 摘要），估大了会让请求被供应商直接拒。两个方向的代价不对称，所以往小了取。
	//
	// 配置成 0 表示"不知道"：此时界面显示"未知"而不是编一个比例，压缩也不会
	// 基于一个假的窗口触发。
	fallbackContextWindow = 128000
)

// knownContextWindows 是各模型的上下文窗口，单位 token。
//
// # 为什么必须按模型分别写，而不是一个全局默认值
//
// 供应商的 /models 接口**只返回 id、object、owned_by，不返回窗口大小**（2026-08-27
// 实测），所以这个值只能由我们自己维护。而一个全局默认值必然会错——它对写下它的
// 那一刻的那个模型是对的，换个模型、或者同一个模型升级之后就悄悄错了。
//
// 这件事已经真实发生过一次：这里原本写死 128000（DeepSeek v3 时代的规格），而
// deepseek-v4-flash 的实际窗口是 1048576，**差 8.2 倍**。后果不是"显示不准"——
// 触发线因此落在真实窗口的 9% 处，压缩在完全不必要的时候反复触发，既花钱又把
// 好好的原文变成了有损摘要。
//
// # 这些数字是怎么来的
//
// 实测。发一个超长请求让供应商拒绝，它的错误信息里带着真值：
//
//	This model's maximum context length is 1048576 tokens. However, you
//	requested 1200085 tokens (1200084 in the messages, 1 in the completion).
//
// 同一次探测还确认了输出上限：max_tokens 的合法区间是 [1, 393216]。
//
// 加新模型时照这个办法量一遍，不要照抄文案宣传的数字。
// 窗口表与 model.Catalog 是同一份事实：按模型名从 Catalog 生成。
// 单独维护一份曾经真实翻过车——为开源把 Catalog 条目改名后，这里没跟上，
// CLI 启动横幅对正在使用的模型报"不在已知窗口表里"。
func knownContextWindows() map[string]int {
	tables := make(map[string]int, len(model.Catalog))
	for _, entry := range model.Catalog {
		tables[entry.Name] = entry.ContextWindow
	}
	return tables
}

// contextWindowSource 说明这次用的窗口值是哪来的，用于启动横幅与界面提示。
type contextWindowSource string

const (
	// windowFromConfig 表示用户显式配置了 GOSEEK_CONTEXT_WINDOW。
	windowFromConfig contextWindowSource = "配置"
	// windowFromModelTable 表示按模型名从 knownContextWindows 查到的。
	windowFromModelTable contextWindowSource = "模型表"
	// windowFromFallback 表示模型名不认识，用了保守的兜底值。
	//
	// 这个来源要在启动时显式提示——它意味着窗口多半是错的。
	windowFromFallback contextWindowSource = "兜底（模型未知）"
	// windowUnknown 表示用户显式配置成了 0，即"不知道"。
	windowUnknown contextWindowSource = "未知"
)

// resolveContextWindow 按"用户配置 > 模型表 > 兜底"确定窗口，并说明来源。
func resolveContextWindow(raw, model string) (int, contextWindowSource) {
	if raw != "" {
		window := parseContextWindow(raw)
		if window == 0 {
			return 0, windowUnknown
		}
		return window, windowFromConfig
	}
	if window, known := knownContextWindows()[model]; known {
		return window, windowFromModelTable
	}
	return fallbackContextWindow, windowFromFallback
}

// settings 是本次运行的有效配置。
type settings struct {
	APIKey  string
	BaseURL string
	Model   string
	// ContextWindow 是模型一次调用的总上下文容量。0 表示未知。
	ContextWindow int
	// ContextWindowSource 说明上面那个值是哪来的。
	//
	// 单独记下来是因为这个值曾经悄悄错了 8 倍而没人发现（见 knownContextWindows
	// 的注释）。把来源显式化，启动横幅就能提示"这个数是猜的"。
	ContextWindowSource contextWindowSource
}

// loadSettings 按"环境变量 > 配置文件 > 默认值"解析配置。
//
// 配置文件不存在不是错误：首次运行本来就没有它，此时 APIKey 为空，由调用方
// （resolveSettings）决定是提示用户输入还是直接退出。
//
// 空字符串一律当作"没设置"：`GOSEEK_MODEL= goseek` 这种写法在 shell 里很常见，
// 把它理解成"要一个空模型名"没有任何意义，只会让请求带着空 model 字段发出去。
func loadSettings() (settings, error) {
	path, err := configFilePath()
	if err != nil {
		return settings{}, err
	}
	stored, err := readEnvFile(path)
	if err != nil {
		return settings{}, err
	}

	// pick 实现那条取值顺序。写成闭包而不是四段重复的 if，是为了让"顺序只有
	// 一处定义"——四个键各写一遍的话，迟早有一个的顺序被改错。
	pick := func(key, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
		if value := strings.TrimSpace(stored[key]); value != "" {
			return value
		}
		return fallback
	}
	model := pick(modelKey, defaultModel)
	window, source := resolveContextWindow(pick(contextWindowKey, ""), model)
	// 窗口太小时**启动就报错**，而不是等到第几百轮的某一次请求被供应商拒绝。
	//
	// 在小于最小可行窗口的配置里，"压缩一定能把上下文压到硬边界以下"这条保证不
	// 成立，而它是整套记忆结构的地基。让它在这里失败，用户得到的是一句能照着改的
	// 配置说明；让它在运行时失败，用户得到的是供应商的一句 400，而且根本看不出
	// 是窗口配小了。理由的详细版见 contextmgr.CheckWindow。
	if err := contextmgr.CheckWindow(window); err != nil {
		return settings{}, fmt.Errorf("%w（当前值来自%s，用 %s 可以覆盖）",
			err, source, contextWindowKey)
	}
	return settings{
		APIKey:              pick(apiKeyKey, ""),
		BaseURL:             pick(baseURLKey, defaultBaseURL),
		Model:               model,
		ContextWindow:       window,
		ContextWindowSource: source,
	}, nil
}

// parseContextWindow 解析用户配置的窗口值。
//
// 填了但不是正整数，按"未知"（0）处理而不是悄悄换成别的值——用户显然想指定一个
// 值，把他的错误配置替换掉只会让他对着一个错误的比例排查很久。返回 0 让界面明确
// 显示"未知"。
//
// 空值不在这里处理：那属于"没配置"，由 resolveContextWindow 去查模型表。
func parseContextWindow(raw string) int {
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// configFilePath 返回配置文件的完整路径。
func configFilePath() (string, error) {
	directory, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, configFileName), nil
}

// readEnvFile 解析 KEY=value 形式的配置文件。
//
// 文件不存在时返回空表：这是首次运行的正常情形，不是错误。
//
// 解析刻意宽松——空行、# 注释、没有等号的行统统跳过，不报错。这个文件是用户
// 手写的，为一行笔误就拒绝启动，不如忽略那一行、让他从"key 没生效"里发现问题。
// 反过来，格式严格并不能换来任何安全性：这里没有会被利用的语法。
func readEnvFile(path string) (map[string]string, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 只按第一个等号切分：取值里可以含等号，例如 base URL 的查询参数。
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return values, nil
}

// unquote 去掉取值两侧成对的引号。
//
// 用户手写配置时习惯加引号，而引号不是取值的一部分——带着它去做 Bearer 认证
// 会得到一个难以理解的 401。
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// saveAPIKey 把 API key 写入配置文件，保留文件中已有的其他键。
//
// 先读回已有内容再整体重写，而不是往文件末尾追加一行：追加会在文件里留下同一个
// 键的两份取值，读的时候后一份覆盖前一份——能用，但用户打开文件会看到自相矛盾
// 的两行，无从判断哪个在生效。
//
// 只写非空的键：把 GOSEEK_MODEL= 这样的空行写进去，只会让人以为自己配置过。
func saveAPIKey(key string) error {
	directory, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, configDirPerm); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	path := filepath.Join(directory, configFileName)
	stored, err := readEnvFile(path)
	if err != nil {
		return err
	}
	stored[apiKeyKey] = key

	var builder strings.Builder
	builder.WriteString("# GoSeek 配置。环境变量优先于本文件。\n")
	for _, name := range []string{apiKeyKey, baseURLKey, modelKey, contextWindowKey} {
		if value := stored[name]; value != "" {
			fmt.Fprintf(&builder, "%s=%s\n", name, value)
		}
	}
	if err := os.WriteFile(path, []byte(builder.String()), configFilePerm); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}

// promptForAPIKey 在没有配置 key 时向用户索取一个，并写入配置文件。
//
// 输入不做回显屏蔽：标准库没有这个能力，为它引入 golang.org/x/term 不值得，
// 因此提示里明确告诉用户输入会显示出来，让他自己决定要不要在共享屏幕时操作。
func promptForAPIKey(input *bufio.Reader, output io.Writer) (string, error) {
	path, err := configFilePath()
	if err != nil {
		return "", err
	}

	fmt.Fprintf(output, "未找到 API key。GoSeek 需要一个 OpenAI-compatible 的 API key。\n")
	fmt.Fprintf(output, "配置会写入 %s（权限 0600）。\n", path)
	fmt.Fprintf(output, "输入的内容会显示在终端上。\n\n")
	fmt.Fprintf(output, "API key: ")

	line, err := input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("没有输入 API key")
	}
	if err := saveAPIKey(key); err != nil {
		return "", err
	}

	// 不回显刚保存的 key，只说保存成功。终端记录、截图、录屏都会带走它。
	fmt.Fprintf(output, "\n已保存。以后直接运行 goseek 即可。\n\n")
	return key, nil
}
