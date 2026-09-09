package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"goseek/internal/httpapi"
	"goseek/internal/model"
)

// builtinPluginsFS 挂载 go:embed 打包的内置插件（plugins/ 目录）。
//
//go:embed all:plugins
var builtinPluginsFS embed.FS

// frontendFS 挂载 go:embed 打包的前端静态文件（dist/ 目录）。
// 由 make build 在编译前从 front/dist 拷贝过来；目录缺失时构建会失败，
// 这是刻意的——App 分发必须带前端。

//go:embed all:dist
var frontendFS embed.FS

const (
	// defaultServeAddr 是 HTTP 服务的默认监听地址。
	//
	// 只监听回环地址。服务不做鉴权，而它能在本机执行任意命令——绑到 0.0.0.0
	// 等于把一个远程 shell 交给同网段的任何人。要对外提供必须先有鉴权，
	// 那不在本阶段范围内。
	defaultServeAddr = "127.0.0.1:8765"

	// shutdownTimeout 是优雅关闭的等待上限。
	//
	// SSE 连接会一直开着，Shutdown 会等它们结束——而它们只在客户端断开时才结束。
	// 因此这里必须有上限，否则一个开着的浏览器标签页就能让服务永远关不掉。
	shutdownTimeout = 5 * time.Second
)

// runServe 启动 HTTP 服务，直到收到中断信号。
func runServe(arguments []string) error {
	//获取后端服务启动地址
	address, err := parseServeOptions(arguments)
	if err != nil {
		return err
	}

	// 服务端不做交互式索取 key：它可能跑在没有终端的地方，而且一个卡在
	// "请输入 API key" 的服务比一个明确退出的服务更难排查。
	active, err := loadSettings()
	if err != nil {
		return err
	}
	if active.APIKey == "" {
		return fmt.Errorf("未找到 API key。先运行一次 goseek 配置它，或设置环境变量 %s", apiKeyKey)
	}
	//数据目录
	directory, err := dataDir()
	if err != nil {
		return err
	}
	//JSON格式日志处理器，级别为info及以上
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 传一个工厂而不是单个实例：每个会话拿到自己的模型客户端和连接池，
	// 一个会话的慢请求不会拖住别的会话。
	newModel := func(modelName string) httpapi.ModelClient {
		return model.New(model.Config{
			BaseURL: active.BaseURL,
			APIKey:  active.APIKey,
			Model:   modelName,
		})
	}
	//组装一个Http服务
	httpapi.BuiltinPluginsFS = builtinPluginsFS
	server, err := httpapi.NewServer(directory, frontendFS, newModel, active.Model, active.ContextWindow, logger)
	if err != nil {
		return err
	}
	defer server.Close()
	//真正用来处理Http服务
	httpServer := &http.Server{
		Addr: address,
		//handler定义了一些，诸如创建会话，根据id获取会话的接口
		//比如说，如果要发起一轮新对话，前端就会调用POST /api/v1/sessions/{id}/turns
		Handler: server.Handler(),
		// 不设 WriteTimeout：SSE 连接本来就要长期开着，写超时会把它们定期掐断。
		// 读超时可以设，请求体都很小。
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 收到 Ctrl-C 或 SIGTERM 时取消 context，触发优雅关闭。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务已启动", "addr", address, "data_dir", directory)
		fmt.Printf("GoSeek 服务已启动: http://%s\n模型 %s，上下文窗口 %s\n按 Ctrl-C 停止。\n",
			address, active.Model, describeWindow(active))
		// ErrServerClosed 是 Shutdown 触发的正常结束，不算错误。
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	logger.Info("正在停止服务")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// 超时说明还有连接没断（多半是 SSE）。强制关掉——server.Close 随后会
		// 停掉 Runner 并释放会话锁，那比让进程挂着更重要。
		logger.Warn("优雅关闭超时，强制结束", "error", err)
	}
	return nil
}

// parseServeOptions 解析 serve 子命令的参数。
func parseServeOptions(arguments []string) (string, error) {
	flags := newFlagSet("goseek serve")
	address := flags.String("addr", defaultServeAddr, "HTTP 监听地址")
	if err := flags.Parse(arguments); err != nil {
		return "", err
	}
	return *address, nil
}
