// Package browser 提供一个与用户浏览器完全隔离的 CDP 浏览器实例。
//
// rod 启动独立的 headless Chromium 进程，使用独立用户数据目录，不共享任何
// 登录态、扩展或窗口状态。元素操作走 CDP（CSS/文本定位），不依赖屏幕坐标。
// 进程通过 CDP URL 文件复用：screenshot 打开页面后，read-page / click 等命令
// 可以连接到同一个 Chrome 实例操作当前页面。
package browser

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	interactionTimeout   = 8 * time.Second
	streamFrameInterval  = 180 * time.Millisecond
	streamCaptureTimeout = 3 * time.Second
	streamJPEGQuality    = 85
	targetCloseTimeout   = 2 * time.Second
	// captureScale 是截图的 deviceScaleFactor：2.0 让 Retina 屏上文字不发虚。
	captureScale = 2.0
)

// Browser 管理一个隔离的浏览器实例。
type Browser struct {
	mu              sync.Mutex
	browser         *rod.Browser
	page            *rod.Page
	currentTargetID string
	dataDir         string

	viewportMu sync.Mutex
	streamMu   sync.Mutex
	streamSeq  uint64
	stopStream func()
}

// ConnectExisting 只连接 dataDir 中仍存活的 CDP 实例，不创建目录或启动 Chromium。
func ConnectExisting(dataDir string) (*Browser, bool, error) {
	dataFile := filepath.Join(dataDir, "cdp_url")
	data, err := os.ReadFile(dataFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 CDP URL 失败: %w", err)
	}
	url := strings.TrimSpace(string(data))
	if url == "" || !browserAlive(url) {
		return nil, false, nil
	}
	b := rod.New().ControlURL(url)
	if err := b.Connect(); err != nil {
		return nil, false, nil
	}
	return &Browser{browser: b, dataDir: dataDir}, true, nil
}

// New 连接 dataDir 中已有的 CDP 实例；没有存活实例时启动新的 Chromium。
func New(dataDir string) (*Browser, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建浏览器数据目录失败: %w", err)
	}

	if existing, ok, err := ConnectExisting(dataDir); err != nil {
		return nil, err
	} else if ok {
		return existing, nil
	}
	dataFile := filepath.Join(dataDir, "cdp_url")
	_ = os.Remove(dataFile)

	// Leakless(false)：Chrome 进程在 goseek-browser 退出后仍然存活，
	// 后续命令才能通过 CDP URL 连接回同一实例。
	browserLauncher := launcher.New().
		UserDataDir(dataDir).
		Headless(true).
		Set("disable-gpu").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-background-networking").
		Set("disable-session-crashed-bubble").
		Leakless(false)
	// 机器已有 Chrome 时显式使用它，避开 Rod 自带浏览器的全局下载锁。
	if bin, found := launcher.LookPath(); found {
		browserLauncher.Bin(bin)
	}
	url, err := browserLauncher.Launch()
	if err != nil {
		return nil, fmt.Errorf("启动隔离浏览器失败: %w", err)
	}
	if err := os.WriteFile(dataFile, []byte(url), 0o600); err != nil {
		return nil, fmt.Errorf("保存 CDP URL 失败: %w", err)
	}

	b := rod.New().ControlURL(url)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("连接浏览器失败: %w", err)
	}

	return &Browser{browser: b, dataDir: dataDir}, nil
}

// browserAlive 检查 CDP URL 的 TCP 端口是否可达。
func browserAlive(cdpURL string) bool {
	host := strings.TrimPrefix(cdpURL, "ws://")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	if idx := strings.Index(host, ":"); idx >= 0 {
		conn, err := net.DialTimeout("tcp", host, 2*1000*1000*1000)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	return false
}

// Page 返回当前活跃页面（懒创建，复用已有页面）。
func (br *Browser) Page() (*rod.Page, error) {
	br.mu.Lock()
	page := br.page
	currentTargetID := br.currentTargetID
	br.mu.Unlock()
	if page != nil {
		if info, err := page.Info(); err == nil && info != nil &&
			(currentTargetID == "" || string(info.TargetID) == currentTargetID) {
			return page, nil
		}
	}
	if currentTargetID != "" {
		if page := br.GetPage(currentTargetID); page != nil {
			br.setCurrentPage(page)
			return page, nil
		}
	}
	// 复用已有页面：read-page / click 等命令复用 screenshot 打开的页面。
	pages := br.ListPages()
	if len(pages) > 0 {
		if page := br.GetPage(pages[len(pages)-1].ID); page != nil {
			br.setCurrentPage(page)
			return page, nil
		}
	}
	page, err := br.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("创建页面失败: %w", err)
	}
	br.setCurrentPage(page)
	return page, nil
}

