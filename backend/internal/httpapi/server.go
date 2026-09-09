package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"goseek/internal/browser"
	"goseek/internal/contextmgr"
	"goseek/internal/domain"
	"goseek/internal/mcp"
	"goseek/internal/model"
	"goseek/internal/plugin"
	"goseek/internal/store"
)

// Server 是 GoSeek 的 HTTP 接口。
//
// 它把读写两条路分开：
//
//   - **读**（列表、快照、事件重放）走 store.Reader，不加锁、不经过 Runner，
//     因此一轮交互正在进行时也能立刻响应；
//   - **写**（新建会话、提交消息）走 Runner，每个打开的会话一个，它是那个会话
//     状态的唯一写者。
type Server struct {
	dataDirectory string
	// frontendFS 是打包进二进制的前端静态文件（SPA）。nil 表示纯后端模式。
	frontendFS fs.FS
	// isolatedBrowsers 是每个会话独立的隔离浏览器实例（懒创建）——不同会话
	// 之间的浏览器页面互不可见。侧栏实时画面与人工点击都走这里。
	isolatedBrowsers map[domain.SessionID]*browser.Browser
	browserMu        sync.Mutex
	reader           *store.Reader
	// newModel 每次创建 Runner 时产出一个模型客户端。传函数而不是实例，是为了
	// 让每个会话有自己的 HTTP 客户端和连接池，一个会话的慢请求不拖住别的会话。
	newModel func(string) ModelClient
	// contextWindow 是模型的上下文窗口，由配置提供，透传给每个 Runner。
	defaultContextWindow int
	defaultModel         string
	logger               *slog.Logger

	// mutex 保护 runners。它只在"查找或创建 Runner"这一小段里持有，不会包住
	// 任何 I/O——创建 Runner 要打开数据库和加锁，因此那部分必须在锁外面。
	mutex   sync.Mutex
	runners map[domain.SessionID]*Runner
	closed  bool
}

const legacyBrowserDataDirectory = "/tmp/goseek-isolated-browser"

// NewServer 组装 HTTP 服务。
func NewServer(
	dataDirectory string,
	frontendFS fs.FS,
	newModel func(string) ModelClient,
	defaultModel string,
	defaultContextWindow int,
	logger *slog.Logger,
) (*Server, error) {
	// 先用写路径打开一次：它负责建目录、建库、执行未应用的迁移。Reader 只读，
	// 遇到不存在的库只会报 "unable to open database file"——首次启动必然撞上。
	// 开完就关，锁不会被长期占着。
	//确保数据库存在并且已经迁移到最新版本
	if err := ensureDatabase(dataDirectory); err != nil {
		return nil, err
	}
	//数据库只读句柄
	reader, err := store.NewReader(dataDirectory)
	if err != nil {
		return nil, err
	}
	return &Server{
		dataDirectory:        dataDirectory,
		frontendFS:           frontendFS,
		reader:               reader,
		newModel:             newModel,
		defaultContextWindow: defaultContextWindow,
		defaultModel:         defaultModel,
		logger:               logger,
		runners:              make(map[domain.SessionID]*Runner),
	}, nil
}

// Handler 返回带日志中间件的路由。
//
// 用标准库的 ServeMux。Go 1.22 起它支持方法和路径参数（"POST /a/{id}/b"），
// 常见的第三方路由库能提供的东西在这个规模下都用不上
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	//会话创建
	mux.HandleFunc("POST /api/v1/sessions", server.createSession)
	//列出会话
	mux.HandleFunc("GET /api/v1/sessions", server.listSessions)
	mux.HandleFunc("GET /api/v1/sessions/running", server.listRunningSessions)
	//根据id拿到对话
	mux.HandleFunc("GET /api/v1/sessions/{id}", server.getSession)
	//一轮对话的开始
	mux.HandleFunc("POST /api/v1/sessions/{id}/turns", server.submitTurn)
	//用户明确要求的一次压缩（面板上的 /compact）
	mux.HandleFunc("POST /api/v1/sessions/{id}/compact", server.compactSession)
	//完整的摘要树，供面板逐层展开
	mux.HandleFunc("GET /api/v1/sessions/{id}/memory", server.getMemory)
	//人工修订一段活跃摘要
	mux.HandleFunc("PATCH /api/v1/sessions/{id}/memory/{batchID}", server.editMemory)
	//归档 / 取消归档 / 重命名
	mux.HandleFunc("PATCH /api/v1/sessions/{id}", server.updateSession)
	//彻底删除一个会话（连同它的消息、事件、摘要）
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", server.deleteSession)
	//中止当前这一轮
	mux.HandleFunc("POST /api/v1/sessions/{id}/turns/cancel", server.cancelTurn)
	//根据id获取事件信息
	mux.HandleFunc("GET /api/v1/sessions/{id}/events", server.streamEvents)
	mux.HandleFunc("GET /api/v1/models", server.listModels)
	mux.HandleFunc("GET /api/v1/workspace/default", server.defaultWorkspaceHandler)
	mux.HandleFunc("GET /api/v1/sessions/{id}/file", server.getSessionFile)
	mux.HandleFunc("GET /api/v1/browser/stream", server.browserStream)
	mux.HandleFunc("POST /api/v1/browser/click", server.browserClick)
	mux.HandleFunc("POST /api/v1/browser/scroll", server.browserScroll)
	mux.HandleFunc("POST /api/v1/browser/viewport", server.browserViewport)
	mux.HandleFunc("POST /api/v1/browser/text", server.browserInsertText)
	mux.HandleFunc("POST /api/v1/browser/key", server.browserKey)
	mux.HandleFunc("GET /api/v1/browser/url", server.browserURL)
	mux.HandleFunc("/api/v1/browser/proxy/", server.browserProxy)
	mux.HandleFunc("GET /api/v1/browser/tabs", server.browserTabs)
	mux.HandleFunc("POST /api/v1/browser/tabs/activate", server.browserActivateTab)
	mux.HandleFunc("DELETE /api/v1/browser/tabs/{targetID}", server.browserCloseTab)
	mux.HandleFunc("POST /api/v1/browser/navigate", server.browserNavigate)
	mux.HandleFunc("GET /api/v1/browser/screenshot", server.browserScreenshot)
	mux.HandleFunc("GET /api/v1/browser/read-page", server.browserReadPage)
	mux.HandleFunc("POST /api/v1/browser/click-element", server.browserClickElement)
	mux.HandleFunc("POST /api/v1/browser/type", server.browserTypeText)
	mux.HandleFunc("POST /api/v1/sessions/{id}/images", server.uploadImage)
	mux.HandleFunc("GET /api/v1/images/{imageID}", server.getImage)
	mux.HandleFunc("GET /api/v1/skills", server.listSkills)
	mux.HandleFunc("GET /api/v1/skills/{name}", server.getSkill)
	mux.HandleFunc("POST /api/v1/skills/{name}", server.uploadSkill)
	mux.HandleFunc("DELETE /api/v1/skills/{name}", server.deleteSkill)
	mux.HandleFunc("GET /api/v1/plugins", server.listPlugins)
	mux.HandleFunc("PATCH /api/v1/plugins/{name}", server.togglePlugin)
	mux.HandleFunc("DELETE /api/v1/plugins/{name}", server.deletePlugin)
	mux.HandleFunc("GET /api/v1/mcp", server.listMCP)
	mux.HandleFunc("POST /api/v1/mcp", server.setMCP)
	mux.HandleFunc("DELETE /api/v1/mcp/{name}", server.deleteMCP)

	// 前端静态文件（SPA 兜底）：非 /api 开头的路径都交给 index.html，
	// 前端 Router 接管。frontendFS 为 nil 表示纯后端模式（开发时由 Vite 提供前端）。
	if server.frontendFS != nil {
		mux.HandleFunc("/", server.serveFrontend)
	}

	return server.withLogging(mux)
}

// listModels 返回 GoSeek 支持的模型目录。
func (server *Server) listModels(writer http.ResponseWriter, request *http.Request) {
	items := make([]modelInfoView, 0, len(model.Catalog))
	for _, entry := range model.Catalog {
		thresholds := contextmgr.NewThresholds(entry.ContextWindow)
		items = append(items, modelInfoView{
			Name:                   entry.Name,
			Display:                entry.Display,
			ContextWindow:          entry.ContextWindow,
			EffectiveContextWindow: thresholds.HardLimit,
			CompactionTrigger:      thresholds.CompactAt,
			WindowSource:           entry.WindowSource,
			MeasuredAt:             entry.MeasuredAt,
		})
	}
	writeJSON(writer, http.StatusOK, map[string][]modelInfoView{"models": items})
}

// defaultWorkspaceHandler 返回服务进程当前目录，供新建会话弹窗作为默认值。
func (server *Server) defaultWorkspaceHandler(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"path": defaultWorkspace()})
}

