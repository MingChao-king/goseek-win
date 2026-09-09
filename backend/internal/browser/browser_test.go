package browser

import (
	"bytes"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

func TestConnectExistingDoesNotCreateMissingProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "missing-profile")

	br, ok, err := ConnectExisting(profile)
	if err != nil {
		t.Fatalf("ConnectExisting: %v", err)
	}
	if ok || br != nil {
		t.Fatalf("missing profile returned browser=%v ok=%v", br, ok)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile should remain absent, stat error = %v", err)
	}
}

func TestNavigateTargetReturnsCreatedPageID(t *testing.T) {
	br, cleanup := newTestBrowser(t)
	defer cleanup()

	const pageURL = "data:text/html,<title>navigate-target</title>"
	targetID, err := br.NavigateTarget(pageURL, 0)
	if err != nil {
		t.Fatalf("NavigateTarget: %v", err)
	}
	if targetID == "" {
		t.Fatal("NavigateTarget returned an empty target ID")
	}

	info := waitForPageInfo(t, br, targetID, pageURL)
	if info.URL != pageURL {
		t.Fatalf("target URL = %q; want %q", info.URL, pageURL)
	}
}

func TestNavigateTargetReturnsBeforeSlowPageFinishesLoading(t *testing.T) {
	br, cleanup := newTestBrowser(t)
	defer cleanup()

	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		time.Sleep(2 * time.Second)
		_, _ = writer.Write([]byte("<title>slow target</title>"))
	}))
	defer server.Close()

	start := time.Now()
	targetID, err := br.NavigateTarget(server.URL, 0)
	if err != nil {
		t.Fatalf("NavigateTarget: %v", err)
	}
	if targetID == "" {
		t.Fatal("NavigateTarget returned an empty target ID")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("NavigateTarget waited %s for a slow page; want immediate target creation", elapsed)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("created target never started navigation")
	}
}

