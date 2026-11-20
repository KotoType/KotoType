package ui

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestDirect2DAndWrite(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	loadTheme()
	if !initDirect2D() {
		t.Fatal("initDirect2D failed")
	}
	defer releaseDirect2D()

	f := textFont{size: 13.5, middle: true}
	w, h := measureText("关于 KotoType v0.1.1-10-g8d79915", f, 400)
	if w < 80 || h < 10 || h > 40 {
		t.Fatalf("unexpected measureText size: w=%.2f h=%.2f", w, h)
	}

	wrap := textFont{size: 13.5, wrap: true, clipped: true}
	box := rectF{0, 0, 120, 40}
	long := strings.Repeat("这是一段用于测试语音条自动尾部截断的中文内容", 4)
	tail := tailFitting(long, box, wrap)
	if !strings.HasPrefix(tail, "…") || len([]rune(tail)) >= len([]rune(long)) {
		t.Fatalf("tailFitting did not truncate as expected: %q", tail)
	}

	// Render one full frame on a hidden window to verify every COM vtable slot.
	instance, _, _ = getModuleHandle.Call(0)
	register("KotoTypeD2DTest", defProc)
	hwnd, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(str("KotoTypeD2DTest"))), 0, wsPopup, 0, 0, 230, 84, 0, 0, instance, 0)
	if hwnd == 0 {
		t.Fatal("CreateWindowExW failed")
	}
	defer destroyWindow.Call(hwnd)

	c := newCanvas(hwnd, 96)
	defer c.release()
	if !c.begin() {
		t.Fatal("canvas.begin failed")
	}
	c.clear(th.flyout)
	c.fill(rectF{8, 8, 222, 76}, 8, th.card)
	c.stroke(rectF{8.5, 8.5, 221.5, 75.5}, 7.5, 1, th.border)
	c.circle(36, 42, 20, th.accent)
	c.line(68, 30, 190, 30, 1, th.divider)
	c.clip(rectF{68, 32, 190, 74})
	c.text(tail, rectF{68, 32, 190, 74}, wrap, th.text)
	c.text("\ue720", rectF{16, 22, 56, 62}, textFont{size: 16, icon: true, align: 2, middle: true}, th.onAccent)
	c.unclip()
	loadAboutLogoPixels()
	bmp := c.createBitmap(aboutLogoW, aboutLogoH, aboutLogoRaw)
	if bmp == nil {
		t.Fatal("createBitmap failed")
	}
	c.drawBitmap(bmp, rectF{0, 0, 32, 32})
	bmp.Release()
	if hr := c.target.Call(targetEndDraw, 0, 0); int32(hr) < 0 {
		t.Fatalf("EndDraw failed: 0x%08x", uint32(hr))
	}
}