// getSessionFile 返回会话 workspace 内的一个文本文件内容，供侧栏点击文件查看。
// 路径必须是 workspace 内的相对路径（防路径穿越），文件大小限制 1 MB。
func (server *Server) getSessionFile(writer http.ResponseWriter, request *http.Request) {
	sessionID := domain.SessionID(request.PathValue("id"))
	runner, err := server.runnerFor(sessionID)
	if err != nil {
		writeError(writer, http.StatusNotFound, "session_not_found", "会话不存在")
		return
	}
	relPath := request.URL.Query().Get("path")
	if relPath == "" {
		writeError(writer, http.StatusBadRequest, "missing_path", "缺少 path 参数")
		return
	}
	workspace := runner.session.Workspace
	absPath := filepath.Join(workspace, relPath)
	// 防路径穿越：解析后的绝对路径必须在 workspace 之内。
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		server.writeInternal(writer, "解析工作目录失败", err)
		return
	}
	absPath, err = filepath.Abs(absPath)
	if err != nil || !strings.HasPrefix(absPath, absWorkspace+string(filepath.Separator)) {
		writeError(writer, http.StatusForbidden, "path_outside_workspace", "路径超出工作目录范围")
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		writeError(writer, http.StatusNotFound, "file_not_found", "文件不存在或是目录")
		return
	}
	if info.Size() > 1024*1024 {
		writeError(writer, http.StatusRequestEntityTooLarge, "file_too_large", "文件超过 1 MB，请用终端查看")
		return
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		server.writeInternal(writer, "读取文件失败", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"path":    relPath,
		"content": string(data),
	})
}

// getIsolatedBrowser 返回指定会话独立的隔离浏览器实例（懒创建）。
// 不同会话的浏览器互不互通。
func (server *Server) getIsolatedBrowser(sessionID domain.SessionID) (*browser.Browser, error) {
	server.browserMu.Lock()
	defer server.browserMu.Unlock()
	if br, err := server.restoreIsolatedBrowserLocked(sessionID); err != nil || br != nil {
		return br, err
	}

	dataDirectory := server.sessionBrowserDirectory(sessionID)
	owner, err := server.legacyBrowserOwner()
	if err != nil {
		return nil, err
	}
	if owner == sessionID {
		dataDirectory = legacyBrowserDataDirectory
	}
	br, err := browser.New(dataDirectory)
	if err != nil {
		return nil, err
	}
	server.isolatedBrowsers[sessionID] = br
	return br, nil
}

// restoreIsolatedBrowserLocked 只恢复仍存活的浏览器，不启动新进程。
// 调用方必须持有 browserMu。
func (server *Server) restoreIsolatedBrowserLocked(sessionID domain.SessionID) (*browser.Browser, error) {
	if server.isolatedBrowsers == nil {
		server.isolatedBrowsers = make(map[domain.SessionID]*browser.Browser)
	}
	if br := server.isolatedBrowsers[sessionID]; br != nil {
		return br, nil
	}

	owner, err := server.legacyBrowserOwner()
	if err != nil {
		return nil, err
	}
	if owner == sessionID {
		return server.connectBrowserLocked(sessionID, legacyBrowserDataDirectory)
	}
	if br, err := server.connectBrowserLocked(sessionID, server.sessionBrowserDirectory(sessionID)); err != nil || br != nil {
		return br, err
	}
	if owner != "" {
		return nil, nil
	}

	legacy, ok, err := browser.ConnectExisting(legacyBrowserDataDirectory)
	if err != nil || !ok {
		return nil, err
	}
	claimed, err := server.claimLegacyBrowser(sessionID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, nil
	}
	server.isolatedBrowsers[sessionID] = legacy
	return legacy, nil
}

func (server *Server) connectBrowserLocked(sessionID domain.SessionID, dataDirectory string) (*browser.Browser, error) {
	br, ok, err := browser.ConnectExisting(dataDirectory)
	if err != nil || !ok {
		return nil, err
	}
	server.isolatedBrowsers[sessionID] = br
	return br, nil
}

func (server *Server) sessionBrowserDirectory(sessionID domain.SessionID) string {
	return filepath.Join(server.dataDirectory, "browsers", string(sessionID))
}

func (server *Server) legacyBrowserOwnerPath() string {
	return filepath.Join(server.dataDirectory, "browser-legacy-owner")
}

func (server *Server) legacyBrowserOwner() (domain.SessionID, error) {
	data, err := os.ReadFile(server.legacyBrowserOwnerPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取旧浏览器归属失败: %w", err)
	}
	return domain.SessionID(strings.TrimSpace(string(data))), nil
}

func (server *Server) claimLegacyBrowser(sessionID domain.SessionID) (bool, error) {
	file, err := os.OpenFile(server.legacyBrowserOwnerPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		owner, readErr := server.legacyBrowserOwner()
		return owner == sessionID, readErr
	}
	if err != nil {
		return false, fmt.Errorf("记录旧浏览器归属失败: %w", err)
	}
	if _, err := file.WriteString(string(sessionID) + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(server.legacyBrowserOwnerPath())
		return false, fmt.Errorf("记录旧浏览器归属失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("记录旧浏览器归属失败: %w", err)
	}
	return true, nil
}

// browserStream 把隔离浏览器的实时画面作为 MJPEG 流推给前端（侧栏 <img> 直接消费）。
func (server *Server) browserStream(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "no_flusher", "Streaming unsupported")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	targetID := request.URL.Query().Get("targetId")
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	server.browserMu.Lock()
	br, err := server.restoreIsolatedBrowserLocked(sessionID)
	server.browserMu.Unlock()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if br == nil || targetID == "" {
		writeError(writer, http.StatusNotFound, "tab_not_found", "页面不存在")
		return
	}

	writer.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=goseek-frame")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Write([]byte("--goseek-frame\r\n"))
	flusher.Flush()

	frameCh := make(chan []byte, 4)
	stop, err := br.StartViewportStream(targetID, func(jpeg []byte) {
		select {
		case frameCh <- jpeg:
		default: // 丢帧，保持实时
		}
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "screencast_failed", err.Error())
		return
	}
	defer stop()

	ctx := request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-frameCh:
			writer.Write([]byte("Content-Type: image/jpeg\r\n"))
			fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(frame))
			writer.Write(frame)
			writer.Write([]byte("\r\n--goseek-frame\r\n"))
			flusher.Flush()
		}
	}
}

