package model

// CatalogEntry 描述一个 GoSeek 直接支持的模型。
//
// Name 使用网关返回的完整名称，不剥前缀：同名模型经由不同网关的有效窗口可能不同。
type CatalogEntry struct {
	Name              string
	Display           string
	ContextWindow     int
	WindowSource      string
	MeasuredAt        string
	SupportsTools     bool
	SupportsStreaming bool
	// SupportsVision 表示该模型是否接受 content parts 里的 image_url。
	SupportsVision bool
}

// Catalog 是当前支持的模型目录。这里刻意不动态拉取 /models：接口不返回窗口大小，
// 动态列表只会制造“能看见但不能选”的入口。
//
// Name 必须是网关/供应商返回的**完整模型名**（例如中转加的前缀，如果有），
// 与 GOSEEK_MODEL 及会话里保存的 model 字段一致——目录条目一旦改名，所有
// 旧会话的模型都会 Lookup 失败回退兜底窗口（真实事故：为开源改名后仪表盘
// 显示 534%、压缩阈值全错）。
var Catalog = []CatalogEntry{
	{
		Name:              "deepseek-v4-pro",
		Display:           "DeepSeek V4 Pro（示例）",
		ContextWindow:     1048576,
		WindowSource:      "gateway-measured",
		MeasuredAt:        "2026-09-01",
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    false,
	},
	{
		Name:              "deepseek-v4-flash",
		Display:           "DeepSeek V4 Flash（示例）",
		ContextWindow:     1048576,
		WindowSource:      "gateway-measured",
		MeasuredAt:        "2026-09-01",
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    false,
	},
	{
		Name:              "glm-5.3",
		Display:           "GLM 5.3（示例，多模态）",
		ContextWindow:     1048576,
		WindowSource:      "gateway-measured",
		MeasuredAt:        "2026-09-01",
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    true,
	},
	{
		Name:              "glm-5.3-flash",
		Display:           "GLM 5.3 Flash（示例，多模态）",
		ContextWindow:     1048576,
		WindowSource:      "gateway-measured",
		MeasuredAt:        "2026-09-01",
		SupportsTools:     true,
		SupportsStreaming: true,
		SupportsVision:    true,
	},
}

// Lookup 返回目录中的模型。未命中返回 false；调用方应继续走配置或兜底逻辑。
func Lookup(name string) (CatalogEntry, bool) {
	for _, entry := range Catalog {
		if entry.Name == name {
			return entry, true
		}
	}
	return CatalogEntry{}, false
}

// Contains 报告模型名是否在目录中。
func Contains(name string) bool {
	_, found := Lookup(name)
	return found
}
