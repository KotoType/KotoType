package ui

import (
	"bytes"
	"fmt"
	"image/png"
	"math"
	"runtime"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KotoType/KotoType/assets"
	"github.com/KotoType/KotoType/internal/winapi"
)

// Standalone Windows 11 Fluent "About KotoType" window with Mica backdrop,
// embedded PNG logo, clickable version/commit/author info card and GitHub link.

const (
	aboutStyle = wsCaption | wsSysMenu
	aboutW     = 420
	aboutH     = 358

	aboutHitNone = iota
	aboutHitCommit
	aboutHitAuthor
	aboutHitGitHub
	aboutHitCopy

	aboutCopyTimer = 1

	authorName = "dark495"
	authorURL  = "https://github.com/xlch88"
)

var (
	openClipboard  = user32.NewProc("OpenClipboard")
	closeClipboard = user32.NewProc("CloseClipboard")
	emptyClipboard = user32.NewProc("EmptyClipboard")
	setClipboard   = user32.NewProc("SetClipboardData")
	globalAlloc    = kernel32.NewProc("GlobalAlloc")
	globalLock     = kernel32.NewProc("GlobalLock")
	globalUnlock   = kernel32.NewProc("GlobalUnlock")
	rtlMoveMemory  = kernel32.NewProc("RtlMoveMemory")

	aboutWnd     uintptr
	aboutDPI     int
	aboutMica    bool
	aboutCanvas  *canvas
	aboutLogoBmp *winapi.Object
	aboutLogoW   int
	aboutLogoH   int
	aboutLogoRaw []byte // Premultiplied BGRA decoded once from assets.IconPNG.
	aboutHover   int
	aboutPressed int
	aboutCopied  bool

	aboutCardRect   = rectF{24, 178, aboutW - 24, 286}
	aboutCommitRect = rectF{140, 214, aboutW - 32, 250}
	aboutAuthorRect = rectF{140, 250, aboutW - 32, 286}
	aboutGitHubRect = rectF{24, 302, 204, 336}
	aboutCopyRect   = rectF{216, 302, 396, 336}
)

func loadAboutLogoPixels() {
	if len(aboutLogoRaw) > 0 {
		return
	}
	img, err := png.Decode(bytes.NewReader(assets.IconPNG))
	if err != nil {
		return
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	buf := make([]byte, w*h*4)
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			// RGBA() already returns alpha-premultiplied 16-bit channels.
			r, g, bl, a := img.At(x, y).RGBA()
			buf[i] = byte(bl >> 8)
			buf[i+1] = byte(g >> 8)
			buf[i+2] = byte(r >> 8)
			buf[i+3] = byte(a >> 8)
			i += 4
		}
	}
	aboutLogoW, aboutLogoH, aboutLogoRaw = w, h, buf
}

// parseVersion splits a git-describe string like "v0.1.1-24-g9f86668" into
// display version ("v0.1.1") and short commit hash ("9f86668").
func parseVersion(v string) (ver, commit string) {
	v = strings.TrimSpace(v)
	if idx := strings.LastIndex(v, "-g"); idx > 0 {
		commit = strings.TrimSuffix(v[idx+2:], "-dirty")
		prefix := v[:idx]
		if dash := strings.LastIndex(prefix, "-"); dash > 0 {
			prefix = prefix[:dash]
		}
		return prefix, commit
	}
	return v, ""
}

func openURL(u string) {
	shellExecute.Call(0, uintptr(unsafe.Pointer(str("open"))), uintptr(unsafe.Pointer(str(u))), 0, 0, 1) // SW_SHOWNORMAL
}

func releaseAboutSurface() {
	aboutLogoBmp.Release()
	aboutLogoBmp = nil
	aboutCanvas.release()
}

func createAbout() {
	aboutWnd, _, _ = createWindow.Call(0, uintptr(unsafe.Pointer(str("KotoTypeAbout"))), uintptr(unsafe.Pointer(str("关于 KotoType"))), aboutStyle,
		0x80000000, 0x80000000, 0, 0, 0, 0, instance, 0)
	dpi, _, _ := getDpiForWindow.Call(aboutWnd)
	aboutDPI = max(int(dpi), 96)
	aboutCanvas = newCanvas(aboutWnd, aboutDPI)
	aboutMica = styleFrame(aboutWnd, true)
	sizeAboutWindow()
}

func sizeAboutWindow() {
	frame := windows.Rect{
		Right:  int32(math.Round(float64(aboutW) * float64(aboutDPI) / 96)),
		Bottom: int32(math.Round(float64(aboutH) * float64(aboutDPI) / 96)),
	}
	adjustWindowRect.Call(uintptr(unsafe.Pointer(&frame)), aboutStyle, 0, 0, uintptr(aboutDPI))
	setWindowPos.Call(aboutWnd, 0, 0, 0, uintptr(frame.Right-frame.Left), uintptr(frame.Bottom-frame.Top), 0x2|0x4|0x10) // NOMOVE | NOZORDER | NOACTIVATE
}