// browserClick 把前端侧栏里的人工点击转发给隔离浏览器（视口坐标）。
func (server *Server) browserClick(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TargetID string  `json:"targetId"`
		X        float64 `json:"x"`
		Y        float64 `json:"y"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.TargetID == "" {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {targetId, x, y}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.ClickAtPage(body.TargetID, body.X, body.Y); err != nil {
		writeError(writer, http.StatusInternalServerError, "click_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "clicked"})
}

// browserScroll 把侧栏滚动转发给隔离浏览器。
func (server *Server) browserScroll(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TargetID string  `json:"targetId"`
		X        float64 `json:"x"`
		Y        float64 `json:"y"`
		DeltaY   float64 `json:"deltaY"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.TargetID == "" {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {targetId, x, y, deltaY}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.ScrollAtPage(body.TargetID, body.X, body.Y, body.DeltaY); err != nil {
		writeError(writer, http.StatusInternalServerError, "scroll_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "scrolled"})
}

func (server *Server) browserViewport(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TargetID string  `json:"targetId"`
		Width    int     `json:"width"`
		Height   int     `json:"height"`
		Scale    float64 `json:"scale"`
		Fit      bool    `json:"fit"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil ||
		body.TargetID == "" || body.Width <= 0 || body.Height <= 0 {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {targetId, width, height}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if body.Scale <= 0 {
		body.Scale = 1
	}
	viewport, err := br.SetViewportScale(
		body.TargetID,
		body.Width,
		body.Height,
		body.Scale,
		body.Fit,
	)
	if err != nil {
		writeError(writer, http.StatusNotFound, "tab_not_found", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, viewport)
}

func (server *Server) browserInsertText(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TargetID string `json:"targetId"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.TargetID == "" {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {targetId, text}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.InsertText(body.TargetID, body.Text); err != nil {
		writeError(writer, http.StatusNotFound, "tab_not_found", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "inserted"})
}

func (server *Server) browserKey(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TargetID string `json:"targetId"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil ||
		body.TargetID == "" || body.Key == "" {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {targetId, key}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.PressKey(body.TargetID, body.Key); err != nil {
		writeError(writer, http.StatusBadRequest, "key_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "pressed"})
}

// browserURL 返回隔离浏览器当前页面的 URL。
func (server *Server) browserURL(writer http.ResponseWriter, request *http.Request) {
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	url := br.CurrentURL()
	writeJSON(writer, http.StatusOK, map[string]string{"url": url})
}

// browserTabs 返回隔离浏览器当前所有打开的页面（tab 列表）。
//
// 这是侧栏每 2 秒轮询一次的只读接口，**绝不能启动浏览器**。内存中没有实例时，
// 只尝试按会话目录重连仍存活的 CDP；没有可恢复实例就返回空列表。
func (server *Server) browserTabs(writer http.ResponseWriter, request *http.Request) {
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	server.browserMu.Lock()
	br, err := server.restoreIsolatedBrowserLocked(sessionID)
	server.browserMu.Unlock()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if br == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"tabs": []browser.PageInfo{}})
		return
	}
	pages := br.ListPages()
	if pages == nil {
		pages = []browser.PageInfo{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tabs": pages})
}

// browserActivateTab 激活某个页面（切 tab）。
func (server *Server) browserActivateTab(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		TargetID string `json:"targetId"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.TargetID == "" {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {targetId}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.ActivatePage(body.TargetID); err != nil {
		writeError(writer, http.StatusNotFound, "tab_not_found", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "activated"})
}

// browserCloseTab 关闭当前会话浏览器中的一个真实页面并返回最新列表。
func (server *Server) browserCloseTab(writer http.ResponseWriter, request *http.Request) {
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	targetID := request.PathValue("targetID")
	if targetID == "" {
		writeError(writer, http.StatusBadRequest, "missing_target", "缺少 targetID")
		return
	}

	server.browserMu.Lock()
	br, err := server.restoreIsolatedBrowserLocked(sessionID)
	server.browserMu.Unlock()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if br == nil {
		writeError(writer, http.StatusNotFound, "tab_not_found", "页面不存在")
		return
	}
	if err := br.ClosePage(targetID); err != nil {
		writeError(writer, http.StatusNotFound, "tab_not_found", err.Error())
		return
	}
	pages := br.ListPages()
	if pages == nil {
		pages = []browser.PageInfo{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tabs": pages})
}

// ensureDatabase 确保数据库存在并且已经迁移到最新版本。
func ensureDatabase(dataDirectory string) error {
	sessions, err := store.New(dataDirectory)
	if err != nil {
		return err
	}
	return sessions.Close()
}

// serveFrontend 服务打包的前端 SPA：静态文件直接返回，其余路径回退到
// index.html 交给前端 Router。favicon 等不存在的资源返回 404。
func (server *Server) serveFrontend(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	// 尝试读静态文件
	data, err := fs.ReadFile(server.frontendFS, "dist"+path)
	if err != nil {
		// SPA 兜底：非资源路径回退 index.html
		if !strings.HasPrefix(path, "/assets/") {
			data, err = fs.ReadFile(server.frontendFS, "dist/index.html")
			if err != nil {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			writer.Write(data)
			return
		}
		http.NotFound(writer, request)
		return
	}
	// Content-Type
	switch {
	case strings.HasSuffix(path, ".html"):
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		writer.Header().Set("Content-Type", "application/javascript")
	case strings.HasSuffix(path, ".css"):
		writer.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(path, ".svg"):
		writer.Header().Set("Content-Type", "image/svg+xml")
	case strings.HasSuffix(path, ".png"):
		writer.Header().Set("Content-Type", "image/png")
	}
	writer.Write(data)
}

// Close 停止所有 Runner 并释放资源。
func (server *Server) Close() {
	server.mutex.Lock()
	server.closed = true
	runners := make([]*Runner, 0, len(server.runners))
	for _, runner := range server.runners {
		runners = append(runners, runner)
	}
	server.runners = nil
	server.mutex.Unlock()

	// 在锁外面停：Stop 会等待正在跑的一轮结束，持锁等待会让别的请求全部卡住。
	for _, runner := range runners {
		runner.Stop()
	}
	if err := server.reader.Close(); err != nil {
		server.logger.Error("关闭只读连接失败", "error", err)
	}
}

// runnerFor 返回指定会话的 Runner，该会话还没有 Runner 时就创建一个。
//
// Runner 只在该会话**第一次需要被写入**时才创建（提交消息或订阅事件流）。
// 只看列表、只读快照不会经过这里，因此不会打开会话、也不会占住它的文件锁。
//
// 为什么中间要解锁：创建 Runner 需要打开数据库、抢文件锁、修复上次中断，都是
// 可能耗时的 I/O，不能持着 server.mutex 做，否则所有会话的请求都会一起卡住。
// 但"创建"与"登记"之间有窗口，两个并发请求可能为同一个会话各建一个 Runner，
// 重新加锁后的双重检查就是处理这个窗口。
func (server *Server) runnerFor(id domain.SessionID) (*Runner, error) {
	server.mutex.Lock()
	//服务关了就解锁，后续也不用进行
	//服务关闭是在另一个goroutine进行，因此为了防止读写竞态
	//基于，未必用户不会同时打开两个窗口同时发消息的情况。
	if server.closed {
		server.mutex.Unlock()
		return nil, errors.New("服务正在关闭")
	}
	//同理，runners为map，防止读取时出现写入
	//使用map，很显然，如果有多个对话，就会对应map中不同key为session_id的键值对
	if existing, ok := server.runners[id]; ok {
		server.mutex.Unlock()
		return existing, nil
	}
	server.mutex.Unlock()

	// 创建要打开数据库、加文件锁、修复上次中断，都是 I/O，不能在锁里做。
	// 走到这里说明该会话还没有 Runner，需要新建并启动它的后台循环。
	//newrunner内部，是可能要进行中断的任务处理，不可能在锁内进行
	// 在这个newrunner处，完成了hub的装配，因此后续调用emit，就是在使用hub的emit方法
	created, err := NewRunner(server.dataDirectory, id, server.newModel, server.defaultModel, server.defaultContextWindow, server.logger)
	if err != nil {
		return nil, err
	}
	//加锁，依旧是防止读写竞态
	server.mutex.Lock()
	defer server.mutex.Unlock()
	// 第一次检查之后，当前请求在锁外创建 Runner；这期间服务可能刚好开始关闭。
	// 若闭着眼睛登记，就会留下一只服务关闭后仍占着会话锁的 Runner，所以这里停掉它。
	if server.closed {
		server.mutex.Unlock()
		created.Stop()
		server.mutex.Lock()
		return nil, errors.New("服务正在关闭")
	}
	// 并发的两个请求可能都为同一个 id 建好了 Runner。同一会话只能有一个写者，
	// 此刻 map 里已经有一个，说明另一个请求抢先登记了；丢掉刚建好的这个，
	// 否则两个 Runner 会同时占着同一把文件锁，历史会交错写坏。
	if existing, ok := server.runners[id]; ok {
		server.mutex.Unlock()
		created.Stop()
		server.mutex.Lock()
		return existing, nil
	}
	// 登记成功。之后同一个会话再来提交或订阅，会直接复用这一只 Runner。
	server.runners[id] = created
	server.logger.Info("打开会话", "session_id", string(id))
	return created, nil
}

// ---- 处理器 ----

// createSessionRequest 是新建会话的请求体。
type createSessionRequest struct {
	// Workspace 是工具执行命令的目录。为空时用服务进程的当前目录。
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
}

// createSession 新建一个会话。
//
// 只写下这个会话，不为它创建 Runner：用户可能只是先建着，真正开始对话时再打开。
func (server *Server) createSession(writer http.ResponseWriter, request *http.Request) {
	var body createSessionRequest
	if request.ContentLength > 0 {
		//转码为createSessionRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
	}
	workspace := body.Workspace
	if workspace == "" {
		//为空默认当前目录
		workspace = defaultWorkspace()
	}
	workspace, err := normalizeWorkspace(workspace)
	if err != nil {
		writeError(writer, http.StatusBadRequest, workspaceErrorCode(err), err.Error())
		return
	}
	if body.Model != "" && !model.Contains(body.Model) {
		writeError(writer, http.StatusBadRequest, "unknown_model", "模型不在支持列表里")
		return
	}

	sessions, err := store.New(server.dataDirectory)
	if err != nil {
		server.writeInternal(writer, "打开数据库失败", err)
		return
	}
	defer sessions.Close()

	session, err := sessions.Create(workspace, body.Model)
	if err != nil {
		server.writeInternal(writer, "创建会话失败", err)
		return
	}
	writeJSON(writer, http.StatusCreated, sessionSummary{
		ID:        string(session.ID),
		Workspace: session.Workspace,
		Model:     session.Model,
		UpdatedAt: session.UpdatedAt,
		Title:     "（还没有对话）",
	})
}

// normalizeWorkspace 校验并归一化新建会话的工作区路径。
func normalizeWorkspace(workspace string) (string, error) {
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("工作区必须是绝对路径：%s", workspace)
	}
	cleaned := filepath.Clean(workspace)
	info, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("工作区不存在：%s", cleaned)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("工作区不是目录：%s", cleaned)
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", fmt.Errorf("解析工作区符号链接失败：%s", cleaned)
	}
	return resolved, nil
}

// workspaceErrorCode 把工作区错误映射成稳定标识。
func workspaceErrorCode(err error) string {
	if strings.Contains(err.Error(), "不存在") {
		return "workspace_not_found"
	}
	return "invalid_workspace"
}

// listSessions 返回全部会话，按最后活动倒序。
func (server *Server) listSessions(writer http.ResponseWriter, request *http.Request) {
	// 列表默认只给未归档的。归档的意思就是"从眼前拿走"，所以要显式索取
	// （?include_archived=1）才会出现。
	includeArchived := request.URL.Query().Get("include_archived") == "1"
	summaries, err := server.reader.List(includeArchived)
	if err != nil {
		server.writeInternal(writer, "读取会话列表失败", err)
		return
	}

	items := make([]sessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		items = append(items, sessionSummary{
			ID:           string(summary.ID),
			Title:        summary.Title,
			MessageCount: summary.MessageCount,
			UpdatedAt:    summary.UpdatedAt,
			Workspace:    summary.Workspace,
			Model:        summary.Model,
			Archived:     summary.Archived,
			CustomTitle:  summary.CustomTitle,
		})
	}
	writeJSON(writer, http.StatusOK, listSessionsResponse{Sessions: items})
}

// runningSessionView 是一个正在运行的会话的线上形态：只带侧栏提醒需要的字段。
type runningSessionView struct {
	ID string `json:"id"`
	// State 是当前轮进行到的步骤（WAITING_MODEL / RUNNING_TOOL / COMPRESSING）。
	State domain.RunState `json:"state"`
}

// listRunningSessions 返回此刻有轮在跑的会话。
//
// 给侧栏回答"哪个会话正在运行"。它只扫 runners 现有成员、绝不创建 Runner：
// 创建一个 Runner 意味着打开数据库、抢会话文件锁、恢复上次中断——一次轮询
// 就该是一次纯内存读取，把那些重活留给用户真正打开会话的那一刻。
func (server *Server) listRunningSessions(writer http.ResponseWriter, request *http.Request) {
	server.mutex.Lock()
	entries := make([]*Runner, 0, len(server.runners))
	for _, runner := range server.runners {
		entries = append(entries, runner)
	}
	// runner 的状态是原子量，锁外读安全；map 的增删才需要 server.mutex。
	server.mutex.Unlock()

	items := make([]runningSessionView, 0, len(entries))
	for _, runner := range entries {
		state := runner.TurnState()
		if state == "" {
			continue // 不在跑的不进列表
		}
		items = append(items, runningSessionView{ID: string(runner.sessionID), State: state})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"running": items})
}

// existingRunner 返回已存在的 Runner（只读，不创建）。会话没有 Runner 时返回 nil。
//
// 快照想知道"这一刻有没有轮在跑"，但它不该为一个附带信息把整个 Runner 建起来。
func (server *Server) existingRunner(id domain.SessionID) *Runner {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.runners[id]
}

// getSession 返回一个会话的快照：元信息加全部消息，以及当前的事件序号。
//
// 前端拿它渲染完整历史，再从 last_sequence 往后订阅实时事件，中间不重不漏。
func (server *Server) getSession(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	session, lastSequence, err := server.reader.Session(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}

	history := session.Messages()
	messages := make([]messageView, 0, len(history))
	for _, message := range history {
		messages = append(messages, newMessageView(message))
	}

	// 顺带算一次上下文占用。
	//
	// 为什么快照要带它：占用是"下一次请求会占多少"，而面板刚打开时还没有下一次
	// 请求，事件流也只从 last_sequence 之后订阅，看不到历史上的 usage 事件。
	// 不带的话，刷新页面之后仪表盘就是空的，直到用户再发一条消息——那看起来像
	// 是功能没了。
	//
	// 这里**现算**而不是去翻最后一条 usage 事件：现算回答的正是仪表盘声称回答的
	// 那个问题（下一次请求会占多少），而且能反映压缩和摘要修订带来的变化；翻旧
	// 事件只能告诉你"上一次请求占了多少"。
	usage, err := server.snapshotUsage(session)
	if err != nil {
		server.writeInternal(writer, "估算上下文占用失败", err)
		return
	}
	modelName := session.Model
	if modelName == "" {
		modelName = server.defaultModel
	}
	entry, _ := model.Lookup(modelName)
	thresholds := contextmgr.NewThresholds(entry.ContextWindow)
	// 快照瞬间的运行状态：会话恰好有 Runner 在跑就如实带上，前端据此显示
	// "思考中/执行中"而不是误报空闲。空闲时是零值，由 omitempty 省略，旧前端不受影响。
	snapshotRunState := domain.RunState("")
	if runner := server.existingRunner(session.ID); runner != nil {
		snapshotRunState = runner.TurnState()
	}
	writeJSON(writer, http.StatusOK, sessionSnapshot{
		ID:           string(session.ID),
		Workspace:    session.Workspace,
		CreatedAt:    session.CreatedAt,
		UpdatedAt:    session.UpdatedAt,
		LastSequence: lastSequence,
		Messages:     messages,
		Memory:       newMemoryView(session.Memory),
		Usage:        domain.NewContextUsagePayload(usage),
		RunState:     snapshotRunState,
		Model:        modelName,
		ModelInfo: modelInfoView{
			Name:                   entry.Name,
			Display:                entry.Display,
			ContextWindow:          entry.ContextWindow,
			EffectiveContextWindow: thresholds.HardLimit,
			CompactionTrigger:      thresholds.CompactAt,
			WindowSource:           entry.WindowSource,
			MeasuredAt:             entry.MeasuredAt,
		},
	})
}

// submitTurnRequest 是 POST /api/v1/sessions/{id}/turns 的请求体。
//
// 这一轮只有一个入口输入：用户要说的原文。程序不做任何意图分析，原样交给 Agent。
type submitTurnRequest struct {
	Content string `json:"content"`
	// ImageIDs 是随消息一起发送的已上传图片 ID 列表。空表示纯文本消息。
	ImageIDs []string `json:"image_ids,omitempty"`
	// SidebarPages 是旧前端发送的页面 URL 列表，保留兼容。
	SidebarPages []string `json:"sidebar_pages,omitempty"`
	// SidebarItems 是发送瞬间侧边栏打开的页面与文件，只进入下一次模型视图。
	SidebarItems []sidebarContextItem `json:"sidebar_items,omitempty"`
}

type sidebarContextItem struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	Path  string `json:"path,omitempty"`
}

// submitTurn 接收一条用户消息并把这一轮交给该会话的 Runner。
//
// 它是 HTTP 层与 Agent 之间的边界：这里只校验输入、找到会话写者、确认提交被
// 接受，不执行任何模型或工具调用。
//
// 正常路径返回 202 而不是 200：这一轮才刚开始，要跑几十秒，过程和结果通过
// 已经建立的 SSE 事件流观察。202 只表示"这一轮已被接受并开始"，不代表最终成功。
//
// 错误路径：
//   - session id 不合法或缺失 -> 400 invalid_session_id / invalid_body
//   - 消息内容为空 -> 400 empty_content
//   - 打开会话失败 -> 404/409，由 writeSessionError 映射
//   - 该会话已有轮在跑 -> 409 turn_in_progress
//   - 其他提交失败 -> 500 internal
func (server *Server) submitTurn(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	// 会话 ID 会参与文件路径定位（锁文件），必须在进入任何存储操作之前校验，
	// 避免 "ses_../../etc/passwd" 这类输入被拼进路径。
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	var body submitTurnRequest
	// 请求体可能缺失或不是合法 JSON；这里解码失败当作参数错误处理，不进入业务。
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	if body.Content == "" {
		writeError(writer, http.StatusBadRequest, "empty_content", "消息内容不能为空")
		return
	}
	// 找到或创建这个会话的 Runner。它是该会话状态的唯一写者，也是后面 Submit 的
	// 投递目标；拿不到就说明会话不存在、正被占用或服务正在关闭。
	runner, err := server.runnerFor(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}
	// 加载图片引用（如果有）。图片必须属于当前会话且尚未发送。
	var images []domain.MessageImage
	if len(body.ImageIDs) > 0 {
		images, err = server.loadImages(id, body.ImageIDs)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_images", err.Error())
			return
		}
	}

	ambientContext := formatSidebarContext(body.SidebarItems, body.SidebarPages)

	// 把用户原文投给 Runner 的主循环。Submit 只确认"这一轮是否被接受"，
	// 不等待模型真正跑完，所以这里可以立刻回复 HTTP。
	submitContext, cancelSubmit := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancelSubmit()
	if err := runner.Submit(submitContext, body.Content, ambientContext, images...); err != nil {
		// "已有轮在跑"是明确的并发语义，单独映射成 409，让前端能按 code 处理。
		if errors.Is(err, ErrTurnInProgress) {
			writeError(writer, http.StatusConflict, "turn_in_progress", err.Error())
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeError(writer, http.StatusServiceUnavailable, "submit_timeout", "提交握手超时，请重试")
			return
		}
		server.writeInternal(writer, "提交失败", err)
		return
	}
	// 202：Accepted。与"一轮完成"解耦，结果由事件流观察。
	writeJSON(writer, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func formatSidebarContext(items []sidebarContextItem, legacyPages []string) string {
	const maxItems = 32
	seen := make(map[string]bool)
	lines := make([]string, 0, min(maxItems, len(items)+len(legacyPages)))
	add := func(kind, title, location string) {
		location = strings.TrimSpace(location)
		if location == "" || len(lines) >= maxItems {
			return
		}
		key := kind + "\x00" + location
		if seen[key] {
			return
		}
		seen[key] = true
		title = strings.TrimSpace(title)
		if len(title) > 160 {
			title = title[:160]
		}
		if len(location) > 4096 {
			location = location[:4096]
		}
		if title != "" && title != location {
			lines = append(lines, fmt.Sprintf("- %s：%s（%s）", kind, title, location))
			return
		}
		lines = append(lines, fmt.Sprintf("- %s：%s", kind, location))
	}
	for _, item := range items {
		switch item.Kind {
		case "page":
			add("页面", item.Title, item.URL)
		case "file":
			add("文件", item.Title, item.Path)
		}
	}
	for _, page := range legacyPages {
		add("页面", "", page)
	}
	if len(lines) == 0 {
		return ""
	}
	return "[临时界面上下文：仅描述用户发送本条消息时侧边栏打开的内容，不属于对话历史，也不要在回复中复述]\n" +
		strings.Join(lines, "\n")
}

// compactSession 按用户的明确要求压缩一次上下文。
//
// 它和 submitTurn 的结构完全一样，因为它们是同一类东西：把一件要跑几十秒的工作
// 投给 Runner，立刻返回 202，过程和结果走事件流。区别只在投的是什么。
//
// 没有请求体——压缩没有参数。范围由轮次边界和保留区决定，让调用方指定等于把
// 覆盖不变量交给它维护。
//
// 一轮在跑时返回 409 `turn_in_progress`：压缩改的是下一次请求要用的视图，和一轮
// 交互并发做，结果无从定义。前端据此提示"等这一轮跑完再压"。
func (server *Server) compactSession(writer http.ResponseWriter, request *http.Request) {
	//会话id
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}
	//runner简而言之就是让一个会话变为可写状态，该runner用于承载agent loop
	runner, err := server.runnerFor(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}
	if err := runner.Compact(); err != nil {
		if errors.Is(err, ErrTurnInProgress) {
			//在跑返回turn_in_progress
			writeError(writer, http.StatusConflict, "turn_in_progress", err.Error())
			return
		}
		server.writeInternal(writer, "压缩失败", err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// getMemory 返回完整的摘要树，供面板逐层展开。
//
// 走只读路径（store.Reader），因此**一轮正在跑时也能查**——它不需要 Runner，
// 也不会排在写路径后面。
//
// 它和快照里那个 `memory` 字段不重复：快照只给活跃前沿且不带正文（每次打开会话
// 都要传，正文可能上千字 × 上百个节点）；这里给全部节点、带正文，只在用户主动
// 展开树时才请求一次。
func (server *Server) getMemory(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	memory, err := server.reader.Memory(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}
	writeJSON(writer, http.StatusOK, newMemoryTree(memory))
}

// editMemoryRequest 是修订一段摘要的请求体。
type editMemoryRequest struct {
	// Content 是修订后的正文。**空串表示撤销修订**，回到模型原始生成的那一版。
	//
	// 因此这里不能像 submitTurn 那样把空内容判成参数错误：空是一个有意义的取值。
	Content string `json:"content"`
}

// editMemory 把一段活跃摘要换成人工修订版。
//
// 这是唯一一个**等到做完才返回**的写操作（其余两个写操作返回 202）。因为它只是
// 一次校验加一次 UPDATE，毫秒级，而调用方需要立刻知道改没改成、为什么没改成。
//
// 它改的**不是** `content` 那一列——那一列存的是模型当初生成了什么，永不修改；
// 改的是 `edited_content`，也就是"现在拿哪一版进上下文"。摘要节点的不可变性
// 因此原样成立（见 domain.MemoryBatch.EditedContent）。
//
// 错误路径：
//   - session id 或 batch id 格式不合法 -> 400
//   - 请求体不是合法 JSON -> 400 invalid_body
//   - 会话不存在 / 被别的进程持有 -> 404 / 409
//   - 该会话已有轮在跑 -> 409 turn_in_progress（它写 session.Memory，不能并发）
//   - 节点不在活跃前沿上 -> 409 batch_not_active（改了也不影响模型看到的内容）
func (server *Server) editMemory(writer http.ResponseWriter, request *http.Request) {
	//session id 校验
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}
	// batch id 同样是外部输入，先校验格式再拿去查——错的格式得到一句明确的说明，
	// 而不是一句"没找到"。
	batchID := domain.MemoryBatchID(request.PathValue("batchID"))
	if err := batchID.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_batch_id", err.Error())
		return
	}

	var body editMemoryRequest
	//json 解码
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	//runner 就是会话中的写者
	runner, err := server.runnerFor(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}
	if err := runner.EditMemory(batchID, body.Content); err != nil {
		switch {
		case errors.Is(err, ErrTurnInProgress):
			writeError(writer, http.StatusConflict, "turn_in_progress", err.Error())
		case errors.Is(err, ErrBatchNotActive):
			writeError(writer, http.StatusConflict, "batch_not_active", err.Error())
		default:
			server.writeInternal(writer, "保存摘要修订失败", err)
		}
		return
	}

	// 把修订之后的整棵树带回去，省掉前端再发一次 GET。修订会改变节点的标题
	// （标题取正文首行），界面上不止那一处要跟着变。
	memory, err := server.reader.Memory(id)
	if err != nil {
		server.writeSessionError(writer, id, err)
		return
	}
	writeJSON(writer, http.StatusOK, newMemoryTree(memory))
}

// snapshotUsage 估算这个会话下一次请求会占多少上下文。
//
// 走的是和真正发请求时**完全同一条路**（contextmgr.Build），因此快照里报的数字和
// 那一刻真正发出去的请求是一致的。两处用不同的方式算，是这类估算最常见的错误来源。
func (server *Server) snapshotUsage(session *domain.Session) (domain.ContextUsage, error) {
	tools, _, err := sessionTools(session, server.dataDirectory)
	if err != nil {
		return domain.ContextUsage{}, err
	}
	window := server.defaultContextWindow
	modelName := session.Model
	if modelName == "" {
		modelName = server.defaultModel
	}
	if entry, found := model.Lookup(modelName); found {
		window = entry.ContextWindow
	}
	view := contextmgr.Build(session, session.Memory, tools.Specs(), window)
	return view.Usage, nil
}

// updateSessionRequest 是归档与重命名的请求体。
//
// 两个字段都是指针：nil 表示"这次不改这一项"。用指针而不是零值判断，是因为
// `false`（取消归档）和 `""`（恢复派生标题）都是**有意义的取值**，用零值当
// "没传"会让这两个操作永远做不到。
type updateSessionRequest struct {
	Archived *bool   `json:"archived,omitempty"`
	Title    *string `json:"title,omitempty"`
	Model    *string `json:"model,omitempty"`
}

// updateSession 归档、取消归档或重命名一个会话。
//
// 它走**写路径的 Store**（新开一个、改完就关），而不是 Runner：归档和重命名只改
// sessions 表上的一列，不碰历史，也不需要会话锁——锁保护的是"同一会话同时只有
// 一个写者"，而改这两列和任何正在跑的一轮都不冲突。
//
// 因此它在会话正被打开着的时候也能用，这是有意的：用户想给一个正在聊的会话
// 改名字，没有理由拦着他。
func (server *Server) updateSession(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	var body updateSessionRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	if body.Archived == nil && body.Title == nil && body.Model == nil {
		writeError(writer, http.StatusBadRequest, "empty_update", "没有要修改的字段")
		return
	}

	sessions, err := store.New(server.dataDirectory)
	if err != nil {
		server.writeInternal(writer, "打开数据库失败", err)
		return
	}
	defer sessions.Close()

	if body.Archived != nil {
		if err := sessions.SetArchived(id, *body.Archived); err != nil {
			server.writeSessionError(writer, id, err)
			return
		}
	}
	if body.Title != nil {
		if err := sessions.SetTitle(id, strings.TrimSpace(*body.Title)); err != nil {
			server.writeSessionError(writer, id, err)
			return
		}
	}
	if body.Model != nil {
		runner, err := server.runnerFor(id)
		if err != nil {
			server.writeSessionError(writer, id, err)
			return
		}
		if err := runner.SetModel(*body.Model); err != nil {
			switch {
			case errors.Is(err, ErrUnknownModel):
				writeError(writer, http.StatusBadRequest, "unknown_model", err.Error())
			case errors.Is(err, ErrTurnInProgress):
				writeError(writer, http.StatusConflict, "turn_in_progress", err.Error())
			case errors.Is(err, ErrUnresolvedToolCalls):
				writeError(writer, http.StatusConflict, "unresolved_tool_calls", err.Error())
			default:
				server.writeInternal(writer, "切换模型失败", err)
			}
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "updated"})
}

// deleteSession 彻底删除一个会话。
//
// # 顺序是正确性的全部
//
//  1. 从 runners 里摘掉它（持锁），后续请求再也拿不到这个 Runner
//  2. runner.Stop()：取消 ctx、等这一轮收尾、关 Hub、释放会话锁
//  3. 删数据库行（消息/事件/摘要靠外键级联）与锁文件
//
// 第 1 步必须在第 2 步之前：否则 Stop 期间进来的请求会拿到一个正在关闭的 Runner。
// 第 2 步必须在第 3 步之前：否则会留下一个对着不存在的会话继续写入的 goroutine。
//
// Stop 会**等**正在跑的那一轮结束，但它先 cancel()，所以命令的进程组会被杀掉，
// 不会真的挂很久；上界是模型请求的超时。
func (server *Server) deleteSession(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	// 第 1 步：摘掉 Runner。没有也没关系——那说明这个会话没被打开过。
	server.mutex.Lock()
	runner, open := server.runners[id]
	if open {
		delete(server.runners, id)
	}
	server.mutex.Unlock()

	// 第 2 步：停掉它。在锁外面停——Stop 会等这一轮收尾，持锁等待会让所有会话
	// 的请求一起卡住。
	if open {
		runner.Stop()
	}

	// 第 3 步：删数据。
	sessions, err := store.New(server.dataDirectory)
	if err != nil {
		server.writeInternal(writer, "打开数据库失败", err)
		return
	}
	defer sessions.Close()
	if err := sessions.Delete(id); err != nil {
		server.writeSessionError(writer, id, err)
		return
	}

	server.logger.Info("会话已删除", "session_id", string(id))
	writeJSON(writer, http.StatusOK, map[string]string{"status": "deleted"})
}

// cancelTurn 中止当前这一轮。
//
// 它**不关闭会话**——只取消这一轮的 context，Runner 继续活着等下一条消息。
// 中止之后走已有的失败路径：turn.failed 落库，已完成的工具调用和观察都保留。
//
// 没有正在跑的一轮时返回 409 `no_turn_running`，而不是假装成功——界面据此说
// "当前没有正在进行的交互"。
func (server *Server) cancelTurn(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}

	// 只找已经打开的 Runner，**不创建**：没打开就等于没有正在跑的一轮，
	// 为了中止而把会话打开（抢锁、修复中断）是本末倒置。
	server.mutex.Lock()
	runner, open := server.runners[id]
	server.mutex.Unlock()
	if !open {
		writeError(writer, http.StatusConflict, "no_turn_running", ErrNoTurnRunning.Error())
		return
	}

	if err := runner.CancelTurn(); err != nil {
		if errors.Is(err, ErrNoTurnRunning) {
			writeError(writer, http.StatusConflict, "no_turn_running", err.Error())
			return
		}
		server.writeInternal(writer, "中止失败", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "cancelled"})
}

// writeSessionError 把打开会话时的错误映射成合适的状态码。
func (server *Server) writeSessionError(writer http.ResponseWriter, id domain.SessionID, err error) {
	switch {
	case errors.Is(err, store.ErrSessionNotFound):
		writeError(writer, http.StatusNotFound, "session_not_found",
			fmt.Sprintf("没有找到会话 %s", id))
	case errors.Is(err, store.ErrSessionBusy):
		writeError(writer, http.StatusConflict, "session_busy",
			"该会话正被另一个进程使用（比如某个终端里的 goseek）")
	case id.Validate() != nil:
		writeError(writer, http.StatusBadRequest, "invalid_session_id", id.Validate().Error())
	default:
		server.writeInternal(writer, "打开会话失败", err)
	}
}

// writeInternal 记录内部错误并返回 500。
//
// 详细原因只进日志，响应里给一句稳定的说明：错误链里可能带着文件路径之类的
// 环境信息，不该发给客户端。
func (server *Server) writeInternal(writer http.ResponseWriter, message string, err error) {
	server.logger.Error(message, "error", err)
	writeError(writer, http.StatusInternalServerError, "internal", message)
}

// withLogging 记录每个请求的方法、路径、状态和耗时。
//
// 只记边界，不记业务过程——过程是事件的职责，日志里重复一遍只会让两处不一致。
// 也不记消息正文和命令内容：日志会被复制粘贴到各种地方，而会话里可能有用户的
// 私有文件内容。
func (server *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}

		next.ServeHTTP(recorder, request)

		server.logger.Info("http",
			"method", request.Method,
			"path", request.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds())
	})
}

// statusRecorder 记下实际写出的状态码。
//
// http.ResponseWriter 不提供读取状态码的方法，只能自己包一层拦下 WriteHeader。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader 记录状态码后转交给下层。
func (recorder *statusRecorder) WriteHeader(status int) {
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

// Flush 透传给下层，SSE 需要它。
//
// 包装类型会挡住底层的 http.Flusher，不显式转发的话 SSE 的每一帧都会卡在缓冲区里，
// 表现为"事件全都堆到连接关闭时才一起到达"。
func (recorder *statusRecorder) Flush() {
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// parseSequence 解析 from 查询参数，缺省或非法时按 0 处理。
//
// 按 0 处理意味着"从头重放"：宁可让客户端多收一遍，也不要因为一个参数写错就
// 漏掉开头的事件。
func parseSequence(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// maxImageUploadSize 是单张图片上传的最大字节数（8MB）。
const maxImageUploadSize = 8 << 20

// maxImageUploadMemory 是 multipart 解析时可驻留内存的上限。文件超过它不会报错，
// 只会落盘临时文件；多出的 1MB 余量覆盖表单头、边界和非文件字段。
const maxImageUploadMemory = maxImageUploadSize + 1<<20

// loadImages 按 ID 列表加载图片引用，校验它们都属于当前会话且未发送。
func (server *Server) loadImages(sessionID domain.SessionID, ids []string) ([]domain.MessageImage, error) {
	images := make([]domain.MessageImage, 0, len(ids))
	for _, imageID := range ids {
		image, _, ownerID, messageSeq, err := server.reader.LoadImage(imageID)
		if err != nil {
			return nil, fmt.Errorf("图片 %s 不存在或已发送", imageID)
		}
		if ownerID != sessionID {
			return nil, fmt.Errorf("图片 %s 不属于这个会话", imageID)
		}
		if messageSeq != 0 {
			return nil, fmt.Errorf("图片 %s 已经绑定到消息", imageID)
		}
		images = append(images, image)
	}
	return images, nil
}

// uploadImage 接收一张图片文件，落盘后登记进数据库。
func (server *Server) uploadImage(writer http.ResponseWriter, request *http.Request) {
	id := domain.SessionID(request.PathValue("id"))
	if err := id.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_session_id", err.Error())
		return
	}
	if err := request.ParseMultipartForm(maxImageUploadMemory); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_upload", "图片上传格式错误或超过大小限制")
		return
	}
	file, header, err := request.FormFile("image")
	if err != nil {
		writeError(writer, http.StatusBadRequest, "missing_file", "缺少 image 文件字段")
		return
	}
	defer file.Close()
	if header.Size > maxImageUploadSize {
		writeError(writer, http.StatusBadRequest, "file_too_large", "图片超过 8MB 上限")
		return
	}
	if header.Size == 0 {
		writeError(writer, http.StatusBadRequest, "empty_file", "图片内容为空，浏览器可能禁止读取拖入的文件")
		return
	}
	mediaType := header.Header.Get("Content-Type")
	if !isAllowedImageType(mediaType) {
		writeError(writer, http.StatusBadRequest, "unsupported_type", "仅支持 PNG / JPEG / WebP")
		return
	}
	imageID := fmt.Sprintf("img_%d_%s", time.Now().UnixNano(), fmt.Sprintf("%08x", randInt64()))
	ext := mediaTypeToExt(mediaType)
	imagesDir := filepath.Join(server.dataDirectory, "images", string(id))
	if err := os.MkdirAll(imagesDir, 0o700); err != nil {
		server.writeInternal(writer, "创建图片目录失败", err)
		return
	}
	filePath := filepath.Join(imagesDir, imageID+ext)
	out, err := os.Create(filePath)
	if err != nil {
		server.writeInternal(writer, "创建图片文件失败", err)
		return
	}
	written, err := io.Copy(out, file)
	out.Close()
	if err != nil {
		os.Remove(filePath)
		server.writeInternal(writer, "写入图片文件失败", err)
		return
	}
	width, height := probeImageSize(filePath, mediaType)
	image := domain.MessageImage{
		ID:        imageID,
		FilePath:  filePath,
		MediaType: mediaType,
		Width:     width,
		Height:    height,
	}
	if err := server.reader.SaveImageRecord(image, id, written); err != nil {
		os.Remove(filePath)
		server.writeInternal(writer, "登记图片失败", err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"id":         imageID,
		"file_path":  filePath,
		"media_type": mediaType,
		"byte_size":  written,
		"width":      width,
		"height":     height,
	})
}

// getImage 按 ID 提供图片文件的本地受控服务。
func (server *Server) getImage(writer http.ResponseWriter, request *http.Request) {
	imageID := request.PathValue("imageID")
	if imageID == "" {
		writeError(writer, http.StatusBadRequest, "invalid_image_id", "图片 ID 不能为空")
		return
	}
	image, _, _, _, err := server.reader.LoadImage(imageID)
	if err != nil {
		writeError(writer, http.StatusNotFound, "image_not_found", "图片不存在")
		return
	}
	file, err := os.Open(image.FilePath)
	if err != nil {
		writeError(writer, http.StatusNotFound, "image_not_found", "图片文件不存在")
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", image.MediaType)
	writer.Header().Set("Cache-Control", "private, max-age=86400")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'")
	http.ServeContent(writer, request, filepath.Base(image.FilePath), time.Time{}, file)
}

// isAllowedImageType 检查上传的媒体类型是否在白名单内。
func isAllowedImageType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp":
		return true
	}
	return false
}

// mediaTypeToExt 把 MIME 类型映射成文件扩展名。
func mediaTypeToExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// probeImageSize 解析 PNG / JPEG / WebP 的头部拿到像素尺寸。
func probeImageSize(filePath string, mediaType string) (int, int) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	switch mediaType {
	case "image/png":
		return probePNGSize(file)
	case "image/jpeg":
		return probeJPEGSize(file)
	case "image/webp":
		return probeWebPSize(file)
	}
	return 0, 0
}

