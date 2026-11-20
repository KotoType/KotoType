package ui

import (
	"image"
	"math"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KotoType/KotoType/internal/desktop"
	"github.com/KotoType/KotoType/internal/session"
)

// The floating bar never takes focus (WS_EX_NOACTIVATE + MA_NOACTIVATE), so
// clicking or resizing it leaves the caret in the destination app, like Win+H.

const (
	barDefaultW, barDefaultH = 380, 50 // Default compact size at 96 DPI.
	barMinW, barMaxW         = 190, 900
	barMinH, barMaxH         = 48, 360
	barResizeGrip            = 6 // Edge hit margin in DIPs.
	hideTimer                = 1
	defaultHideDelayMs       = 3000
	pulseTimer               = 2
	pulseInterval            = 33 // ~30 fps smooth breathing animation while listening.
)

const (
	hitNone = iota
	hitMic
	hitSettings
	hitClose
)

const (
	edgeLeft = 1 << iota
	edgeRight
	edgeTop
	edgeBottom
)

var (
	barWnd             uintptr
	barDPI             int // Scale of the monitor the bar is on.
	barCurW, barCurH   int // Current logical size in 96-DPI DIPs.
	barState           session.State
	barText            string    // Last transcript; survives the dictation ending.
	barPulseStart      time.Time // Start time of the listening breathing animation.
	barHover           int
	barPressed         int
	barResizeEdge      int // Non-zero bitmask of edge* while dragging an edge.
	barResizeStartPt   point
	barResizeStartRect windows.Rect
	barClosed          bool    // The user closed the bar mid-dictation; stay hidden until it has ended.
	barUserMoved       bool    // The user manually dragged the bar during this dictation.
	barOwner           uintptr // Window the bar is about; 0 when shown from the tray.
	barCanvas          *canvas
)

func px(v int) int32 { return int32(v * barDPI / 96) }

func clampBarSize(w, h int) (int, int) {
	if w <= 0 {
		w = barDefaultW
	}
	if h <= 0 {
		h = barDefaultH
	}
	return max(barMinW, min(w, barMaxW)), max(barMinH, min(h, barMaxH))
}

func barMicCenter() image.Point { return image.Pt(25, barCurH/2) }

func barButtonBoxes() (closeR, settingsR image.Rectangle) {
	top := (barCurH - 24) / 2
	if barCurH > 72 {
		top = 10
	}
	closeRight := barCurW - 8
	closeLeft := closeRight - 24
	setRight := closeLeft - 2
	setLeft := setRight - 24
	return image.Rect(closeLeft, top, closeRight, top+24), image.Rect(setLeft, top, setRight, top+24)
}

func createBar() {
	barCurW, barCurH = clampBarSize(cfg.BarWidth, cfg.BarHeight)
	barWnd, _, _ = createWindow.Call(wsExTopmost|wsExToolWin|wsExNoActive, uintptr(unsafe.Pointer(str("KotoTypeBar"))), uintptr(unsafe.Pointer(str("KotoType"))), wsPopup,
		0, 0, 0, 0, 0, 0, instance, 0)
	round := int32(2) // DWMWCP_ROUND: Windows 11 rounded corners, border and shadow.
	dwmSetAttribute.Call(barWnd, 33, uintptr(unsafe.Pointer(&round)), 4)
	styleFrame(barWnd, false)
	barCanvas = newCanvas(barWnd, 96)
	setBarDPI(96)
	// EVENT_SYSTEM_FOREGROUND, delivered to this thread's message loop
	// (WINEVENT_OUTOFCONTEXT | WINEVENT_SKIPOWNPROCESS).
	setWinEventHook.Call(3, 3, 0, syscall.NewCallback(barForeground), 0, 0, 0x2)
}