func TestClosePageRemovesOnlyRequestedTarget(t *testing.T) {
	profile, err := os.MkdirTemp("", "goseek-close-page-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	br, err := New(profile)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = br.browser.Close()
		time.Sleep(500 * time.Millisecond)
		_ = os.RemoveAll(profile)
	})

	const firstURL = "data:text/html,<title>first</title>"
	const secondURL = "data:text/html,<title>second</title>"
	if err := br.Navigate(firstURL, 0); err != nil {
		t.Fatalf("navigate first: %v", err)
	}
	if err := br.Navigate(secondURL, 0); err != nil {
		t.Fatalf("navigate second: %v", err)
	}

	var firstID, secondID string
	deadline := time.Now().Add(5 * time.Second)
	for firstID == "" || secondID == "" {
		for _, page := range br.ListPages() {
			switch page.URL {
			case firstURL:
				firstID = page.ID
			case secondURL:
				secondID = page.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected both test pages, got %#v", br.ListPages())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := br.ClosePage(firstID); err != nil {
		t.Fatalf("ClosePage(first): %v", err)
	}
	for _, page := range br.ListPages() {
		if page.ID == firstID {
			t.Fatalf("closed target still listed: %#v", page)
		}
	}
	if br.GetPage(secondID) == nil {
		t.Fatal("closing first target also removed second target")
	}
	if err := br.ClosePage("missing-target"); err == nil {
		t.Fatal("closing missing target succeeded")
	}
}

func TestTargetedBrowserInteractionAndResponsiveViewport(t *testing.T) {
	br, cleanup := newTestBrowser(t)
	defer cleanup()

	const firstURL = `data:text/html,<meta name="viewport" content="width=device-width"><style>body{margin:0;height:1600px}input,button{margin:20px;width:180px;height:44px}</style><input aria-label="name"><button onclick="document.body.dataset.clicked='yes'">click</button>`
	const secondURL = `data:text/html,<title>second</title><body data-clicked="no">second</body>`
	if err := br.Navigate(firstURL, 0); err != nil {
		t.Fatalf("navigate first: %v", err)
	}
	if err := br.Navigate(secondURL, 0); err != nil {
		t.Fatalf("navigate second: %v", err)
	}
	firstID := waitForTarget(t, br, firstURL)
	secondID := waitForTarget(t, br, secondURL)

	if err := br.SetViewport(firstID, 480, 360); err != nil {
		t.Fatalf("SetViewport: %v", err)
	}
	firstPage := br.GetPage(firstID)
	var viewport struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	result, err := firstPage.Eval(`() => ({width: innerWidth, height: innerHeight})`)
	if err != nil {
		t.Fatalf("read viewport: %v", err)
	}
	if err := result.Value.Unmarshal(&viewport); err != nil {
		t.Fatalf("decode viewport: %v", err)
	}
	if viewport.Width != 480 || viewport.Height != 360 {
		t.Fatalf("viewport = %dx%d; want 480x360", viewport.Width, viewport.Height)
	}

	inputCenter := elementCenter(t, firstPage, "input")
	if err := br.ClickAtPage(firstID, inputCenter.x, inputCenter.y); err != nil {
		t.Fatalf("click input: %v", err)
	}
	if err := br.InsertText(firstID, "hello"); err != nil {
		t.Fatalf("insert text: %v", err)
	}
	if err := br.PressKey(firstID, "Backspace"); err != nil {
		t.Fatalf("press key: %v", err)
	}
	var value string
	result, err = firstPage.Eval(`() => document.querySelector('input').value`)
	if err != nil {
		t.Fatalf("read input: %v", err)
	}
	if err := result.Value.Unmarshal(&value); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	if value != "hell" {
		t.Fatalf("input value = %q; want hell", value)
	}

	buttonCenter := elementCenter(t, firstPage, "button")
	if err := br.ClickAtPage(firstID, buttonCenter.x, buttonCenter.y); err != nil {
		t.Fatalf("click button: %v", err)
	}
	var clicked string
	result, err = firstPage.Eval(`() => document.body.dataset.clicked || ''`)
	if err != nil {
		t.Fatalf("read click state: %v", err)
	}
	if err := result.Value.Unmarshal(&clicked); err != nil {
		t.Fatalf("decode click state: %v", err)
	}
	if clicked != "yes" {
		t.Fatalf("requested target did not receive click: %q", clicked)
	}
	secondPage := br.GetPage(secondID)
	result, err = secondPage.Eval(`() => document.body.dataset.clicked`)
	if err != nil {
		t.Fatalf("read second target: %v", err)
	}
	if err := result.Value.Unmarshal(&clicked); err != nil {
		t.Fatalf("decode second target: %v", err)
	}
	if clicked != "no" {
		t.Fatalf("click leaked into another target: %q", clicked)
	}

	if err := br.ScrollAtPage(firstID, 200, 200, 500); err != nil {
		t.Fatalf("scroll: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var scrollY float64
		result, err = firstPage.Eval(`() => scrollY`)
		if err == nil {
			_ = result.Value.Unmarshal(&scrollY)
		}
		if scrollY > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("requested target did not scroll")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := br.ActivatePage(firstID); err != nil {
		t.Fatalf("ActivatePage: %v", err)
	}
	if got := br.CurrentURL(); got != firstURL {
		t.Fatalf("current URL = %q; want first target", got)
	}
}

func TestViewportStreamStreamsRealPageFrames(t *testing.T) {
	br, cleanup := newTestBrowser(t)
	defer cleanup()

	const pageURL = `data:text/html,<body style="background:rgb(20,120,220)">live</body>`
	if err := br.Navigate(pageURL, 0); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	targetID := waitForTarget(t, br, pageURL)
	frames := make(chan []byte, 1)
	stop, err := br.StartViewportStream(targetID, func(frame []byte) {
		select {
		case frames <- append([]byte(nil), frame...):
		default:
		}
	})
	if err != nil {
		t.Fatalf("StartScreencast: %v", err)
	}
	defer stop()

	select {
	case frame := <-frames:
		if len(frame) < 4 || !strings.HasPrefix(string(frame[:3]), "\xff\xd8\xff") {
			t.Fatalf("frame is not jpeg: %x", frame[:min(len(frame), 8)])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("screencast did not produce a frame")
	}
}

func TestViewportStreamTracksViewportResizeWithoutCropping(t *testing.T) {
	br, cleanup := newTestBrowser(t)
	defer cleanup()

	const pageURL = `data:text/html,<meta name="viewport" content="width=device-width"><style>html,body{margin:0;width:1200px;height:360px;background:white}.marker{position:absolute;left:1080px;top:80px;width:100px;height:80px;background:rgb(220,20,60)}</style><div class="marker"></div>`
	targetID, err := br.NavigateTarget(pageURL, 0)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	waitForElement(t, br.GetPage(targetID), ".marker")
	if err := br.SetViewport(targetID, 480, 360); err != nil {
		t.Fatalf("set initial viewport: %v", err)
	}

	frames := make(chan []byte, 8)
	stop, err := br.StartViewportStream(targetID, func(frame []byte) {
		select {
		case frames <- append([]byte(nil), frame...):
		default:
		}
	})
	if err != nil {
		t.Fatalf("StartViewportStream: %v", err)
	}
	defer stop()

	select {
	case frame := <-frames:
		image, err := jpeg.Decode(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("decode initial frame: %v", err)
		}
		if image.Bounds().Dx() != 480 {
			t.Fatalf("initial frame width = %d; want 480", image.Bounds().Dx())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("viewport stream did not produce its initial frame")
	}

	if err := br.SetViewport(targetID, 1200, 360); err != nil {
		t.Fatalf("resize viewport: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-frames:
			image, err := jpeg.Decode(bytes.NewReader(frame))
			if err != nil || image.Bounds().Dx() != 1200 {
				continue
			}
			red, green, blue, _ := image.At(1120, 110).RGBA()
			if red > 45000 && green < 15000 && blue < 25000 {
				return
			}
		case <-deadline:
			t.Fatal("resized stream never showed the far-right marker")
		}
	}
}

func TestFitViewportKeepsFixedWidthPageClickable(t *testing.T) {
	br, cleanup := newTestBrowser(t)
	defer cleanup()

	const pageURL = `data:text/html,<meta name="viewport" content="width=device-width"><style>html,body{margin:0;min-width:1200px;height:1400px}button{position:absolute;left:1080px;top:80px;width:100px;height:50px}</style><button onclick="document.body.dataset.clicked='yes'">far</button>`
	if err := br.Navigate(pageURL, 0); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	targetID := waitForTarget(t, br, pageURL)
	viewport, err := br.SetViewportScale(targetID, 480, 360, 1, true)
	if err != nil {
		t.Fatalf("SetViewportScale: %v", err)
	}
	if viewport.Width < 1190 {
		t.Fatalf("fit viewport width = %d; want fixed page width", viewport.Width)
	}
	if viewport.Scale >= 0.5 {
		t.Fatalf("fit scale = %.3f; want page scaled into narrow surface", viewport.Scale)
	}

	page := br.GetPage(targetID)
	center := elementCenter(t, page, "button")
	if err := br.ClickAtPage(targetID, center.x, center.y); err != nil {
		t.Fatalf("click scaled page button: %v", err)
	}
	result, err := page.Eval(`() => document.body.dataset.clicked || ''`)
	if err != nil {
		t.Fatalf("read click state: %v", err)
	}
	var clicked string
	if err := result.Value.Unmarshal(&clicked); err != nil {
		t.Fatalf("decode click state: %v", err)
	}
	if clicked != "yes" {
		t.Fatalf("scaled page click missed target: %q", clicked)
	}
}

type point struct {
	x float64
	y float64
}

func elementCenter(t *testing.T, page *rod.Page, selector string) point {
	t.Helper()
	result, err := page.Eval(`(selector) => {
		const rect = document.querySelector(selector).getBoundingClientRect();
		return {x: rect.left + rect.width / 2, y: rect.top + rect.height / 2};
	}`, selector)
	if err != nil {
		t.Fatalf("element center %s: %v", selector, err)
	}
	var center struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := result.Value.Unmarshal(&center); err != nil {
		t.Fatalf("decode center %s: %v", selector, err)
	}
	return point{x: center.X, y: center.Y}
}

func waitForTarget(t *testing.T, br *Browser, url string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, page := range br.ListPages() {
			if page.URL == url {
				return page.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("target for %q not found; pages = %#v", url, br.ListPages())
	return ""
}

func waitForPageInfo(t *testing.T, br *Browser, targetID, expectedURL string) PageInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, page := range br.ListPages() {
			if page.ID == targetID && page.URL == expectedURL {
				return page
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("target %q did not finish navigation; pages = %#v", targetID, br.ListPages())
	return PageInfo{}
}

func waitForElement(t *testing.T, page *rod.Page, selector string) {
	t.Helper()
	if page == nil {
		t.Fatalf("page is nil while waiting for %s", selector)
	}
	timed := page.Timeout(5 * time.Second)
	defer timed.CancelTimeout()
	if _, err := timed.Element(selector); err != nil {
		t.Fatalf("wait for element %s: %v", selector, err)
	}
}

func newTestBrowser(t *testing.T) (*Browser, func()) {
	t.Helper()
	profile, err := os.MkdirTemp("", "goseek-browser-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	br, err := New(profile)
	if err != nil {
		_ = os.RemoveAll(profile)
		t.Fatalf("New: %v", err)
	}
	return br, func() {
		_ = br.browser.Close()
		time.Sleep(300 * time.Millisecond)
		_ = os.RemoveAll(profile)
	}
}