// probePNGSize 解析 PNG 的 IHDR chunk。
func probePNGSize(file *os.File) (int, int) {
	buf := make([]byte, 24)
	if _, err := io.ReadFull(file, buf); err != nil {
		return 0, 0
	}
	if string(buf[:4]) != "\x89PNG" {
		return 0, 0
	}
	width := int(buf[16])<<24 | int(buf[17])<<16 | int(buf[18])<<8 | int(buf[19])
	height := int(buf[20])<<24 | int(buf[21])<<16 | int(buf[22])<<8 | int(buf[23])
	return width, height
}

// probeJPEGSize 扫描 JPEG 的 SOF marker 拿到尺寸。
func probeJPEGSize(file *os.File) (int, int) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(file, buf); err != nil {
		return 0, 0
	}
	if buf[0] != 0xFF || buf[1] != 0xD8 {
		return 0, 0
	}
	for {
		var marker [2]byte
		if _, err := io.ReadFull(file, marker[:]); err != nil {
			return 0, 0
		}
		if marker[0] != 0xFF {
			return 0, 0
		}
		for marker[1] == 0xFF {
			if _, err := io.ReadFull(file, marker[1:]); err != nil {
				return 0, 0
			}
		}
		if marker[1] == 0xD9 || marker[1] == 0x01 || (marker[1] >= 0xD0 && marker[1] <= 0xD7) {
			continue
		}
		var lengthBuf [2]byte
		if _, err := io.ReadFull(file, lengthBuf[:]); err != nil {
			return 0, 0
		}
		length := int(lengthBuf[0])<<8 | int(lengthBuf[1])
		if marker[1] >= 0xC0 && marker[1] <= 0xCF && marker[1] != 0xC4 && marker[1] != 0xC8 && marker[1] != 0xCC {
			var sof [5]byte
			if _, err := io.ReadFull(file, sof[:]); err != nil {
				return 0, 0
			}
			height := int(sof[1])<<8 | int(sof[2])
			width := int(sof[3])<<8 | int(sof[4])
			return width, height
		}
		if _, err := file.Seek(int64(length-2), io.SeekCurrent); err != nil {
			return 0, 0
		}
	}
}

