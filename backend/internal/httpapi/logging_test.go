package httpapi

import (
	"io"
	"log/slog"
)

// slogLogger 只是为了让测试里的辅助函数有个短名字。
type slogLogger = slog.Logger

// newDiscardLogger 返回一个丢弃全部输出的 logger。
//
// 测试里不需要日志，而默认 logger 会把每个请求打到 stderr，把测试输出刷得没法看。
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
