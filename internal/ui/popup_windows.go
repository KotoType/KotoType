package ui

import (
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A shared Direct2D Fluent popup used for settings drop-down lists and the
// system-tray context menu.

const (
	popupPad    = 4
	popupRowH   = 32
	popupSepH   = 9
	popupMaxH   = 320 // Up to 10 rows visible before scrolling.
	popupScroll = 64  // Wheel step in DIPs.
)

type popupItem struct {
	label     string
	icon      string // Optional Segoe Fluent Icons glyph.
	separator bool
}

var (
	popupWnd       uintptr
	popupOwner     uintptr // Non-zero for owned drop-downs so the owner keeps focus.
	popupCanvas    *canvas
	popupDPI       int
	popupItems     []popupItem
	popupSelected  = -1
	popupHover     = -1
	popupPressed   = -1
	popupScrollY   float32
	popupContentH  float32
	popupViewH     float32
	popupW, popupH float32
	popupOnSelect  func(int)
	popupTag       = -1
)

func createPopup() {
	popupWnd, _, _ = createWindow.Call(wsExTopmost|wsExToolWin, uintptr(unsafe.Pointer(str("KotoTypePopup"))), 0, wsPopup,
		0, 0, 0, 0, 0, 0, instance, 0)
	round := int32(2) // DWMWCP_ROUND: Windows 11 rounded corners, border and shadow.
	dwmSetAttribute.Call(popupWnd, 33, uintptr(unsafe.Pointer(&round)), 4)
	styleFrame(popupWnd, false)
	popupDPI = 96
	popupCanvas = newCanvas(popupWnd, popupDPI)
}

func popupRowTop(index int) (top, height float32) {
	y := float32(0)
	for i, it := range popupItems {
		h := float32(popupRowH)
		if it.separator {
			h = popupSepH
		}
		if i == index {
			return y, h
		}
		y += h
	}
	return y, 0
}

func popupMaxScroll() float32 {
	if popupContentH <= popupViewH {
		return 0
	}
	return popupContentH - popupViewH
}

func popupEnsureVisible(index int) {
	if index < 0 || index >= len(popupItems) {
		return
	}
	top, h := popupRowTop(index)
	if top < popupScrollY {
		popupScrollY = top
	} else if top+h > popupScrollY+popupViewH {
		popupScrollY = top + h - popupViewH
	}
	popupScrollY = max(0, min(popupScrollY, popupMaxScroll()))
}

// showPopup opens the Fluent flyout anchored to screen rect anchor. When owner
// is non-zero the popup never steals activation, so the owner keeps its active
// title bar and Mica backdrop.
func showPopup(owner uintptr, anchor windows.Rect, minW float32, dpi int, items []popupItem, selected int, tag int, onSelect func(int)) {
	if len(items) == 0 {
		return
	}
	popupOwner = owner
	popupItems = items
	popupSelected = selected
	popupHover = -1
	popupPressed = -1
	popupOnSelect = onSelect
	popupTag = tag
	popupDPI = max(dpi, 96)
	popupCanvas.setDPI(popupDPI)

	hasIcon := false
	for _, it := range items {
		if it.icon != "" {
			hasIcon = true
			break
		}
	}
	labelFont := textFont{size: 13.5, middle: true}
	extraW := float32(32)
	if hasIcon {
		extraW = 54
	}
	w := minW
	var totalH float32
	for _, it := range items {
		if it.separator {
			totalH += popupSepH
			continue
		}
		totalH += popupRowH
		tw, _ := measureText(it.label, labelFont, 600)
		if tw+extraW > w {
			w = tw + extraW
		}
	}
	w = float32(math.Ceil(float64(w)))
	popupContentH = totalH
	popupViewH = min(totalH, popupMaxH)
	if popupContentH > popupViewH {
		w += 10 // Room for the slim scrollbar.
	}
	popupW = w
	popupH = popupViewH + popupPad*2
	popupScrollY = 0
	if selected >= 0 && selected < len(items) {
		top, h := popupRowTop(selected)
		popupScrollY = max(0, min(top-(popupViewH-h)/2, popupMaxScroll()))
	}

	cx := (anchor.Left + anchor.Right) / 2
	cy := (anchor.Top + anchor.Bottom) / 2
	monitor, _, _ := monitorFromPoint.Call(uintptr(uint32(cx))|uintptr(uint32(cy))<<32, 2) // MONITOR_DEFAULTTONEAREST
	work := workArea(monitor)
	scale := func(v float32) int32 { return int32(math.Round(float64(v) * float64(popupDPI) / 96)) }
	wPx, hPx := scale(popupW), scale(popupH)
	if maxW := work.Right - work.Left - 16; wPx > maxW && maxW > 120 {
		wPx = maxW
		popupW = float32(wPx) * 96 / float32(popupDPI)
	}
	gap := scale(4)
	x := anchor.Left
	y := anchor.Bottom + gap
	if y+hPx > work.Bottom-gap && anchor.Top-gap-hPx >= work.Top+gap {
		y = anchor.Top - gap - hPx
	}
	x = max(work.Left+gap, min(x, work.Right-wPx-gap))
	y = max(work.Top+gap, min(y, work.Bottom-hPx-gap))

	styleFrame(popupWnd, false)
	flags := uintptr(0x40) // SWP_SHOWWINDOW
	if owner != 0 {
		flags |= 0x10 // SWP_NOACTIVATE
	}
	setWindowPos.Call(popupWnd, ^uintptr(0), uintptr(x), uintptr(y), uintptr(wPx), uintptr(hPx), flags) // HWND_TOPMOST
	if owner == 0 {
		setForeground.Call(popupWnd)
	}
	invalidateRect.Call(popupWnd, 0, 0)
}

func closePopup() {
	if !visible(popupWnd) {
		return
	}
	owner := popupOwner
	popupOwner = 0
	popupTag = -1
	popupOnSelect = nil
	showWindow.Call(popupWnd, swHide)
	popupCanvas.release()
	if owner != 0 {
		invalidateRect.Call(owner, 0, 0)
	}
}

func popupHit(xPx, yPx int32) int {
	x, y := popupCanvas.pixels(xPx), popupCanvas.pixels(yPx)
	if x < popupPad || x >= popupW-popupPad || y < popupPad || y >= popupH-popupPad {
		return -1
	}
	contentY := y - popupPad + popupScrollY
	cur := float32(0)
	for i, it := range popupItems {
		h := float32(popupRowH)
		if it.separator {
			h = popupSepH
		}
		if contentY >= cur && contentY < cur+h {
			if it.separator {
				return -1
			}
			return i
		}
		cur += h
	}
	return -1
}

func popupMoveSelection(delta int) {
	if len(popupItems) == 0 {
		return
	}
	i := popupHover
	if i < 0 {
		if popupSelected >= 0 {
			i = popupSelected
		} else if delta > 0 {
			i = -1
		} else {
			i = len(popupItems)
		}
	}
	for step := 0; step < len(popupItems); step++ {
		i = (i + delta + len(popupItems)) % len(popupItems)
		if !popupItems[i].separator {
			popupHover = i
			popupEnsureVisible(i)
			invalidateRect.Call(popupWnd, 0, 0)
			return
		}
	}
}

func popupChoose(index int) {
	if index < 0 || index >= len(popupItems) || popupItems[index].separator {
		return
	}
	cb := popupOnSelect
	closePopup()
	if cb != nil {
		cb(index)
	}
}

func paintPopup() {
	c := popupCanvas
	if !c.begin() {
		return
	}
	c.clear(th.flyout)
	view := rectF{popupPad, popupPad, popupW - popupPad, popupH - popupPad}
	hasScroll := popupContentH > popupViewH
	rightPad := float32(popupPad)
	if hasScroll {
		rightPad += 8
	}
	hasIcon := false
	for _, it := range popupItems {
		if it.icon != "" {
			hasIcon = true
			break
		}
	}

	c.clip(view)
	y := float32(popupPad) - popupScrollY
	for i, it := range popupItems {
		if it.separator {
			mid := y + popupSepH/2
			if mid >= view.T-2 && mid <= view.B+2 {
				c.line(popupPad+8, mid, popupW-rightPad-8, mid, 1, th.divider)
			}
			y += popupSepH
			continue
		}
		rowRect := rectF{popupPad, y, popupW - rightPad, y + popupRowH}
		if rowRect.B >= view.T && rowRect.T <= view.B {
			switch {
			case i == popupPressed && i == popupHover:
				c.fill(rowRect, 4, th.itemPressed)
			case i == popupHover:
				c.fill(rowRect, 4, th.itemHover)
			case i == popupSelected:
				c.fill(rowRect, 4, th.itemHover.alpha(th.itemHover.A*0.75))
			}
			if i == popupSelected {
				// Windows 11 accent selection pill on the leading edge.
				pill := rectF{rowRect.L + 1, rowRect.T + 8, rowRect.L + 4, rowRect.B - 8}
				c.fill(pill, 1.5, th.accent)
			}
			textL := rowRect.L + 12
			if hasIcon {
				if it.icon != "" {
					iconBox := rectF{rowRect.L + 8, rowRect.T, rowRect.L + 28, rowRect.B}
					c.text(it.icon, iconBox, textFont{size: 14, icon: true, align: 2, middle: true}, th.subtle)
				}
				textL = rowRect.L + 36
			}
			c.text(it.label, rectF{textL, rowRect.T, rowRect.R - 10, rowRect.B}, textFont{size: 13.5, middle: true}, th.text)
		}
		y += popupRowH
	}
	c.unclip()

	if hasScroll {
		trackT, trackB := float32(popupPad+4), popupH-popupPad-4
		trackH := trackB - trackT
		thumbH := max(24, trackH*(popupViewH/popupContentH))
		ratio := popupScrollY / popupMaxScroll()
		thumbT := trackT + (trackH-thumbH)*ratio
		thumb := rectF{popupW - 7, thumbT, popupW - 4, thumbT + thumbH}
		c.fill(thumb, 1.5, th.subtle.alpha(0.45))
	}
	c.end()
}

func popupHandleKey(vk uintptr) bool {
	switch vk {
	case 0x1b: // VK_ESCAPE
		closePopup()
		return true
	case 0x26: // VK_UP
		popupMoveSelection(-1)
		return true
	case 0x28: // VK_DOWN
		popupMoveSelection(1)
		return true
	case 0x0d: // VK_RETURN
		if popupHover >= 0 {
			popupChoose(popupHover)
		} else if popupSelected >= 0 {
			popupChoose(popupSelected)
		}
		return true
	}
	return false
}

func popupScrollWheel(wParam uintptr) {
	if maxS := popupMaxScroll(); maxS > 0 {
		delta := float32(hiword(wParam)) / 120
		popupScrollY = max(0, min(popupScrollY-delta*popupScroll, maxS))
		invalidateRect.Call(popupWnd, 0, 0)
	}
}

func popupProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmMouseActivate:
		if popupOwner != 0 {
			return 3 // MA_NOACTIVATE: keep the settings window focused.
		}
		return 1 // MA_ACTIVATE
	case wmActivate:
		if popupOwner == 0 && wParam&0xffff == 0 {
			closePopup()
		}
		return 0
	case wmMouseMove:
		if hit := popupHit(loword(lParam), hiword(lParam)); hit != popupHover {
			popupHover = hit
			invalidateRect.Call(hwnd, 0, 0)
			trackMouseLeave(hwnd)
		}
		return 0
	case wmMouseLeave:
		if popupHover != -1 {
			popupHover = -1
			invalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmLButtonDown:
		popupPressed = popupHit(loword(lParam), hiword(lParam))
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmLButtonUp:
		hit := popupHit(loword(lParam), hiword(lParam))
		pressed := popupPressed
		popupPressed = -1
		if hit >= 0 && hit == pressed {
			popupChoose(hit)
		} else {
			invalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmMouseWheel:
		popupScrollWheel(wParam)
		return 0
	case wmKeyDown:
		popupHandleKey(wParam)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		var ps paintStruct
		beginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		paintPopup()
		endPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	return defProc(hwnd, message, wParam, lParam)
}