// probeWebPSize 解析 WebP 的 VP8X / VP8 / VP8L 头。
func probeWebPSize(file *os.File) (int, int) {
	buf := make([]byte, 30)
	if _, err := io.ReadFull(file, buf); err != nil {
		return 0, 0
	}
	if string(buf[:4]) != "RIFF" || string(buf[8:12]) != "WEBP" {
		return 0, 0
	}
	format := string(buf[12:16])
	switch format {
	case "VP8 ":
		width := int(buf[26]) | int(buf[27]&0x3F)<<8
		height := int(buf[28]) | int(buf[29]&0x3F)<<8
		return width, height
	case "VP8L":
		bits := uint32(buf[21]) | uint32(buf[22])<<8 | uint32(buf[23])<<16 | uint32(buf[24])<<24
		width := int(bits&0x3FFF) + 1
		height := int(bits>>14&0x3FFF) + 1
		return width, height
	case "VP8X":
		width := 1 + int(buf[24]) | int(buf[25])<<8 | int(buf[26])<<16
		height := 1 + int(buf[27]) | int(buf[28])<<8 | int(buf[29])<<16
		return width, height
	}
	return 0, 0
}

// randInt64 返回一个非负的随机 int64，用于生成图片 ID 的后缀。
func randInt64() int64 {
	return rand.Int63()
}