// Navigate 打开 URL。带 URL 参数的 screenshot 每次开一个新 tab（像浏览器新页面），
// 不复用已有页面；等 waitSeconds 后稳定。
func (br *Browser) Navigate(url string, waitSeconds int) error {
	_, err := br.NavigateTarget(url, waitSeconds)
	return err
}

// NavigateTarget 打开 URL 并返回新页面的 target ID。
func (br *Browser) NavigateTarget(url string, waitSeconds int) (string, error) {
	created, err := (proto.TargetCreateTarget{
		URL:              url,
		BrowserContextID: br.browser.BrowserContextID,
	}).Call(br.browser)
	if err != nil {
		return "", fmt.Errorf("创建页面失败: %w", err)
	}
	br.mu.Lock()
	br.currentTargetID = string(created.TargetID)
	br.page = nil
	br.mu.Unlock()

	if waitSeconds > 0 {
		page, err := br.browser.PageFromTarget(created.TargetID)
		if err != nil {
			_, _ = (proto.TargetCloseTarget{TargetID: created.TargetID}).Call(br.browser)
			return "", fmt.Errorf("连接新页面失败: %w", err)
		}
		br.setCurrentPage(page)
		page.WaitStable(time.Duration(waitSeconds) * time.Second / 2)
	}
	return string(created.TargetID), nil
}