func showAbout() {
	aboutMica = styleFrame(aboutWnd, true)
	if !visible(aboutWnd) {
		anchorWnd := settingsWnd
		if !visible(anchorWnd) {
			anchorWnd, _, _ = getForeground.Call()
		}
		monitor, _, _ := monitorFromWindow.Call(anchorWnd, 2) // MONITOR_DEFAULTTONEAREST
		var dpi, dpiY uint32
		getDpiForMonitor.Call(monitor, 0, uintptr(unsafe.Pointer(&dpi)), uintptr(unsafe.Pointer(&dpiY)))
		if newDPI := max(int(dpi), 96); newDPI != aboutDPI {
			aboutDPI = newDPI
			aboutCanvas.setDPI(aboutDPI)
			sizeAboutWindow()
		}
		var ar windows.Rect
		getWindowRect.Call(aboutWnd, uintptr(unsafe.Pointer(&ar)))
		w, h := ar.Right-ar.Left, ar.Bottom-ar.Top
		work := workArea(monitor)
		cx, cy := (work.Left+work.Right)/2, (work.Top+work.Bottom)/2
		if visible(settingsWnd) {
			var sr windows.Rect
			getWindowRect.Call(settingsWnd, uintptr(unsafe.Pointer(&sr)))
			cx, cy = (sr.Left+sr.Right)/2, (sr.Top+sr.Bottom)/2
		}
		x := max(work.Left, min(cx-w/2, work.Right-w))
		y := max(work.Top, min(cy-h/2, work.Bottom-h))
		setWindowPos.Call(aboutWnd, 0, uintptr(x), uintptr(y), 0, 0, 0x1|0x4) // NOSIZE | NOZORDER
	}
	showWindow.Call(aboutWnd, 1) // SW_SHOWNORMAL
	setForeground.Call(aboutWnd)
	invalidateRect.Call(aboutWnd, 0, 0)
}

func copyTextToClipboard(text string) {
	if r, _, _ := openClipboard.Call(aboutWnd); r == 0 {
		return
	}
	defer closeClipboard.Call()
	emptyClipboard.Call()
	chars := utf16.Encode([]rune(text))
	chars = append(chars, 0)
	size := uintptr(len(chars) * 2)
	mem, _, _ := globalAlloc.Call(0x0002, size) // GMEM_MOVEABLE
	if mem == 0 {
		return
	}
	ptr, _, _ := globalLock.Call(mem)
	if ptr != 0 {
		rtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&chars[0])), size)
		globalUnlock.Call(mem)
		setClipboard.Call(13, mem) // CF_UNICODETEXT
	}
}

func aboutHit(xPx, yPx int32) int {
	x, y := aboutCanvas.pixels(xPx), aboutCanvas.pixels(yPx)
	_, commit := parseVersion(opts.Version)
	switch {
	case commit != "" && aboutCommitRect.contains(x, y):
		return aboutHitCommit
	case aboutAuthorRect.contains(x, y):
		return aboutHitAuthor
	case aboutGitHubRect.contains(x, y):
		return aboutHitGitHub
	case aboutCopyRect.contains(x, y):
		return aboutHitCopy
	}
	return aboutHitNone
}