// skillsDir 返回全局 skill 存储目录。
func (server *Server) skillsDir() string {
	return filepath.Join(server.dataDirectory, "skills")
}

// listSkills GET /api/v1/skills —— 返回所有 skill 的名称和描述。
func (server *Server) listSkills(writer http.ResponseWriter, request *http.Request) {
	type skillInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		// Source 是 skill 的来源：user（用户导入）或插件名。
		Source string `json:"source"`
	}
	entries, err := os.ReadDir(server.skillsDir())
	if err != nil {
		if os.IsNotExist(err) {
			entries = nil
		} else {
			server.writeInternal(writer, "读取 skill 目录失败", err)
			return
		}
	}
	skills := make([]skillInfo, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(server.skillsDir(), entry.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		info := skillInfo{Name: entry.Name()}
		info.Source = "user"
		for _, line := range strings.SplitN(string(data), "\n", 4) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# ") {
				info.Name = strings.TrimPrefix(line, "# ")
			} else if line != "" && !strings.HasPrefix(line, "#") {
				info.Description = line
				break
			}
		}
		skills = append(skills, info)
	}
	// 聚合插件 skill：来源标注为插件名，只读（前端不给删除按钮）。
	if plugins, err := plugin.LoadAll(filepath.Join(server.dataDirectory, "plugins")); err == nil {
		disabled := readDisabledPlugins(server.dataDirectory)
		for _, p := range plugins {
			if disabled[p.Name] {
				continue
			}
			for _, skillName := range p.SkillNames {
				data, err := os.ReadFile(filepath.Join(p.SkillsDir(), skillName, "SKILL.md"))
				if err != nil {
					continue
				}
				info := skillInfo{Name: skillName, Source: p.Name}
				for _, line := range strings.SplitN(string(data), "\n", 4) {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "# ") {
						info.Name = strings.TrimPrefix(line, "# ")
					} else if line != "" && !strings.HasPrefix(line, "#") {
						info.Description = line
						break
					}
				}
				skills = append(skills, info)
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"skills": skills})
}