// Screenshot 截取当前页面视口（不是全页），返回 jpeg 字节和 URL。
// 全页截图对长页面会产出巨大的图片，侧栏显示不实用；视口截图对应
// 用户在浏览器里实际看到的区域。
func (br *Browser) Screenshot() ([]byte, string, error) {
	page, err := br.Page()
	if err != nil {
		return nil, "", err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	// 空浏览器实例：返回空帧而不是挂住。
	if info, infoErr := page.Info(); infoErr != nil || info == nil {
		return nil, "about:blank", errors.New("浏览器没有打开页面")
	}
	// 视口截图（全页=false），高 JPEG 质量让 Retina 屏上文字可读。
	quality := streamJPEGQuality
	data, err := page.Screenshot(false, &proto.PageCaptureScreenshot{
		Format:           proto.PageCaptureScreenshotFormatJpeg,
		Quality:          &quality,
		FromSurface:      true,
		OptimizeForSpeed: false,
	})
	if err != nil {
		return nil, "", fmt.Errorf("截图失败: %w", err)
	}
	info, _ := page.Info()
	url := ""
	if info != nil {
		url = info.URL
	}
	return data, url, nil
}

// Element 描述一个可交互元素。
type Element struct {
	Index int    `json:"index"`
	Tag   string `json:"tag"`
	Text  string `json:"text"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// ReadElements 列出当前页面的可交互元素。
func (br *Browser) ReadElements() ([]Element, string, error) {
	page, err := br.Page()
	if err != nil {
		return nil, "", err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	info, _ := page.Info()
	title := ""
	url := ""
	if info != nil {
		title = info.Title
		url = info.URL
	}

	res, err := page.Eval(`() => {
		const els = document.querySelectorAll('a[href], button, input, select, [role=button]');
		const visible = [];
		for (const el of els) {
			if (visible.length >= 120) break;
			const style = getComputedStyle(el);
			// display:none / visibility:hidden / 0x0 的元素对模型毫无意义，
			// 却会把 GitHub 这类页面的编号额度吃光——真正的按钮反而被挤出列表。
			if (style.display === "none" || style.visibility === "hidden" || style.opacity === "0") continue;
			if (el.type === "hidden") continue;
			const r = el.getBoundingClientRect();
			if (r.width < 2 || r.height < 2) continue;
			const text = (el.innerText || el.value || el.placeholder || el.getAttribute('aria-label') || '').trim().slice(0, 60);
			if (!text && el.tagName !== 'INPUT') continue;
			visible.push({index: visible.length, tag: el.tagName.toLowerCase(), text: text,
				x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height)});
		}
		return visible;
	}`)
	if err != nil {
		return nil, "", fmt.Errorf("读取元素失败: %w", err)
	}
	var elements []Element
	if err := res.Value.Unmarshal(&elements); err != nil {
		return nil, "", fmt.Errorf("解析元素失败: %w", err)
	}
	return elements, fmt.Sprintf("%s (%s)", title, url), nil
}

// findElement 按编号查找页面元素。
func findElement(page *rod.Page, index int) (*rod.Element, error) {
	jsFind := `(i) => {
		const els = document.querySelectorAll('a[href], button, input, select, [role=button]');
		return els[i] || null;
	}`
	opts := &rod.EvalOptions{JS: jsFind, JSArgs: []interface{}{index}}
	return page.ElementByJS(opts)
}

// ClickElement 按索引点击元素，先滚到可见位置。
//
// 每步都带 8 秒超时：页面被脚本阻塞时，无超时的 CDP 调用会永久挂起，
// 把整个浏览器实例拖死（侧栏和后续命令全部不可用）。
func (br *Browser) ClickElement(index int) error {
	page, err := br.Page()
	if err != nil {
		return err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	// rod 的 ScrollIntoView / el.Click 内部会等 Hover 稳定 + WaitEnabled，
	// 在 GitHub 这类有大量脚本监听的页面上会一直等到超时。这里直接取元素
	// 中心坐标，走和侧栏用户点击完全相同的 InputDispatchMouseEvent 路径。
	// 与 ReadElements 用同一套可见性过滤——read-page 返回的编号是过滤后
	// 列表的下标，这里必须用同样的规则取元素，否则编号对不上（真实踩过：
	// GitHub 登录页 50 个隐藏 input 把 Sign in 挤出列表，模型永远点不到）。
	result, err := timed.Eval(`(i) => {
		const els = document.querySelectorAll('a[href], button, input, select, [role=button]');
		const visible = [];
		for (const el of els) {
			if (visible.length > i) break;
			const style = getComputedStyle(el);
			if (style.display === "none" || style.visibility === "hidden" || style.opacity === "0") continue;
			if (el.type === "hidden") continue;
			const r = el.getBoundingClientRect();
			if (r.width < 2 || r.height < 2) continue;
			const text = (el.innerText || el.value || el.placeholder || el.getAttribute('aria-label') || '').trim().slice(0, 60);
			if (!text && el.tagName !== 'INPUT') continue;
			visible.push(el);
		}
		const el = visible[i];
		if (!el) return null;
		el.scrollIntoView({block: "center", behavior: "instant"});
		const r = el.getBoundingClientRect();
		return {x: r.x + r.width / 2, y: r.y + r.height / 2};
	}`, index)
	if err != nil {
		return fmt.Errorf("定位元素失败: %w", err)
	}
	var center struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := result.Value.Unmarshal(&center); err != nil || center.X == 0 {
		return fmt.Errorf("元素 %d 没有可点击位置", index)
	}
	if err := br.clickAtLocked(page, center.X, center.Y); err != nil {
		return fmt.Errorf("点击失败: %w", err)
	}
	// Grafana 等页面在点击后（尤其是筛选器切换）需要一小段时间渲染和写回 URL，
	// 固定短等避免后续 read-page/screenshot 拿到的是切换前的状态。
	time.Sleep(2 * time.Second)
	return nil
}

// TypeText 向指定元素填入文本（带 8 秒超时，理由同 ClickElement）。
func (br *Browser) TypeText(index int, text string) error {
	page, err := br.Page()
	if err != nil {
		return err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	el, err := findElement(timed, index)
	if err != nil {
		return fmt.Errorf("找不到元素 %d: %w", index, err)
	}
	if err := el.Input(text); err != nil {
		return fmt.Errorf("输入失败: %w", err)
	}
	return nil
}

// Close 关闭浏览器连接（不杀进程，进程由下次调用复用或自然超时）。
func (br *Browser) Close() {
	br.mu.Lock()
	defer br.mu.Unlock()
	// 不 Close page 或 browser：让 Chrome 进程继续存活供下次复用。
	// 用户数据目录也不清理。
}

// StartViewportStream 连续捕获指定页面当前 viewport 的 jpeg 帧。
// Page.startScreencast 会在 viewport 改变后继续缩放旧画面，因此这里固定使用
// Page.captureScreenshot，确保每一帧都和点击、滚动使用同一套实时坐标。
func (br *Browser) StartViewportStream(
	targetID string,
	onFrame func(jpeg []byte),
) (stop func(), err error) {
	page, err := br.pageForTarget(targetID)
	if err != nil {
		return func() {}, err
	}
	activation := page.Timeout(interactionTimeout)
	if _, err := activation.Activate(); err != nil {
		activation.CancelTimeout()
		return func() {}, fmt.Errorf("激活 screencast 页面失败: %w", err)
	}
	activation.CancelTimeout()

	firstFrame, err := br.captureViewportFrame(page)
	if err != nil {
		return func() {}, fmt.Errorf("捕获页面首帧失败: %w", err)
	}

	br.streamMu.Lock()
	if br.stopStream != nil {
		br.stopStream()
		br.stopStream = nil
	}
	br.streamSeq++
	sequence := br.streamSeq
	streamPage, cancelStream := page.WithCancel()
	stopFrames := make(chan struct{})
	var rawStopOnce sync.Once
	rawStop := func() {
		rawStopOnce.Do(func() {
			close(stopFrames)
			cancelStream()
		})
	}
	br.stopStream = rawStop
	br.streamMu.Unlock()

	onFrame(firstFrame)
	go br.captureViewportFrames(streamPage, stopFrames, onFrame)

	var once sync.Once
	return func() {
		once.Do(func() {
			br.streamMu.Lock()
			defer br.streamMu.Unlock()
			if br.streamSeq != sequence {
				cancelStream()
				return
			}
			rawStop()
			br.stopStream = nil
		})
	}, nil
}

func (br *Browser) captureViewportFrames(
	page *rod.Page,
	stop <-chan struct{},
	onFrame func(jpeg []byte),
) {
	ticker := time.NewTicker(streamFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		frame, err := br.captureViewportFrame(page)
		if err != nil {
			continue
		}
		select {
		case <-stop:
			return
		default:
			onFrame(frame)
		}
	}
}

func (br *Browser) captureViewportFrame(page *rod.Page) ([]byte, error) {
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	timed := page.Timeout(streamCaptureTimeout)
	defer timed.CancelTimeout()
	quality := streamJPEGQuality
	frame, err := proto.PageCaptureScreenshot{
		Format:           proto.PageCaptureScreenshotFormatJpeg,
		Quality:          &quality,
		FromSurface:      true,
		OptimizeForSpeed: true,
	}.Call(timed)
	if err != nil {
		return nil, err
	}
	return frame.Data, nil
}

// PageInfo 描述一个打开的页面（tab）。
type PageInfo struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// ListPages 返回 Chrome 当前报告的所有 page target，不为列表读取附着页面。
func (br *Browser) ListPages() []PageInfo {
	targets, err := (proto.TargetGetTargets{}).Call(br.browser)
	if err != nil {
		return nil
	}
	var result []PageInfo
	for _, info := range targets.TargetInfos {
		if info == nil || info.Type != proto.TargetTargetInfoTypePage {
			continue
		}
		result = append(result, PageInfo{
			ID:    string(info.TargetID),
			URL:   info.URL,
			Title: info.Title,
		})
	}
	return result
}

// GetPage 按 targetID 取一个已打开的页面；找不到返回 nil。
func (br *Browser) GetPage(targetID string) *rod.Page {
	if targetID == "" {
		return nil
	}
	info, err := (proto.TargetGetTargetInfo{
		TargetID: proto.TargetTargetID(targetID),
	}).Call(br.browser)
	if err != nil || info.TargetInfo == nil ||
		info.TargetInfo.Type != proto.TargetTargetInfoTypePage {
		return nil
	}
	page, err := br.browser.PageFromTarget(proto.TargetTargetID(targetID))
	if err != nil {
		return nil
	}
	return page
}

func (br *Browser) pageForTarget(targetID string) (*rod.Page, error) {
	if targetID == "" {
		return br.Page()
	}
	page := br.GetPage(targetID)
	if page == nil {
		return nil, fmt.Errorf("页面 %s 不存在", targetID)
	}
	return page, nil
}

// ClosePage 关闭指定 targetID 的真实浏览器页面。
func (br *Browser) ClosePage(targetID string) error {
	info, err := (proto.TargetGetTargetInfo{
		TargetID: proto.TargetTargetID(targetID),
	}).Call(br.browser)
	if err != nil || info.TargetInfo == nil ||
		info.TargetInfo.Type != proto.TargetTargetInfoTypePage {
		return fmt.Errorf("页面 %s 不存在", targetID)
	}
	if _, err := (proto.TargetCloseTarget{
		TargetID: proto.TargetTargetID(targetID),
	}).Call(br.browser); err != nil {
		return fmt.Errorf("关闭页面 %s 失败: %w", targetID, err)
	}
	deadline := time.Now().Add(targetCloseTimeout)
	for {
		remaining, lookupErr := (proto.TargetGetTargetInfo{
			TargetID: proto.TargetTargetID(targetID),
		}).Call(br.browser)
		if lookupErr != nil || remaining.TargetInfo == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("关闭页面 %s 超时", targetID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	br.mu.Lock()
	if br.currentTargetID == targetID {
		br.currentTargetID = ""
		br.page = nil
	}
	br.mu.Unlock()
	return nil
}

// ActivatePage 把某个页面带到前台（激活 tab）。
func (br *Browser) ActivatePage(targetID string) error {
	page := br.GetPage(targetID)
	if page == nil {
		return fmt.Errorf("页面 %s 不存在", targetID)
	}
	if _, err := page.Activate(); err != nil {
		return err
	}
	br.mu.Lock()
	br.page = page
	br.currentTargetID = targetID
	br.mu.Unlock()
	return nil
}

func (br *Browser) setCurrentPage(page *rod.Page) {
	br.mu.Lock()
	br.page = page
	br.currentTargetID = string(page.TargetID)
	br.mu.Unlock()
}

// CurrentURL 返回当前页面的 URL。
func (br *Browser) CurrentURL() string {
	page, err := br.Page()
	if err != nil {
		return ""
	}
	info, err := page.Info()
	if err != nil || info == nil {
		return ""
	}
	return info.URL
}

// Cookies 返回当前浏览器的所有 cookies（用于反向代理同步会话）。
func (br *Browser) Cookies() ([]*proto.NetworkCookie, error) {
	return br.browser.GetCookies()
}

// UserAgent 返回浏览器的 User-Agent 字符串。
func (br *Browser) UserAgent() string {
	page, err := br.Page()
	if err != nil {
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
	}
	res, err := proto.RuntimeEvaluate{Expression: "navigator.userAgent"}.Call(page)
	if err != nil {
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
	}
	var ua string
	if res.Result != nil {
		_ = res.Result.Value.Unmarshal(&ua)
	}
	if ua == "" {
		return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
	}
	return ua
}

// NavigateCurrent 在当前活跃页面（不新开 tab）上导航到 URL。
func (br *Browser) NavigateCurrent(url string, waitSeconds int) error {
	page, err := br.Page()
	if err != nil {
		return err
	}
	if err := page.Navigate(url); err != nil {
		return fmt.Errorf("导航到 %s 失败: %w", url, err)
	}
	if waitSeconds > 0 {
		page.WaitStable(time.Duration(waitSeconds) * time.Second / 2)
	}
	return nil
}

// ClickAt 在页面视口坐标 (x, y) 处模拟一次人工左键点击（move → down → up）。
func (br *Browser) ClickAt(x, y float64) error {
	return br.ClickAtPage("", x, y)
}

// ClickAtPage 在指定页面的视口坐标处模拟一次人工左键点击。
func (br *Browser) ClickAtPage(targetID string, x, y float64) error {
	page, err := br.pageForTarget(targetID)
	if err != nil {
		return err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	return br.clickAtLocked(page, x, y)
}

// clickAtLocked 发送鼠标事件序列；调用方必须已持有 viewportMu。
// （Go 的 sync.Mutex 不可重入——曾经在这里再次 Lock 造成死锁。）
func (br *Browser) clickAtLocked(page *rod.Page, x, y float64) error {
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	if err := (proto.InputDispatchMouseEvent{
		Type: proto.InputDispatchMouseEventTypeMouseMoved,
		X:    x,
		Y:    y,
	}).Call(timed); err != nil {
		return err
	}
	pressed := 1
	if err := (proto.InputDispatchMouseEvent{
		Type:       proto.InputDispatchMouseEventTypeMousePressed,
		X:          x,
		Y:          y,
		Button:     proto.InputMouseButtonLeft,
		Buttons:    &pressed,
		ClickCount: 1,
	}).Call(timed); err != nil {
		return err
	}
	released := 0
	return (proto.InputDispatchMouseEvent{
		Type:       proto.InputDispatchMouseEventTypeMouseReleased,
		X:          x,
		Y:          y,
		Button:     proto.InputMouseButtonLeft,
		Buttons:    &released,
		ClickCount: 1,
	}).Call(timed)
}

// ScrollAt 在页面视口坐标 (x, y) 处滚动 deltaY（正数向下）。
func (br *Browser) ScrollAt(x, y float64, deltaY float64) error {
	return br.ScrollAtPage("", x, y, deltaY)
}

// ScrollAtPage 在指定页面的视口坐标处滚动 deltaY（正数向下）。
func (br *Browser) ScrollAtPage(targetID string, x, y float64, deltaY float64) error {
	page, err := br.pageForTarget(targetID)
	if err != nil {
		return err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	_, err = timed.Eval(`(x, y, deltaY) => {
		const hit = document.elementFromPoint(x, y);
		if (hit) {
			const allowed = hit.dispatchEvent(new WheelEvent("wheel", {
				bubbles: true,
				cancelable: true,
				clientX: x,
				clientY: y,
				deltaY,
			}));
			if (!allowed) return;
		}
		let node = hit instanceof Element ? hit : null;
		while (node && node !== document.documentElement) {
			const style = getComputedStyle(node);
			const scrollable = /(auto|scroll|overlay)/.test(style.overflowY) &&
				node.scrollHeight > node.clientHeight;
			if (scrollable) {
				node.scrollBy({top: deltaY, behavior: "auto"});
				return;
			}
			node = node.parentElement;
		}
		(document.scrollingElement || document.documentElement).scrollBy({
			top: deltaY,
			behavior: "auto",
		});
	}`, x, y, deltaY)
	return err
}

// ViewportState 描述侧栏画面实际对应的 Chromium viewport。
type ViewportState struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	Fit    bool    `json:"fit"`
}

// SetViewport 把指定页面的真实 CSS viewport 调整到侧栏内容区大小。
func (br *Browser) SetViewport(targetID string, width, height int) error {
	_, err := br.SetViewportScale(targetID, width, height, 1, false)
	return err
}

// SetViewportScale 设置画面 viewport。fit 会在页面存在固定横向溢出时扩大虚拟
// viewport；前端把返回尺寸等比缩进侧栏，并按同一尺寸换算点击坐标。
func (br *Browser) SetViewportScale(
	targetID string,
	width, height int,
	requestedScale float64,
	fit bool,
) (ViewportState, error) {
	page, err := br.pageForTarget(targetID)
	if err != nil {
		return ViewportState{}, err
	}
	br.viewportMu.Lock()
	defer br.viewportMu.Unlock()
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	width = min(max(width, 240), 4096)
	height = min(max(height, 180), 4096)
	apply := func(viewportWidth, viewportHeight int) error {
		screenWidth := viewportWidth
		screenHeight := viewportHeight
		return timed.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
			Width:             viewportWidth,
			Height:            viewportHeight,
			DeviceScaleFactor: 1,
			Mobile:            false,
			ScreenWidth:       &screenWidth,
			ScreenHeight:      &screenHeight,
		})
	}

	minimumScale := max(0.25, float64(width)/2560, float64(height)/2048)
	scale := min(max(requestedScale, minimumScale), 1)
	if fit {
		if err := apply(width, height); err != nil {
			return ViewportState{}, err
		}
		result, evalErr := timed.Eval(`() => Math.max(
			innerWidth,
			document.documentElement?.scrollWidth || 0,
			document.body?.scrollWidth || 0
		)`)
		if evalErr == nil {
			var contentWidth float64
			if result.Value.Unmarshal(&contentWidth) == nil && contentWidth > float64(width)+2 {
				scale = min(max(float64(width)/contentWidth, minimumScale), 1)
			}
		}
	}

	viewportWidth := min(int(math.Round(float64(width)/scale)), 2560)
	viewportHeight := min(int(math.Round(float64(height)/scale)), 2048)
	if err := apply(viewportWidth, viewportHeight); err != nil {
		return ViewportState{}, err
	}
	appliedScale := min(float64(width)/float64(viewportWidth), float64(height)/float64(viewportHeight))
	return ViewportState{
		Width:  viewportWidth,
		Height: viewportHeight,
		Scale:  appliedScale,
		Fit:    fit,
	}, nil
}

// InsertText 向指定页面当前获得焦点的元素插入文字，支持中文与粘贴。
func (br *Browser) InsertText(targetID, text string) error {
	page, err := br.pageForTarget(targetID)
	if err != nil {
		return err
	}
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	return proto.InputInsertText{Text: text}.Call(timed)
}

// PressKey 向指定页面当前获得焦点的元素发送一个常用非文本按键。
func (br *Browser) PressKey(targetID, key string) error {
	page, err := br.pageForTarget(targetID)
	if err != nil {
		return err
	}
	keys := map[string]input.Key{
		"Backspace":  input.Backspace,
		"Delete":     input.Delete,
		"Enter":      input.Enter,
		"Tab":        input.Tab,
		"Escape":     input.Escape,
		"ArrowUp":    input.ArrowUp,
		"ArrowDown":  input.ArrowDown,
		"ArrowLeft":  input.ArrowLeft,
		"ArrowRight": input.ArrowRight,
		"Home":       input.Home,
		"End":        input.End,
		"PageUp":     input.PageUp,
		"PageDown":   input.PageDown,
	}
	inputKey, ok := keys[key]
	if !ok {
		return fmt.Errorf("不支持按键 %s", key)
	}
	timed := page.Timeout(interactionTimeout)
	defer timed.CancelTimeout()
	if err := inputKey.Encode(proto.InputDispatchKeyEventTypeKeyDown, 0).Call(timed); err != nil {
		return err
	}
	return inputKey.Encode(proto.InputDispatchKeyEventTypeKeyUp, 0).Call(timed)
}

// SetCookie 注入一条 cookie（域名限定，供 SSO 会话复用）。
func (br *Browser) SetCookie(domain, name, value string) error {
	page, err := br.Page()
	if err != nil {
		return err
	}
	// 先导航到目标域，cookie 才能绑定到正确的 origin。
	currentInfo, _ := page.Info()
	if currentInfo == nil || !strings.Contains(currentInfo.URL, domain) {
		if err := page.Navigate("https://" + domain); err != nil {
			return fmt.Errorf("导航到 %s 失败: %w", domain, err)
		}
		page.WaitStable(2)
	}
	params := &proto.NetworkCookieParam{
		Name:   name,
		Value:  value,
		Domain: "." + domain,
		Path:   "/",
		Secure: true,
	}
	if err := br.browser.SetCookies([]*proto.NetworkCookieParam{params}); err != nil {
		return fmt.Errorf("设置 cookie %s 失败: %w", name, err)
	}
	return nil
}