func paintAbout() {
	c := aboutCanvas
	if !c.begin() {
		return
	}
	if aboutMica {
		c.clear(rgba{})
	} else {
		c.clear(th.window)
	}

	// Render the embedded PNG logo directly with Direct2D.
	if aboutLogoBmp == nil {
		loadAboutLogoPixels()
		aboutLogoBmp = c.createBitmap(aboutLogoW, aboutLogoH, aboutLogoRaw)
	}
	logoRect := rectF{aboutW/2 - 38, 22, aboutW/2 + 38, 98}
	if aboutLogoBmp != nil {
		c.drawBitmap(aboutLogoBmp, logoRect)
	}

	c.text("KotoType", rectF{24, 108, aboutW - 24, 136}, textFont{size: 20, bold: true, align: 2, middle: true}, th.text)
	c.text("轻量级 Windows 语音听写工具", rectF{24, 140, aboutW - 24, 162}, textFont{size: 12.5, align: 2, middle: true}, th.subtle)

	// Three-row info card: Version, Commit (link), Author (link).
	card := aboutCardRect
	c.fill(card, 8, th.card)
	c.stroke(card.inset(0.5), 7.5, 1, th.border)

	_, commit := parseVersion(opts.Version)
	verFull := fmt.Sprintf("%s (%s/%s)", opts.Version, runtime.GOOS, runtime.GOARCH)
	commitDisp := commit
	if commitDisp == "" {
		commitDisp = "—"
	}
	infoRows := []struct {
		k, v string
		hit  int
		link bool
	}{
		{"版本号", verFull, aboutHitNone, false},
		{"Commit", commitDisp, aboutHitCommit, commit != ""},
		{"作者", authorName, aboutHitAuthor, true},
	}
	for i, row := range infoRows {
		ry := card.T + float32(i)*36
		if i > 0 {
			c.line(card.L+14, ry+0.5, card.R-14, ry+0.5, 1, th.divider)
		}
		c.text(row.k, rectF{card.L + 14, ry, card.L + 78, ry + 36}, textFont{size: 13, middle: true}, th.subtle)
		if !row.link {
			c.text(row.v, rectF{card.L + 82, ry, card.R - 14, ry + 36}, textFont{size: 12.5, bold: true, align: 1, middle: true}, th.text)
			continue
		}
		f := textFont{size: 13, align: 1, middle: true}
		vw, _ := measureText(row.v, f, 200)
		pillW := float32(math.Ceil(float64(vw))) + 28
		pill := rectF{card.R - 10 - pillW, ry + 5, card.R - 10, ry + 31}
		if aboutHover == row.hit {
			bg := th.itemHover
			if aboutPressed == row.hit {
				bg = th.itemPressed
			}
			c.fill(pill, 4, bg)
		}
		c.text(row.v, rectF{pill.L + 4, ry, pill.R - 20, ry + 36}, f, th.accent)
		c.text("\ue8a7", rectF{pill.R - 18, ry, pill.R - 4, ry + 36}, textFont{size: 11, icon: true, align: 2, middle: true}, th.accent)
	}

	// Bottom action buttons.
	drawButton(c, aboutGitHubRect, "访问 GitHub 项目页", "\ue774", true, true, aboutHover == aboutHitGitHub, aboutPressed == aboutHitGitHub)
	copyLbl, copyIcon := "复制版本信息", "\ue8c8"
	if aboutCopied {
		copyLbl, copyIcon = "已复制", "\ue73e"
	}
	drawButton(c, aboutCopyRect, copyLbl, copyIcon, false, true, aboutHover == aboutHitCopy, aboutPressed == aboutHitCopy)

	c.end()
}

func aboutProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmDPIChanged:
		aboutDPI = max(int(hiword(wParam)), 96)
		aboutCanvas.setDPI(aboutDPI)
		sizeAboutWindow()
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmMouseMove:
		if hit := aboutHit(loword(lParam), hiword(lParam)); hit != aboutHover {
			aboutHover = hit
			invalidateRect.Call(hwnd, 0, 0)
			trackMouseLeave(hwnd)
		}
		return 0
	case wmMouseLeave:
		if aboutHover != aboutHitNone || aboutPressed != aboutHitNone {
			aboutHover = aboutHitNone
			aboutPressed = aboutHitNone
			invalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmLButtonDown:
		aboutPressed = aboutHit(loword(lParam), hiword(lParam))
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmLButtonUp:
		hit := aboutHit(loword(lParam), hiword(lParam))
		pressed := aboutPressed
		aboutPressed = aboutHitNone
		invalidateRect.Call(hwnd, 0, 0)
		if hit != aboutHitNone && hit == pressed {
			switch hit {
			case aboutHitCommit:
				if _, commit := parseVersion(opts.Version); commit != "" {
					openURL(aboutURL + "/commit/" + commit)
				}
			case aboutHitAuthor:
				openURL(authorURL)
			case aboutHitGitHub:
				openAbout()
			case aboutHitCopy:
				copyTextToClipboard(fmt.Sprintf("KotoType %s (%s/%s)", opts.Version, runtime.GOOS, runtime.GOARCH))
				aboutCopied = true
				setTimer.Call(hwnd, aboutCopyTimer, 1800, 0)
				invalidateRect.Call(hwnd, 0, 0)
			}
		}
		return 0
	case wmTimer:
		if wParam == aboutCopyTimer {
			killTimer.Call(hwnd, aboutCopyTimer)
			aboutCopied = false
			invalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmKeyDown:
		if wParam == 0x1b { // VK_ESCAPE
			showWindow.Call(hwnd, swHide)
			releaseAboutSurface()
			trimIdleMemory()
		}
		return 0
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		var ps paintStruct
		beginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		paintAbout()
		endPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmClose:
		killTimer.Call(hwnd, aboutCopyTimer)
		aboutCopied = false
		showWindow.Call(hwnd, swHide)
		releaseAboutSurface()
		trimIdleMemory()
		return 0
	}
	return defProc(hwnd, message, wParam, lParam)
}