// getSkill GET /api/v1/skills/{name} —— 返回一个 skill 的完整正文。
func (server *Server) getSkill(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	clean := filepath.Base(filepath.Clean(name))
	if clean == "." || clean == ".." || clean == "/" {
		writeError(writer, http.StatusBadRequest, "invalid_skill_name", "skill 名称无效")
		return
	}
	data, err := os.ReadFile(filepath.Join(server.skillsDir(), clean, "SKILL.md"))
	if err != nil {
		// 用户目录没有：从插件里找（重名时用户 skill 优先）。
		data = nil
		if plugins, loadErr := plugin.LoadAll(filepath.Join(server.dataDirectory, "plugins")); loadErr == nil {
			for _, p := range plugins {
				for _, skillName := range p.SkillNames {
					// SKILL.md 里的 H1 可能与目录名不同；列表里的 name 是 H1。
					// 匹配目录名或 H1。
					path := filepath.Join(p.SkillsDir(), skillName, "SKILL.md")
					content, readErr := os.ReadFile(path)
					if readErr != nil {
						continue
					}
					h1 := strings.TrimPrefix(strings.TrimSpace(strings.SplitN(string(content), "\n", 2)[0]), "# ")
					if skillName == clean || h1 == clean {
						data = content
						break
					}
				}
				if data != nil {
					break
				}
			}
		}
		if data == nil {
			writeError(writer, http.StatusNotFound, "skill_not_found", "skill 不存在")
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"name": clean, "content": string(data)})
}

// uploadSkill POST /api/v1/skills/{name} —— 导入或覆盖一个 skill。
func (server *Server) uploadSkill(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	clean := filepath.Base(filepath.Clean(name))
	if clean == "." || clean == ".." || clean == "/" || strings.ContainsAny(clean, " \t/") {
		writeError(writer, http.StatusBadRequest, "invalid_skill_name", "skill 名称只能包含字母、数字、连字符和下划线")
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON 或超过 1MB")
		return
	}
	dir := filepath.Join(server.skillsDir(), clean)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		server.writeInternal(writer, "创建 skill 目录失败", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body.Content), 0o644); err != nil {
		server.writeInternal(writer, "写入 SKILL.md 失败", err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]string{"name": clean, "status": "imported"})
}

// deleteSkill DELETE /api/v1/skills/{name} —— 删除一个 skill。
func (server *Server) deleteSkill(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	clean := filepath.Base(filepath.Clean(name))
	if clean == "." || clean == ".." || clean == "/" {
		writeError(writer, http.StatusBadRequest, "invalid_skill_name", "skill 名称无效")
		return
	}
	if err := os.RemoveAll(filepath.Join(server.skillsDir(), clean)); err != nil {
		server.writeInternal(writer, "删除 skill 失败", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "deleted"})
}

// pluginsDisabledFile 记录被用户禁用的插件名（一行一个）。
// 不改插件目录本身：启用/禁用是"要不要装"的状态，不是文件操作。
func pluginsDisabledFile(dataDirectory string) string {
	return filepath.Join(dataDirectory, "plugins.disabled")
}

func readDisabledPlugins(dataDirectory string) map[string]bool {
	data, err := os.ReadFile(pluginsDisabledFile(dataDirectory))
	if err != nil {
		return nil
	}
	disabled := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			disabled[name] = true
		}
	}
	return disabled
}

func writeDisabledPlugins(dataDirectory string, disabled map[string]bool) error {
	var lines []string
	for name := range disabled {
		lines = append(lines, name)
	}
	sort.Strings(lines)
	return os.WriteFile(pluginsDisabledFile(dataDirectory), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// pluginView 是插件管理面板用的插件信息。
type pluginView struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Enabled     bool     `json:"enabled"`
	Skills      []string `json:"skills"`
	Commands    []string `json:"commands"`
	HasBin      bool     `json:"has_bin"`
}

// listPlugins GET /api/v1/plugins —— 列出全部插件（含内置解压的）。
func (server *Server) listPlugins(writer http.ResponseWriter, request *http.Request) {
	ensureBuiltinPlugins(server.dataDirectory, filepath.Join(server.dataDirectory, "plugins"))
	disabled := readDisabledPlugins(server.dataDirectory)
	plugins, _ := plugin.LoadAll(filepath.Join(server.dataDirectory, "plugins"))
	views := make([]pluginView, 0)
	for _, p := range plugins {
		view := pluginView{
			Name:        p.Name,
			DisplayName: p.Manifest.DisplayName,
			Version:     p.Manifest.Version,
			Description: p.Manifest.Description,
			Author:      p.Manifest.Author,
			Enabled:     !disabled[p.Name],
			Skills:      p.SkillNames,
			Commands:    p.CommandNames,
			HasBin:      len(listSubdirOrEmpty(p.Dir, "bin")) > 0,
		}
		views = append(views, view)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"plugins": views})
}

func listSubdirOrEmpty(dir, name string) []os.DirEntry {
	entries, err := os.ReadDir(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	return entries
}

// togglePlugin PATCH /api/v1/plugins/{name} —— 启用或禁用。body: {"enabled":bool}
func (server *Server) togglePlugin(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Enabled == nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "需要 {\"enabled\": true|false}")
		return
	}
	disabled := readDisabledPlugins(server.dataDirectory)
	if *body.Enabled {
		delete(disabled, name)
	} else {
		disabled[name] = true
	}
	if err := writeDisabledPlugins(server.dataDirectory, disabled); err != nil {
		server.writeInternal(writer, "写入启用状态失败", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"name": name, "enabled": *body.Enabled})
}