// barForeground puts the bar away as soon as the user switches away from the
// window it is about. The event's own window is not trusted (apps report
// helper windows, see desktop's foreground hook); the real foreground is
// checked instead. The taskbar does not count, so the tray icon still works.
func barForeground(_, _, _, _, _, _, _ uintptr) uintptr {
	foreground, _, _ := getForeground.Call()
	if barOwner == 0 || foreground == 0 || foreground == barOwner || !visible(barWnd) {
		return 0
	}
	var class [64]uint16
	getClassName.Call(foreground, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
	switch windows.UTF16ToString(class[:]) {
	case "Shell_TrayWnd", "Shell_SecondaryTrayWnd", "NotifyIconOverflowWindow", "TopLevelWindowForOverflowXamlIsland":
		return 0
	}
	closeBar()
	return 0
}

func hideBar() {
	killTimer.Call(barWnd, hideTimer)
	killTimer.Call(barWnd, pulseTimer)
	barPulseStart = time.Time{}
	showWindow.Call(barWnd, swHide)
	barCanvas.release()
	trimIdleMemory()
}

// closeBar ends the dictation and hides the bar until the next one starts.
func closeBar() {
	barClosed = barState.Phase != session.Idle
	go opts.Stop()
	hideBar()
}

// setBarDPI sizes the bar for the monitor it is on.
func setBarDPI(dpi int) {
	if dpi == barDPI {
		return
	}
	barDPI = dpi
	barCanvas.setDPI(dpi)
	setWindowPos.Call(barWnd, 0, 0, 0, uintptr(px(barCurW)), uintptr(px(barCurH)), 0x2|0x4|0x10) // NOMOVE | NOZORDER | NOACTIVATE
}

func barEdge(xPx, yPx int32) int {
	x, y := int(xPx)*96/barDPI, int(yPx)*96/barDPI
	edge := 0
	if x < barResizeGrip {
		edge |= edgeLeft
	} else if x >= barCurW-barResizeGrip {
		edge |= edgeRight
	}
	if y < barResizeGrip {
		edge |= edgeTop
	} else if y >= barCurH-barResizeGrip {
		edge |= edgeBottom
	}
	return edge
}

func barCursorID(edge int) uintptr {
	switch edge {
	case edgeLeft | edgeTop, edgeRight | edgeBottom:
		return 32642 // IDC_SIZENWSE
	case edgeRight | edgeTop, edgeLeft | edgeBottom:
		return 32643 // IDC_SIZENESW
	case edgeLeft, edgeRight:
		return 32644 // IDC_SIZEWE
	case edgeTop, edgeBottom:
		return 32645 // IDC_SIZENS
	default:
		return 32512 // IDC_ARROW
	}
}

func barHit(x, y int32) int {
	p := image.Pt(int(x)*96/barDPI, int(y)*96/barDPI)
	d := p.Sub(barMicCenter())
	closeBox, settingsBox := barButtonBoxes()
	switch {
	case d.X*d.X+d.Y*d.Y <= 19*19:
		return hitMic
	case p.In(settingsBox):
		return hitSettings
	case p.In(closeBox):
		return hitClose
	}
	return hitNone
}

// placeBar puts the bar just below the caret (or above it near the bottom of
// the screen), falling back to the bottom centre of the active monitor.
func placeBar(s session.State) {
	var monitor uintptr
	if s.Placed {
		monitor, _, _ = monitorFromPoint.Call(uintptr(uint32(s.Anchor.Min.X))|uintptr(uint32(s.Anchor.Min.Y))<<32, 2)
	} else {
		fg, _, _ := getForeground.Call()
		monitor, _, _ = monitorFromWindow.Call(fg, 2) // MONITOR_DEFAULTTONEAREST
	}
	var dpi, dpiY uint32
	getDpiForMonitor.Call(monitor, 0, uintptr(unsafe.Pointer(&dpi)), uintptr(unsafe.Pointer(&dpiY))) // MDT_EFFECTIVE_DPI
	setBarDPI(int(max(dpi, 96)))
	w, h, gap := px(barCurW), px(barCurH), px(12)
	work := workArea(monitor)
	x, y := (work.Left+work.Right-w)/2, work.Bottom-h-px(48)
	if s.Placed {
		x = int32(s.Anchor.Min.X)
		switch {
		case int32(s.Anchor.Max.Y)+gap+h <= work.Bottom:
			y = int32(s.Anchor.Max.Y) + gap
		case int32(s.Anchor.Min.Y)-gap-h >= work.Top:
			y = int32(s.Anchor.Min.Y) - gap - h
		default:
			x = (work.Left + work.Right - w) / 2
		}
	}
	x = max(work.Left, min(x, work.Right-w))
	y = max(work.Top, min(y, work.Bottom-h))
	setWindowPos.Call(barWnd, ^uintptr(0), uintptr(x), uintptr(y), 0, 0, 0x1|0x10) // HWND_TOPMOST, NOSIZE | NOACTIVATE
}

func showBarIdle() {
	if !visible(barWnd) {
		placeBar(session.State{})
		barOwner = 0
	}
	styleFrame(barWnd, false)
	showWindow.Call(barWnd, swShowNoActive)
	invalidateRect.Call(barWnd, 0, 0)
}

func barHideDelayMs() uintptr {
	s := strings.TrimSpace(cfg.HideDelay)
	if s == "" {
		return defaultHideDelayMs
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 {
		return defaultHideDelayMs
	}
	if sec > 3600 {
		sec = 3600
	}
	return uintptr(math.Round(sec * 1000))
}

func applyState(s session.State) {
	was := barState.Phase
	prevText, prevMsg, prevAnchor := barText, barState.Message, barState.Anchor
	if was == session.Idle && s.Phase != session.Idle {
		if cfg.Sound != "off" {
			play(startSound)
		}
		barText = ""
		barUserMoved = false
		placeBar(s)
		barOwner, _, _ = getForeground.Call()
	} else if s.Phase != session.Idle && s.Placed && !barUserMoved &&
		(s.Anchor.Min.Y != prevAnchor.Min.Y || s.Anchor.Max.Y != prevAnchor.Max.Y) {
		// Caret wrapped onto a new line: follow it vertically so the bar never covers typed text.
		placeBar(s)
	}
	if was != session.Idle && s.Phase == session.Idle && cfg.Sound != "off" {
		play(stopSound)
	}
	if s.Text != "" {
		barText = session.Format(s.Text, cfg.Punctuation)
	}
	barState = s
	if barClosed {
		// The dictation being aborted still reports its last states.
		barClosed = s.Phase != session.Idle
		return
	}
	if s.Phase == session.Listening && was != session.Listening {
		barPulseStart = time.Now()
		setTimer.Call(barWnd, pulseTimer, pulseInterval, 0)
	} else if s.Phase != session.Listening && was == session.Listening {
		killTimer.Call(barWnd, pulseTimer)
		barPulseStart = time.Time{}
	}
	if s.Phase == session.Idle {
		delay := barHideDelayMs()
		// When delay is 0 and an active dictation just finished, hide immediately.
		if delay == 0 && was != session.Idle {
			hideBar()
			return
		}
		if delay == 0 {
			delay = defaultHideDelayMs // Keep standalone error notices readable.
		}
		// Also surfaces "focus is not a text field" when nothing started.
		if !visible(barWnd) {
			placeBar(s)
			barOwner, _, _ = getForeground.Call()
		}
		setTimer.Call(barWnd, hideTimer, delay, 0)
	} else {
		killTimer.Call(barWnd, hideTimer)
	}
	// Skip level-only updates while listening so the bar only repaints on text
	// changes or the blink timer.
	if s.Phase == was && barText == prevText && s.Message == prevMsg && visible(barWnd) {
		return
	}
	styleFrame(barWnd, false)
	showWindow.Call(barWnd, swShowNoActive)
	invalidateRect.Call(barWnd, 0, 0)
}

func finishBarResize(release bool) {
	if barResizeEdge == 0 {
		return
	}
	barResizeEdge = 0
	if release {
		releaseCapture.Call()
	}
	if cfg.BarWidth != barCurW || cfg.BarHeight != barCurH {
		cfg.BarWidth, cfg.BarHeight = barCurW, barCurH
		opts.OnChange(cfg)
	}
}

// paintBar draws the bar: the microphone button (accent while listening,
// with a level-driven halo), the status line, the transcript tail and the
// close/settings icon buttons.
func paintBar() {
	c := barCanvas
	if !c.begin() {
		return
	}
	c.clear(th.flyout)
	hotkey := desktop.HotkeyName(cfg.Hotkey)
	mic, glyph, status := th.control, th.text, barState.Message
	listening := barState.Phase == session.Listening
	mc := barMicCenter()
	switch barState.Phase {
	case session.Connecting:
		status = "正在连接…"
	case session.Listening:
		phase := float64(0)
		if !barPulseStart.IsZero() {
			phase = float64(time.Since(barPulseStart)) / float64(1400*time.Millisecond)
		}
		breath := float32(0.5 - 0.5*math.Cos(phase*2*math.Pi))
		wave := float32(phase - math.Floor(phase))
		// Smooth expanding ripple + gentle breathing halo (stays within 50 DIP height).
		c.circle(float32(mc.X), float32(mc.Y), 17+wave*7.5, th.accent.alpha((1-wave)*0.24))
		c.circle(float32(mc.X), float32(mc.Y), 17+breath*4.5, th.accent.alpha(0.08+breath*0.18))
		mic, glyph = th.accent.alpha(0.84+breath*0.16), th.onAccent
		status = "聆听中 · 按任意键停止"
	case session.Finishing:
		status = "正在完成…"
	default:
		if status == "" {
			status = hotkey + " 开始听写"
		}
	}
	if listening {
		switch {
		case barPressed == hitMic && barHover == hitMic:
			mic = th.accentPressed
		case barHover == hitMic:
			mic = th.accentHover
		}
	} else {
		switch {
		case barPressed == hitMic && barHover == hitMic:
			mic = th.pressed
		case barHover == hitMic:
			mic = th.hover
		}
	}
	micBox := rectF{float32(mc.X - 17), float32(mc.Y - 17), float32(mc.X + 17), float32(mc.Y + 17)}
	c.fill(micBox, 17, mic)
	if !listening {
		border := th.controlBorder
		if barState.Phase == session.Connecting || barState.Phase == session.Finishing {
			border = th.accent
		} else if barHover == hitMic {
			border = th.controlHoverBorder
		}
		c.stroke(micBox.inset(0.5), 16.5, 1, border)
	}
	c.text("\ue720", micBox, textFont{size: 14.5, icon: true, align: 2, middle: true}, glyph) // Microphone

	closeBox, settingsBox := barButtonBoxes()
	for _, b := range []struct {
		hit   int
		box   image.Rectangle
		glyph string
	}{{hitClose, closeBox, "\ue711"}, {hitSettings, settingsBox, "\ue713"}} {
		r := rectF{float32(b.box.Min.X), float32(b.box.Min.Y), float32(b.box.Max.X), float32(b.box.Max.Y)}
		if barHover == b.hit {
			bg := th.itemHover
			if barPressed == b.hit {
				bg = th.itemPressed
			}
			c.fill(r, 4, bg)
		}
		c.text(b.glyph, r, textFont{size: 11.5, icon: true, align: 2, middle: true}, th.subtle)
	}

	textR := float32(barCurW - 62)
	c.text(status, rectF{52, 5, textR, 20}, textFont{size: 11, middle: true}, th.subtle)
	text, color := barText, th.text
	if text == "" && barState.Phase != session.Idle {
		text, color = "请开始说话…", th.disabled
	}
	box, f := rectF{52, 21, textR, float32(barCurH - 5)}, textFont{size: 13.5, wrap: true, clipped: true}
	c.text(tailFitting(text, box, f), box, f, color)
	c.end()
}

func barProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmMouseActivate:
		return 3 // MA_NOACTIVATE
	case wmNCHitTest:
		var origin point
		clientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&origin)))
		cx, cy := loword(lParam)-origin.X, hiword(lParam)-origin.Y
		if barResizeEdge != 0 || barEdge(cx, cy) != 0 || barHit(cx, cy) != hitNone {
			return 1 // HTCLIENT
		}
		return 2 // HTCAPTION: drag the bar from anywhere else.
	case wmSetCursor:
		if loword(lParam) == 1 { // HTCLIENT
			edge := barResizeEdge
			if edge == 0 {
				var p, origin point
				getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
				clientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&origin)))
				edge = barEdge(p.X-origin.X, p.Y-origin.Y)
			}
			cur, _, _ := loadCursor.Call(0, barCursorID(edge))
			setCursor.Call(cur)
			return 1
		}
	case wmDPIChanged:
		// Dragged onto a monitor with another scale.
		setBarDPI(int(hiword(wParam)))
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmMouseMove:
		if barResizeEdge != 0 {
			var cur point
			getCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
			dx, dy := cur.X-barResizeStartPt.X, cur.Y-barResizeStartPt.Y
			r := barResizeStartRect
			minW, maxW := px(barMinW), px(barMaxW)
			minH, maxH := px(barMinH), px(barMaxH)
			if barResizeEdge&edgeLeft != 0 {
				r.Left = min(r.Right-minW, max(r.Right-maxW, barResizeStartRect.Left+dx))
			}
			if barResizeEdge&edgeRight != 0 {
				r.Right = max(r.Left+minW, min(r.Left+maxW, barResizeStartRect.Right+dx))
			}
			if barResizeEdge&edgeTop != 0 {
				r.Top = min(r.Bottom-minH, max(r.Bottom-maxH, barResizeStartRect.Top+dy))
			}
			if barResizeEdge&edgeBottom != 0 {
				r.Bottom = max(r.Top+minH, min(r.Top+maxH, barResizeStartRect.Bottom+dy))
			}
			barCurW, barCurH = clampBarSize(
				int(math.Round(float64(r.Right-r.Left)*96/float64(barDPI))),
				int(math.Round(float64(r.Bottom-r.Top)*96/float64(barDPI))),
			)
			setWindowPos.Call(hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0x4|0x10) // NOZORDER | NOACTIVATE
			invalidateRect.Call(hwnd, 0, 0)
			return 0
		}
		if hit := barHit(loword(lParam), hiword(lParam)); hit != barHover {
			barHover = hit
			invalidateRect.Call(hwnd, 0, 0)
			trackMouseLeave(hwnd)
		}
		return 0
	case wmMouseLeave:
		if barResizeEdge == 0 {
			barHover = hitNone
			barPressed = hitNone
			invalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmLButtonDown:
		x, y := loword(lParam), hiword(lParam)
		if edge := barEdge(x, y); edge != 0 {
			barResizeEdge = edge
			barHover, barPressed = hitNone, hitNone
			getCursorPos.Call(uintptr(unsafe.Pointer(&barResizeStartPt)))
			getWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&barResizeStartRect)))
			setCapture.Call(hwnd)
			invalidateRect.Call(hwnd, 0, 0)
			return 0
		}
		barPressed = barHit(x, y)
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmLButtonUp:
		if barResizeEdge != 0 {
			finishBarResize(true)
			return 0
		}
		hit := barHit(loword(lParam), hiword(lParam))
		pressed := barPressed
		barPressed = hitNone
		invalidateRect.Call(hwnd, 0, 0)
		if hit != pressed || hit == hitNone {
			return 0
		}
		switch hit {
		case hitMic:
			go opts.Toggle()
		case hitSettings, hitClose:
			// Both end the dictation and put the bar away; opening settings
			// would take the focus from the text field anyway.
			closeBar()
			if hit == hitSettings {
				showSettings()
			}
		}
		return 0
	case wmCaptureChanged:
		finishBarResize(false)
		return 0
	case wmExitSizeMove:
		barUserMoved = true
		return 0
	case wmTimer:
		switch wParam {
		case hideTimer:
			killTimer.Call(hwnd, hideTimer)
			if barState.Phase == session.Idle && barHover == hitNone && barResizeEdge == 0 {
				hideBar()
			} else if barState.Phase == session.Idle {
				delay := barHideDelayMs()
				if delay == 0 {
					delay = 500
				}
				setTimer.Call(hwnd, hideTimer, delay, 0)
			}
		case pulseTimer:
			if barState.Phase == session.Listening {
				invalidateRect.Call(hwnd, 0, 0)
			} else {
				killTimer.Call(hwnd, pulseTimer)
				barPulseStart = time.Time{}
			}
		}
		return 0
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		var ps paintStruct
		beginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		paintBar()
		endPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	return defProc(hwnd, message, wParam, lParam)
}
