package ui

import (
	"math"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/KotoType/KotoType/internal/winapi"
)

// Direct2D and DirectWrite through raw COM vtables, for the bar, settings
// window and popup flyouts. Everything is drawn in 96-DPI units (DIPs): each
// canvas tells its render target the window's DPI.
//
// Float arguments are passed as their bits: on Windows amd64 Go's syscall
// copies the first four arguments into both integer and XMM registers, and
// later arguments go on the stack where the callee reads the low four bytes.

var (
	d2dCreateFactory    = windows.NewLazySystemDLL("d2d1.dll").NewProc("D2D1CreateFactory")
	dwriteCreateFactory = windows.NewLazySystemDLL("dwrite.dll").NewProc("DWriteCreateFactory")
	iidD2DFactory       = windows.GUID{Data1: 0x06152247, Data2: 0x6f50, Data3: 0x465a, Data4: [8]byte{0x92, 0x45, 0x11, 0x8b, 0xfd, 0x3b, 0x60, 0x07}}
	iidDWriteFactory    = windows.GUID{Data1: 0xb859ee5a, Data2: 0xd838, Data3: 0x4b5b, Data4: [8]byte{0xa2, 0xe8, 0x1a, 0xdc, 0x7d, 0x93, 0xdb, 0x48}}
	d2dFactory          *winapi.Object
	dwFactory           *winapi.Object
	dwFormats           = map[textFont]*winapi.Object{}
	uiFontName          = "Segoe UI Variable Text"
	iconFontName        = "Segoe Fluent Icons"
)

// COM vtable slots verified against Windows SDK d2d1.h and dwrite.h.
const (
	factoryCreateHwndTarget = 14 // ID2D1Factory::CreateHwndRenderTarget
	targetCreateBitmap      = 4  // ID2D1RenderTarget::CreateBitmap
	targetCreateBrush       = 8  // ID2D1RenderTarget::CreateSolidColorBrush
	targetDrawLine          = 15
	targetFillRect          = 17
	targetDrawRounded       = 18
	targetFillRounded       = 19
	targetFillEllipse       = 21
	targetDrawBitmap        = 26
	targetDrawText          = 27
	targetTextAntialias     = 34
	targetPushClip          = 45
	targetPopClip           = 46
	targetClear             = 47
	targetBeginDraw         = 48
	targetEndDraw           = 49
	targetSetDpi            = 51
	hwndTargetResize        = 58 // ID2D1HwndRenderTarget::Resize
	brushSetColor           = 8  // ID2D1SolidColorBrush::SetColor
	dwCreateTextFormat      = 15 // IDWriteFactory::CreateTextFormat
	dwCreateTextLayout      = 18
	dwCreateEllipsis        = 20
	formatSetAlignment      = 3 // IDWriteTextFormat::SetTextAlignment
	formatSetParagraph      = 4
	formatSetWrapping       = 5
	formatSetTrimming       = 9
	layoutGetMetrics        = 60 // IDWriteTextLayout::GetMetrics
)

const recreateTarget = 0x8899000c // D2DERR_RECREATE_TARGET

func f32(v float32) uintptr { return uintptr(math.Float32bits(v)) }

