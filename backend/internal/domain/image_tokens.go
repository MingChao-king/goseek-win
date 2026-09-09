package domain

// 图片 token 的精确估算。
//
// GLM 5.3 Flash 按 28×28 像素 patch 动态分辨率换算，2026-09-02 通过 AISwitch
// 实测 21 组不同尺寸（PNG / JPEG、正方形 / 长条形、小图 / 大图），全部精确
// 命中以下公式，误差 0 token（见设计文档 5.20.4 与现状文档 9.28）。
//
// 该公式天然满足"估算 ≥ 实测"的单侧契约：公式本身就是服务端的精确换算，
// 乘上安全系数之后只会更高。

const (
	// imagePatchSize 是每个视觉 patch 的边长（像素）。
	imagePatchSize = 28
	// imageMinPatches 是单边 patch 数的下限。小于约 112×112 px 的图被上采样。
	imageMinPatches = 4
	// imageMaxPatches 是单边 patch 数的上限。大于约 2492×2492 px 的图被下采样。
	imageMaxPatches = 89
	// imageMarkerTokens 是图片起止标记占用的固定开销。
	imageMarkerTokens = 2
)

// EstimateImageTokens 按实测公式估算一张图片的视觉 token 数。
//
// 宽高为 0 或负数时返回最小值（服务端会按最小分辨率处理）。
func EstimateImageTokens(width, height int) int {
	patchesW := clampPatches(ceilDiv(width, imagePatchSize))
	patchesH := clampPatches(ceilDiv(height, imagePatchSize))
	return patchesW*patchesH + imageMarkerTokens
}

// ceilDiv 返回 a/b 向上取整的值。a<=0 时返回 0。
func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// clampPatches 把单边 patch 数夹到服务端的上下限之间。
func clampPatches(n int) int {
	if n < imageMinPatches {
		return imageMinPatches
	}
	if n > imageMaxPatches {
		return imageMaxPatches
	}
	return n
}