// deletePlugin DELETE /api/v1/plugins/{name} —— 卸载插件（删除目录）。
func (server *Server) deletePlugin(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	if filepath.Base(filepath.Clean(name)) != name || name == "." || name == ".." {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_name", "插件名无效")
		return
	}
	if err := os.RemoveAll(filepath.Join(server.dataDirectory, "plugins", name)); err != nil {
		server.writeInternal(writer, "删除插件失败", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---- MCP 管理 ----

func (server *Server) mcpConfigPath() string {
	return filepath.Join(server.dataDirectory, "mcp.json")
}

// listMCP GET /api/v1/mcp —— 返回当前配置的 MCP server 列表。
func (server *Server) listMCP(writer http.ResponseWriter, request *http.Request) {
	data, err := os.ReadFile(server.mcpConfigPath())
	var configs []mcp.Config
	if err == nil {
		_ = json.Unmarshal(data, &configs)
	}
	if configs == nil {
		configs = []mcp.Config{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"servers": configs})
}

// setMCP POST /api/v1/mcp —— 整体保存 MCP 配置。body: {"servers":[...]}
func (server *Server) setMCP(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Servers []mcp.Config `json:"servers"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	encoded, err := json.MarshalIndent(body.Servers, "", "  ")
	if err != nil {
		server.writeInternal(writer, "编码配置失败", err)
		return
	}
	if err := os.WriteFile(server.mcpConfigPath(), append(encoded, '\n'), 0o600); err != nil {
		server.writeInternal(writer, "写入配置失败", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"servers": body.Servers})
}

// deleteMCP DELETE /api/v1/mcp/{name} —— 删除一个 MCP server 配置项。
func (server *Server) deleteMCP(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	var configs []mcp.Config
	if data, err := os.ReadFile(server.mcpConfigPath()); err == nil {
		_ = json.Unmarshal(data, &configs)
	}
	var kept []mcp.Config
	for _, config := range configs {
		if config.Name != name {
			kept = append(kept, config)
		}
	}
	encoded, _ := json.MarshalIndent(kept, "", "  ")
	if err := os.WriteFile(server.mcpConfigPath(), append(encoded, '\n'), 0o600); err != nil {
		server.writeInternal(writer, "写入配置失败", err)
		return
	}
	if kept == nil {
		kept = []mcp.Config{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"servers": kept})
}

// browserNavigate 在隔离浏览器中打开 URL（新 tab）。
func (server *Server) browserNavigate(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		URL         string `json:"url"`
		WaitSeconds int    `json:"waitSeconds"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.URL == "" {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {url, waitSeconds}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	targetID, err := br.NavigateTarget(body.URL, body.WaitSeconds)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "navigate_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{
		"url":      body.URL,
		"targetId": targetID,
	})
}

// browserScreenshot 截当前页面并返回 jpeg。
func (server *Server) browserScreenshot(writer http.ResponseWriter, request *http.Request) {
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	// 3 秒超时：浏览器未就绪时不阻塞前端轮询。
	type result struct {
		data []byte
		url  string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, url, screenshotErr := br.Screenshot()
		ch <- result{data, url, screenshotErr}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			writeError(writer, http.StatusInternalServerError, "screenshot_failed", r.err.Error())
			return
		}
		writer.Header().Set("Content-Type", "image/jpeg")
		writer.Write(r.data)
	case <-time.After(3 * time.Second):
		// 返回一个 1x1 白色像素 jpeg，前端照常渲染。
		writer.Header().Set("Content-Type", "image/jpeg")
		writer.Write([]byte{
			0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01,
			0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xDB, 0x00, 0x43,
			0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
			0x09, 0x08, 0x0A, 0x0C, 0x14, 0x0D, 0x0C, 0x0B, 0x0B, 0x0C, 0x19, 0x12,
			0x13, 0x0F, 0x14, 0x1D, 0x1A, 0x1F, 0x1E, 0x1D, 0x1A, 0x1C, 0x1C, 0x20,
			0x24, 0x2E, 0x27, 0x20, 0x22, 0x2C, 0x23, 0x1C, 0x1C, 0x28, 0x37, 0x29,
			0x2C, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1F, 0x27, 0x39, 0x3D, 0x38, 0x32,
			0x30, 0x33, 0xFF, 0xD9,
		})
	}
}

// browserReadPage 返回当前页面的可交互元素列表。
func (server *Server) browserReadPage(writer http.ResponseWriter, request *http.Request) {
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	elements, pageInfo, err := br.ReadElements()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"page": pageInfo, "elements": elements})
}

// browserClickElement 按编号点击元素。
func (server *Server) browserClickElement(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Index int `json:"index"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {index}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.ClickElement(body.Index); err != nil {
		writeError(writer, http.StatusInternalServerError, "click_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "clicked"})
}

// browserTypeText 向元素填入文本。
func (server *Server) browserTypeText(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", "参数必须是 {index, text}")
		return
	}
	sessionID := domain.SessionID(request.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(writer, http.StatusBadRequest, "missing_session", "缺少 sessionId 参数")
		return
	}
	br, err := server.getIsolatedBrowser(sessionID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "browser_failed", err.Error())
		return
	}
	if err := br.TypeText(body.Index, body.Text); err != nil {
		writeError(writer, http.StatusInternalServerError, "type_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "typed"})
}

// browserProxy 反向代理：把隔离浏览器当前页面的 HTML/CSS/JS/图片通过后端转发，
// 侧栏 <iframe> 加载代理 URL 就能看到真实渲染的页面（不是截图）。
// Cookie 从隔离浏览器同步，User-Agent 也一致，所以页面行为和真实浏览器一致。
func (server *Server) browserProxy(writer http.ResponseWriter, request *http.Request) {
	sessionID := request.URL.Query().Get("sessionId")
	proxyURL := request.URL.Query().Get("url")
	if proxyURL == "" {
		// 从 Referer 或路径中提取——格式 /api/v1/browser/proxy/https://example.com/page
		rest := strings.TrimPrefix(request.URL.Path, "/api/v1/browser/proxy/")
		if rest != "" {
			proxyURL = rest
		}
	}
	if proxyURL == "" || sessionID == "" {
		http.Error(writer, "missing sessionId or url", 400)
		return
	}
	// 如果路径里编码了完整 URL
	if !strings.HasPrefix(proxyURL, "http") {
		proxyURL = "https://" + proxyURL
	}

	br, err := server.getIsolatedBrowser(domain.SessionID(sessionID))
	if err != nil {
		http.Error(writer, err.Error(), 500)
		return
	}

	// 从隔离浏览器同步 cookies。
	cookies, err := br.Cookies()
	if err == nil {
		for _, c := range cookies {
			http.SetCookie(writer, &http.Cookie{
				Name:   c.Name,
				Value:  c.Value,
				Domain: c.Domain,
				Path:   c.Path,
			})
		}
	}

	// 创建代理请求。
	proxyReq, err := http.NewRequestWithContext(request.Context(), request.Method, proxyURL, request.Body)
	if err != nil {
		http.Error(writer, err.Error(), 500)
		return
	}
	proxyReq.Header.Set("User-Agent", br.UserAgent())
	accept := request.Header.Get("Accept")
	if accept == "" {
		accept = "*/*"
	}
	proxyReq.Header.Set("Accept", accept)
	acceptLang := request.Header.Get("Accept-Language")
	if acceptLang == "" {
		acceptLang = "zh-CN,zh;q=0.9,en;q=0.8"
	}
	proxyReq.Header.Set("Accept-Language", acceptLang)

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不跟随重定向，让 iframe 自己处理
		},
	}
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(writer, err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	// 复制响应头（去掉不能在 iframe 里设置的安全头）。
	for key, values := range resp.Header {
		switch strings.ToLower(key) {
		case "x-frame-options", "content-security-policy", "content-security-policy-report-only":
			continue
		default:
			for _, v := range values {
				writer.Header().Add(key, v)
			}
		}
	}
	writer.WriteHeader(resp.StatusCode)

	// HTML 页面需要重写链接让资源请求也走代理。
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		// 注入 <base> 标签让相对资源走代理。
		proxyBase := fmt.Sprintf("/api/v1/browser/proxy/?sessionId=%s&url=", sessionID)
		html := string(body)

		// 注入 base 标签和拦截脚本。
		inject := fmt.Sprintf(`<base href="%s">
<script>
// 拦截链接点击，改走代理。
document.addEventListener('click', function(e) {
  var a = e.target.closest('a');
  if (a && a.href && !a.href.startsWith('javascript:')) {
    e.preventDefault();
    var proxyURL = '%s' + encodeURIComponent(a.href);
    window.location.href = proxyURL;
  }
}, true);
</script>`, proxyBase+neturl.QueryEscape(proxyURL), proxyBase)

		// 在 <head> 或 <html> 后注入。
		if idx := strings.Index(strings.ToLower(html), "<head>"); idx >= 0 {
			insertPos := idx + len("<head>")
			html = html[:insertPos] + "\n" + inject + html[insertPos:]
		} else if idx := strings.Index(strings.ToLower(html), "<html"); idx >= 0 {
			insertPos := strings.Index(html[idx:], ">") + idx + 1
			html = html[:insertPos] + "\n" + inject + html[insertPos:]
		} else {
			html = inject + html
		}

		writer.Header().Set("Content-Type", contentType)
		writer.Write([]byte(html))
	} else {
		// 非 HTML：直接透传。
		io.Copy(writer, resp.Body)
	}
}