// initDirect2D creates the process-wide factories; false if unavailable.
func initDirect2D() bool {
	if hr, _, _ := d2dCreateFactory.Call(0, uintptr(unsafe.Pointer(&iidD2DFactory)), 0, uintptr(unsafe.Pointer(&d2dFactory))); winapi.Failed(hr) {
		return false
	}
	if hr, _, _ := dwriteCreateFactory.Call(0, uintptr(unsafe.Pointer(&iidDWriteFactory)), uintptr(unsafe.Pointer(&dwFactory))); winapi.Failed(hr) {
		d2dFactory.Release()
		d2dFactory = nil
		return false
	}
	// Fall back gracefully on Windows 10 where Fluent fonts are absent.
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Fonts`, registry.QUERY_VALUE); err == nil {
		names, _ := key.ReadValueNames(0)
		key.Close()
		var hasFluent, hasVar bool
		for _, n := range names {
			low := strings.ToLower(n)
			if strings.Contains(low, "segoe fluent icons") {
				hasFluent = true
			}
			if strings.Contains(low, "segoe ui variable") {
				hasVar = true
			}
		}
		if !hasFluent {
			iconFontName = "Segoe MDL2 Assets"
		}
		if !hasVar {
			uiFontName = "Microsoft YaHei UI"
		}
	}
	return true
}

func releaseDirect2D() {
	for k, f := range dwFormats {
		f.Release()
		delete(dwFormats, k)
	}
	dwFactory.Release()
	d2dFactory.Release()
	dwFactory, d2dFactory = nil, nil
}

type rgba struct{ R, G, B, A float32 }

// hex returns an opaque 0xRRGGBB colour.
func hex(c uint32) rgba {
	return rgba{float32(c>>16&0xff) / 255, float32(c>>8&0xff) / 255, float32(c&0xff) / 255, 1}
}

func (c rgba) alpha(a float32) rgba { c.A = a; return c }

// mix moves c towards o by t.
func (c rgba) mix(o rgba, t float32) rgba {
	return rgba{c.R + (o.R-c.R)*t, c.G + (o.G-c.G)*t, c.B + (o.B-c.B)*t, c.A + (o.A-c.A)*t}
}

type rectF struct{ L, T, R, B float32 }

func (r rectF) contains(x, y float32) bool { return x >= r.L && x < r.R && y >= r.T && y < r.B }
func (r rectF) inset(d float32) rectF      { return rectF{r.L + d, r.T + d, r.R - d, r.B - d} }
func (r rectF) width() float32             { return r.R - r.L }
func (r rectF) height() float32            { return r.B - r.T }

// textFont describes a DirectWrite text format cached process-wide.
type textFont struct {
	size    float32
	bold    bool // 600 semibold when true, 400 regular otherwise
	icon    bool // Segoe Fluent Icons glyphs
	align   int  // 0 leading, 1 trailing, 2 centre
	middle  bool // Centre vertically in the rectangle.
	wrap    bool // Wrap lines; otherwise one line ending in an ellipsis.
	clipped bool // Clip to the rectangle.
}

// canvas draws one window with Direct2D.
type canvas struct {
	hwnd   uintptr
	dpi    float32
	target *winapi.Object
	brush  *winapi.Object
}

func newCanvas(hwnd uintptr, dpi int) *canvas {
	return &canvas{hwnd: hwnd, dpi: float32(dpi)}
}

func (c *canvas) setDPI(dpi int) {
	c.dpi = float32(dpi)
	if c.target != nil {
		c.target.Call(targetSetDpi, f32(c.dpi), f32(c.dpi))
	}
}

// pixels converts window client pixels to DIPs.
func (c *canvas) pixels(v int32) float32 { return float32(v) * 96 / c.dpi }

func (c *canvas) release() {
	if c == nil {
		return
	}
	c.brush.Release()
	c.target.Release()
	c.brush, c.target = nil, nil
}

// begin starts a frame, creating the render target when needed.
func (c *canvas) begin() bool {
	if c == nil || d2dFactory == nil || dwFactory == nil {
		return false
	}
	var client windows.Rect
	getClientRect.Call(c.hwnd, uintptr(unsafe.Pointer(&client)))
	if client.Right <= 0 || client.Bottom <= 0 {
		return false
	}
	size := [2]uint32{uint32(client.Right), uint32(client.Bottom)}
	if c.target != nil {
		if uint32(c.target.Call(hwndTargetResize, uintptr(unsafe.Pointer(&size)))) == recreateTarget {
			c.release()
		}
	}
	if c.target == nil {
		props := struct {
			Type, Format, Alpha uint32
			DpiX, DpiY          float32
			Usage, MinLevel     uint32
		}{Type: 1, Format: 87, Alpha: 1, DpiX: c.dpi, DpiY: c.dpi} // Software renderer (avoids 45MB+ GPU driver), B8G8R8A8 premultiplied for Mica.
		window := struct {
			Hwnd    uintptr
			Size    [2]uint32
			Present uint32
		}{Hwnd: c.hwnd, Size: size}
		if winapi.Failed(d2dFactory.Call(factoryCreateHwndTarget, uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&window)), uintptr(unsafe.Pointer(&c.target)))) {
			c.target = nil
			return false
		}
		white := rgba{1, 1, 1, 1}
		if winapi.Failed(c.target.Call(targetCreateBrush, uintptr(unsafe.Pointer(&white)), 0, uintptr(unsafe.Pointer(&c.brush)))) {
			c.release()
			return false
		}
		c.target.Call(targetTextAntialias, 2) // Grayscale: ClearType breaks alpha over Mica.
	}
	c.target.Call(targetBeginDraw)
	return true
}

func (c *canvas) end() {
	if uint32(c.target.Call(targetEndDraw, 0, 0)) == recreateTarget {
		c.release()
		invalidateRect.Call(c.hwnd, 0, 0)
	}
}

func (c *canvas) clear(col rgba) { c.target.Call(targetClear, uintptr(unsafe.Pointer(&col))) }

func (c *canvas) use(col rgba) uintptr {
	c.brush.Call(brushSetColor, uintptr(unsafe.Pointer(&col)))
	return uintptr(unsafe.Pointer(c.brush))
}

func (c *canvas) fill(r rectF, radius float32, col rgba) {
	brush := c.use(col)
	if radius == 0 {
		c.target.Call(targetFillRect, uintptr(unsafe.Pointer(&r)), brush)
		return
	}
	rounded := struct {
		rectF
		RX, RY float32
	}{r, radius, radius}
	c.target.Call(targetFillRounded, uintptr(unsafe.Pointer(&rounded)), brush)
}

// stroke outlines r with a line of width centred on its edge.
func (c *canvas) stroke(r rectF, radius, width float32, col rgba) {
	rounded := struct {
		rectF
		RX, RY float32
	}{r, radius, radius}
	c.target.Call(targetDrawRounded, uintptr(unsafe.Pointer(&rounded)), c.use(col), f32(width), 0)
}

func (c *canvas) circle(x, y, radius float32, col rgba) {
	ellipse := [4]float32{x, y, radius, radius}
	c.target.Call(targetFillEllipse, uintptr(unsafe.Pointer(&ellipse)), c.use(col))
}

func (c *canvas) line(x0, y0, x1, y1, width float32, col rgba) {
	pt := func(x, y float32) uintptr { return uintptr(math.Float32bits(x)) | uintptr(math.Float32bits(y))<<32 }
	c.target.Call(targetDrawLine, pt(x0, y0), pt(x1, y1), c.use(col), f32(width), 0)
}

func (c *canvas) clip(r rectF) { c.target.Call(targetPushClip, uintptr(unsafe.Pointer(&r)), 0) }
func (c *canvas) unclip()      { c.target.Call(targetPopClip) }

// createBitmap uploads premultiplied BGRA pixels to a Direct2D bitmap owned by
// c's current render target. The caller must release it before c.release().
func (c *canvas) createBitmap(w, h int, bgra []byte) *winapi.Object {
	if c == nil || c.target == nil || w <= 0 || h <= 0 || len(bgra) < w*h*4 {
		return nil
	}
	size := uintptr(uint32(w)) | uintptr(uint32(h))<<32
	props := struct {
		Format, Alpha uint32
		DpiX, DpiY    float32
	}{Format: 87, Alpha: 1, DpiX: 96, DpiY: 96} // B8G8R8A8_UNORM, PREMULTIPLIED
	var bmp *winapi.Object
	if winapi.Failed(c.target.Call(targetCreateBitmap, size, uintptr(unsafe.Pointer(&bgra[0])), uintptr(w*4),
		uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&bmp)))) {
		return nil
	}
	return bmp
}

func (c *canvas) drawBitmap(bmp *winapi.Object, r rectF) {
	if bmp == nil {
		return
	}
	c.target.Call(targetDrawBitmap, uintptr(unsafe.Pointer(bmp)), uintptr(unsafe.Pointer(&r)), f32(1), 1, 0)
}

func textFormatFor(f textFont) *winapi.Object {
	if dwFactory == nil {
		return nil
	}
	if format := dwFormats[f]; format != nil {
		return format
	}
	family := uiFontName
	if f.icon {
		family = iconFontName
	}
	weight := uintptr(400)
	if f.bold {
		weight = 600
	}
	var format *winapi.Object
	// The zh-cn locale makes DirectWrite fall back to Microsoft YaHei UI for Chinese.
	if winapi.Failed(dwFactory.Call(dwCreateTextFormat, uintptr(unsafe.Pointer(str(family))), 0, weight, 0, 5,
		f32(f.size), uintptr(unsafe.Pointer(str("zh-cn"))), uintptr(unsafe.Pointer(&format)))) {
		return nil
	}
	format.Call(formatSetAlignment, uintptr(f.align))
	if f.middle {
		format.Call(formatSetParagraph, 2)
	}
	if !f.wrap {
		format.Call(formatSetWrapping, 1) // DWRITE_WORD_WRAPPING_NO_WRAP
		var sign *winapi.Object
		dwFactory.Call(dwCreateEllipsis, uintptr(unsafe.Pointer(format)), uintptr(unsafe.Pointer(&sign)))
		trimming := [3]uint32{1} // DWRITE_TRIMMING_GRANULARITY_CHARACTER
		format.Call(formatSetTrimming, uintptr(unsafe.Pointer(&trimming)), uintptr(unsafe.Pointer(sign)))
		sign.Release() // The format keeps its own reference.
	}
	dwFormats[f] = format
	return format
}

func (c *canvas) text(s string, r rectF, f textFont, col rgba) {
	format := textFormatFor(f)
	if format == nil || s == "" {
		return
	}
	chars := utf16.Encode([]rune(s))
	options := uintptr(4) // D2D1_DRAW_TEXT_OPTIONS_ENABLE_COLOR_FONT
	if f.clipped {
		options |= 2 // D2D1_DRAW_TEXT_OPTIONS_CLIP
	}
	c.target.Call(targetDrawText, uintptr(unsafe.Pointer(&chars[0])), uintptr(len(chars)), uintptr(unsafe.Pointer(format)),
		uintptr(unsafe.Pointer(&r)), c.use(col), options, 0)
}

// measureText returns the size s takes in f, wrapped at width.
func measureText(s string, f textFont, width float32) (w, h float32) {
	format := textFormatFor(f)
	if format == nil || s == "" {
		return 0, 0
	}
	chars := utf16.Encode([]rune(s))
	var layout *winapi.Object
	if winapi.Failed(dwFactory.Call(dwCreateTextLayout, uintptr(unsafe.Pointer(&chars[0])), uintptr(len(chars)), uintptr(unsafe.Pointer(format)),
		f32(width), f32(1e6), uintptr(unsafe.Pointer(&layout)))) {
		return 0, 0
	}
	defer layout.Release()
	var metrics struct {
		Left, Top, Width, WidthTrailing, Height, LayoutWidth, LayoutHeight float32
		BidiDepth, Lines                                                   uint32
	}
	layout.Call(layoutGetMetrics, uintptr(unsafe.Pointer(&metrics)))
	return metrics.WidthTrailing, metrics.Height
}

// tailFitting returns the trailing portion of s that fits in r when wrapped,
// prefixed with an ellipsis when truncated.
func tailFitting(s string, r rectF, f textFont) string {
	runes := []rune(s)
	if _, h := measureText(s, f, r.width()); h <= r.height() {
		return s
	}
	lo, hi := 1, len(runes)
	for lo < hi {
		mid := (lo + hi) / 2
		if _, h := measureText("…"+string(runes[mid:]), f, r.width()); h <= r.height() {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return "…" + string(runes[lo:])
}
