package ui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KotoType/KotoType/internal/config"
	"github.com/KotoType/KotoType/internal/desktop"
	"github.com/KotoType/KotoType/internal/provider"
	"github.com/KotoType/KotoType/internal/tip"
)

// The settings window, rendered entirely with Direct2D in Windows 11 Fluent
// style: grouped cards over a Mica backdrop on the left, text service status
// and live recognition test on the right.

const (
	settingsStyle = wsCaption | wsSysMenu | wsMinimizeBox | wsClipChildren
	settingsW     = 780 // Client width in 96-DPI DIPs.

	idTestEdit = 200
	idHideEdit = 201

	ctrlNone         = -1
	ctrlTipInstall   = 100
	ctrlTipUninstall = 101
	ctrlTest         = 102
	ctrlAbout        = 103
	ctrlCfgLoc       = 104
	ctrlCfgDir       = 105
)

type settingsRow struct {
	shown    []config.Choice
	selected int
	toggle   bool // True for binary "开启"/"关闭" fields rendered as a switch.
	onIdx    int
	offIdx   int
	box      rectF // Control hit rectangle in DIPs.
	rowRect  rectF // Full row inside its card.
	enabled  bool
}

type cardGroup struct {
	title  string
	titleY float32
	rect   rectF
	first  int
	count  int
}

var (
	settingsWnd    uintptr
	settingsDPI    int
	settingsMica   bool
	settingsCanvas *canvas
	settingsH      float32

	providers   []provider.Info
	form        []field
	rows        []settingsRow
	cards       []cardGroup
	cfgRowRect  rectF
	cfgLocRect  rectF
	cfgDirRect  rectF
	tipCardRect rectF
	tipTitleY   float32
	tipInstRect rectF
	tipUninRect rectF
	testTitleY  float32
	testCardR   rectF
	testBtnRect rectF
	testStatR   rectF
	testLvlRect rectF
	testBoxRect rectF
	footerY     float32
	aboutRect   rectF

	settingsHover   = ctrlNone
	settingsPressed = ctrlNone

	tipState      tip.Status
	tipBusy       bool
	tipHeadline   string
	tipDetail     string
	tipError      string
	tipInstallLbl string
	tipInstallPri bool // Primary accent style when install or update is needed.
	tipCanInstall bool
	tipCanUninst  bool

	testCancel      context.CancelFunc
	testText        string
	testLevel       float64
	testStatus      = testHint
	testRunning     bool
	testEditWnd     uintptr
	testEditFont    uintptr
	testEditBrush   uintptr
	testEditFocused bool
	hideEditWnd     uintptr
	hideEditFocused bool
	hideEditSyncing bool
	hideFieldIdx    = -1

	// Written by background goroutines, read on the UI thread after a post.
	shared struct {
		sync.Mutex
		choices map[int][]config.Choice
		text    string
		hotkey  string
		level   float64
		status  string
		running bool
		tipFail string
	}
)

const testHint = "点击下方输入框可直接按快捷键听写，或点左侧按钮测试识别。"

func testEditColors() (bg rgba, bgRef, fgRef uintptr) {
	if th.dark {
		return hex(0x252525), 0x00252525, 0x00ffffff
	}
	return hex(0xfafafa), 0x00fafafa, 0x001a1a1a
}

func updateTestEditTheme() {
	if testEditBrush != 0 {
		deleteObject.Call(testEditBrush)
	}
	_, bgRef, _ := testEditColors()
	testEditBrush, _, _ = createSolidBrush.Call(bgRef)
}

func layoutTestEdit() {
	if testEditWnd == 0 {
		return
	}
	oldFont := testEditFont
	// Negative height sets character height in pixels at current DPI.
	h := -int32(math.Round(13.5 * float64(settingsDPI) / 96))
	testEditFont, _, _ = createFont.Call(uintptr(h), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(str("Microsoft YaHei UI"))))
	sendMessage.Call(testEditWnd, wmSetFont, testEditFont, 1)
	if oldFont != 0 {
		deleteObject.Call(oldFont)
	}
	inner := testBoxRect.inset(8)
	scale := func(v float32) uintptr { return uintptr(int32(math.Round(float64(v) * float64(settingsDPI) / 96))) }
	moveWindow.Call(testEditWnd, scale(inner.L), scale(inner.T), scale(inner.width()), scale(inner.height()), 1)
	if hideEditWnd != 0 && hideFieldIdx >= 0 && hideFieldIdx < len(rows) {
		sendMessage.Call(hideEditWnd, wmSetFont, testEditFont, 1)
		b := rows[hideFieldIdx].box
		// Vertically centre the single-line EDIT inside the 30-DIP rounded box.
		const lineH = 18
		top := b.T + (b.height()-lineH)/2
		moveWindow.Call(hideEditWnd, scale(b.L+6), scale(top), scale(b.width()-12), scale(lineH), 1)
	}
}

func syncHideEditText() {
	if hideEditWnd == 0 || hideFieldIdx < 0 || hideFieldIdx >= len(form) {
		return
	}
	v := strings.TrimSpace(form[hideFieldIdx].get(&cfg))
	if v == "" {
		v = form[hideFieldIdx].fallback
	}
	hideEditSyncing = true
	setWindowText.Call(hideEditWnd, uintptr(unsafe.Pointer(str(v))))
	hideEditSyncing = false
}

func buildFormRows() {
	form = settingsFields(opts.Devices, providers, &cfg)
	rows = make([]settingsRow, len(form))
	hideFieldIdx = -1
	for i, f := range form {
		if f.number {
			hideFieldIdx = i
			continue
		}
		if !f.hotkey {
			if len(f.choices) == 2 && f.load == nil {
				if f.choices[0].Name == "开启" && f.choices[1].Name == "关闭" {
					rows[i].toggle, rows[i].onIdx, rows[i].offIdx = true, 0, 1
				} else if f.choices[0].Name == "关闭" && f.choices[1].Name == "开启" {
					rows[i].toggle, rows[i].onIdx, rows[i].offIdx = true, 1, 0
				}
			}
			choices := f.choices
			shared.Lock()
			if cached := shared.choices[i]; len(cached) > 0 && f.load != nil {
				choices = cached
			}
			shared.Unlock()
			fill(i, choices)
		}
	}
	refreshEnabled()
	syncHideEditText()
}

func rebuildSettingsForm() {
	closePopup()
	cancelRecording()
	buildFormRows()
	computeSettingsLayout()
	layoutTestEdit()
	sizeSettingsWindow()
	invalidateRect.Call(settingsWnd, 0, 0)
}

func providersEqual(a, b []provider.Info) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Path != b[i].Path || len(a[i].Fields) != len(b[i].Fields) {
			return false
		}
	}
	return true
}

func createSettings() {
	shared.choices = map[int][]config.Choice{}
	if opts.Providers != nil {
		providers = opts.Providers()
	}
	settingsWnd, _, _ = createWindow.Call(0, uintptr(unsafe.Pointer(str("KotoTypeSettings"))), uintptr(unsafe.Pointer(str("KotoType 设置"))), settingsStyle,
		0x80000000, 0x80000000, 0, 0, 0, 0, instance, 0) // CW_USEDEFAULT
	// WS_EX_LAYERED gives the child EDIT its own opaque DWM surface so GDI's
	// zero alpha bytes do not turn text, caret and selection transparent over Mica.
	testEditWnd, _, _ = createWindow.Call(wsExLayered, uintptr(unsafe.Pointer(str("EDIT"))), 0, wsChild|wsVisible|wsVScroll|0x4|0x40|0x1000, // ES_MULTILINE | ES_AUTOVSCROLL | ES_WANTRETURN
		0, 0, 0, 0, settingsWnd, idTestEdit, instance, 0)
	setLayeredAttr.Call(testEditWnd, 0, 255, 2)                                                                             // LWA_ALPHA = 255 (fully opaque)
	hideEditWnd, _, _ = createWindow.Call(wsExLayered, uintptr(unsafe.Pointer(str("EDIT"))), 0, wsChild|wsVisible|0x1|0x80, // ES_CENTER | ES_AUTOHSCROLL
		0, 0, 0, 0, settingsWnd, idHideEdit, instance, 0)
	setLayeredAttr.Call(hideEditWnd, 0, 255, 2)
	updateTestEditTheme()
	buildFormRows()
	refreshTip()
	dpi, _, _ := getDpiForWindow.Call(settingsWnd)
	settingsDPI = max(int(dpi), 96)
	settingsCanvas = newCanvas(settingsWnd, settingsDPI)
	settingsMica = styleFrame(settingsWnd, true)
	computeSettingsLayout()
	layoutTestEdit()
	sizeSettingsWindow()
}

// computeSettingsLayout lays out every card and control in 96-DPI DIPs.
func computeSettingsLayout() {
	const (
		marginX  = 20
		leftW    = 404
		colGap   = 16
		rightX   = marginX + leftW + colGap
		rightW   = settingsW - marginX - rightX
		rowH     = 44
		titleH   = 20
		titleGap = 4
		cardGap  = 14
	)

	cards = cards[:0]
	y := float32(16)
	for i := 0; i < len(form); {
		title := form[i].section
		start := i
		i++
		for i < len(form) && form[i].section == "" {
			i++
		}
		count := i - start
		cardTop := y + titleH + titleGap
		cardBottom := cardTop + float32(count)*rowH
		cards = append(cards, cardGroup{
			title:  title,
			titleY: y,
			rect:   rectF{marginX, cardTop, marginX + leftW, cardBottom},
			first:  start,
			count:  count,
		})
		for j := 0; j < count; j++ {
			idx := start + j
			ry := cardTop + float32(j)*rowH
			rows[idx].rowRect = rectF{marginX, ry, marginX + leftW, ry + rowH}
			if rows[idx].toggle {
				rows[idx].box = rectF{marginX + leftW - 96, ry + 6, marginX + leftW - 14, ry + rowH - 6}
			} else if form[idx].number {
				rows[idx].box = rectF{marginX + 132, ry + 7, marginX + 132 + 76, ry + rowH - 7}
			} else {
				rows[idx].box = rectF{marginX + 132, ry + 7, marginX + leftW - 14, ry + rowH - 7}
			}
		}
		y = cardBottom + cardGap
	}
	leftBottom := y - cardGap

	// Append the config-location row to the last ("常规") card.
	if n := len(cards); n > 0 {
		ry := cards[n-1].rect.B
		cards[n-1].rect.B += rowH
		cfgRowRect = rectF{marginX, ry, marginX + leftW, ry + rowH}
		const dirBtnW, gap = 88, 6
		cfgDirRect = rectF{marginX + leftW - 14 - dirBtnW, ry + 7, marginX + leftW - 14, ry + rowH - 7}
		cfgLocRect = rectF{marginX + 132, ry + 7, cfgDirRect.L - gap, ry + rowH - 7}
		leftBottom = cards[n-1].rect.B
	}

	// Right column: compact text service card on top, live test card filling
	// the remaining height down to leftBottom.
	tipTitleY = 16
	const tipCardBottom = float32(172)
	tipCardRect = rectF{rightX, tipTitleY + titleH + titleGap, rightX + rightW, tipCardBottom}
	tipInstRect = rectF{rightX + 14, tipCardBottom - 44, rightX + 122, tipCardBottom - 12}
	tipUninRect = rectF{rightX + 130, tipCardBottom - 44, rightX + 226, tipCardBottom - 12}

	testTitleY = tipCardBottom + cardGap
	testTop := testTitleY + titleH + titleGap
	testCardR = rectF{rightX, testTop, rightX + rightW, leftBottom}
	testBtnRect = rectF{rightX + 14, testTop + 14, rightX + 124, testTop + 46}
	testStatR = rectF{rightX + 134, testTop + 11, rightX + rightW - 14, testTop + 49}
	testLvlRect = rectF{rightX + 14, testTop + 58, rightX + rightW - 14, testTop + 64}
	testBoxRect = rectF{rightX + 14, testTop + 76, rightX + rightW - 14, leftBottom - 14}

	footerY = leftBottom + 12
	aboutLabel := "关于 KotoType " + opts.Version
	aboutW, _ := measureText(aboutLabel, textFont{size: 12.5, middle: true}, 400)
	aboutBtnW := float32(math.Ceil(float64(aboutW))) + 34
	aboutRect = rectF{settingsW - marginX - aboutBtnW, footerY, settingsW - marginX, footerY + 32}

	settingsH = footerY + 32 + 14
}

func sizeSettingsWindow() {
	frame := windows.Rect{
		Right:  int32(math.Round(float64(settingsW) * float64(settingsDPI) / 96)),
		Bottom: int32(math.Round(float64(settingsH) * float64(settingsDPI) / 96)),
	}
	adjustWindowRect.Call(uintptr(unsafe.Pointer(&frame)), settingsStyle, 0, 0, uintptr(settingsDPI))
	setWindowPos.Call(settingsWnd, 0, 0, 0, uintptr(frame.Right-frame.Left), uintptr(frame.Bottom-frame.Top), 0x2|0x4|0x10) // NOMOVE | NOZORDER | NOACTIVATE
}

func showSettings() {
	settingsMica = styleFrame(settingsWnd, true)
	if !visible(settingsWnd) {
		fg, _, _ := getForeground.Call()
		monitor, _, _ := monitorFromWindow.Call(fg, 2) // MONITOR_DEFAULTTONEAREST
		var dpi, dpiY uint32
		getDpiForMonitor.Call(monitor, 0, uintptr(unsafe.Pointer(&dpi)), uintptr(unsafe.Pointer(&dpiY)))
		if newDPI := max(int(dpi), 96); newDPI != settingsDPI {
			settingsDPI = newDPI
			settingsCanvas.setDPI(settingsDPI)
			layoutTestEdit()
			sizeSettingsWindow()
		}
		work := workArea(monitor)
		var r windows.Rect
		getWindowRect.Call(settingsWnd, uintptr(unsafe.Pointer(&r)))
		w, h := r.Right-r.Left, r.Bottom-r.Top
		x := max(work.Left, (work.Left+work.Right-w)/2)
		y := max(work.Top, (work.Top+work.Bottom-h)/2)
		setWindowPos.Call(settingsWnd, 0, uintptr(x), uintptr(y), 0, 0, 0x1|0x4) // NOSIZE | NOZORDER
	}
	showWindow.Call(settingsWnd, 1) // SW_SHOWNORMAL
	setForeground.Call(settingsWnd)
	invalidateRect.Call(settingsWnd, 0, 0)
	if testEditWnd != 0 {
		// Paint both client and non-client (scrollbar) areas synchronously to avoid a black first frame.
		redrawWindow.Call(testEditWnd, 0, 0, 0x1|0x4|0x100|0x400) // RDW_INVALIDATE | RDW_ERASE | RDW_UPDATENOW | RDW_FRAME
	}
	if hideEditWnd != 0 {
		redrawWindow.Call(hideEditWnd, 0, 0, 0x1|0x4|0x100)
	}
}

// fill updates field i's choice list and active selection.
func fill(i int, choices []config.Choice) {
	rows[i].shown, rows[i].selected = form[i].options(choices, form[i].get(&cfg))
	if visible(settingsWnd) {
		invalidateRect.Call(settingsWnd, 0, 0)
	}
}

// reloadChoices refetches lists that can change while the app runs.
func reloadChoices() {
	for i, f := range form {
		if f.load == nil {
			continue
		}
		go func() {
			choices, err := f.load()
			shared.Lock()
			shared.choices[i] = choices
			if err != nil && !shared.running {
				shared.status = "读取" + f.label + "列表失败：" + err.Error()
			}
			shared.Unlock()
			postMessage.Call(mainWnd, wmChoices, uintptr(i), 0)
			if err != nil {
				postMessage.Call(mainWnd, wmTestRefresh, 0, 0)
			}
		}()
	}
}

// recording is the index of the hotkey field waiting for a key combination.
var recording = -1

func recordHotkey(i int) {
	if recording >= 0 {
		cancelRecording()
		return
	}
	recording = i
	invalidateRect.Call(settingsWnd, 0, 0)
	desktop.CaptureHotkey(func(value string) {
		shared.Lock()
		shared.hotkey = value
		shared.Unlock()
		postMessage.Call(mainWnd, wmHotkey, 0, 0)
	})
}

func cancelRecording() {
	if recording >= 0 {
		desktop.CaptureHotkey(nil)
		shared.Lock()
		shared.hotkey = ""
		shared.Unlock()
		hotkeyRecorded()
	}
}

func hotkeyRecorded() {
	if recording < 0 {
		return
	}
	i := recording
	recording = -1
	shared.Lock()
	value := shared.hotkey
	shared.Unlock()
	if value != "" {
		form[i].set(&cfg, value)
		changed()
	}
	invalidateRect.Call(settingsWnd, 0, 0)
}

func changed() {
	refreshEnabled()
	opts.OnChange(cfg)
	tray(1) // NIM_MODIFY
	invalidateRect.Call(barWnd, 0, 0)
	invalidateRect.Call(settingsWnd, 0, 0)
}

func choicesLoaded(i int) {
	shared.Lock()
	choices := shared.choices[i]
	shared.Unlock()
	// Avoid closing a drop-down the user is currently browsing.
	if visible(popupWnd) && popupTag == i {
		return
	}
	fill(i, choices)
}

func refreshEnabled() {
	for i, f := range form {
		rows[i].enabled = f.enabled == nil || f.enabled(&cfg)
	}
}

// refreshTip updates the text-service status card.
func refreshTip() {
	s := tip.Check()
	tipState = s
	shared.Lock()
	failure := shared.tipFail
	shared.Unlock()

	tipError = ""
	if failure != "" && !tipBusy {
		tipError = "操作未完成：" + failure
	}
	switch {
	case tipBusy:
		tipHeadline = "正在处理…"
		tipDetail = "请在 Windows 弹出的授权窗口中确认。"
	case !s.Supported:
		tipHeadline = "不可用"
		tipDetail = "这个版本没有附带输入法服务。"
	case !s.Installed:
		tipHeadline = "未安装"
		tipDetail = "安装后即可直接向任意输入框写入文字，无需切换输入法。"
	case !s.Current:
		tipHeadline = "需要更新"
		tipDetail = fmt.Sprintf("检测到旧版服务，正在 %d 个线程中运行。", s.Active)
	default:
		tipHeadline = "已安装"
		tipDetail = fmt.Sprintf("正在 %d 个线程中运行，可直接语音输入。", s.Active)
	}

	tipInstallLbl = "安装"
	tipInstallPri = true
	if s.Current {
		tipInstallLbl = "重新安装"
		tipInstallPri = false
	} else if s.Installed {
		tipInstallLbl = "更新"
		tipInstallPri = true
	}
	tipCanInstall = s.Supported && !tipBusy
	tipCanUninst = s.Installed && !tipBusy
	if visible(settingsWnd) {
		invalidateRect.Call(settingsWnd, 0, 0)
	}
}

func runTip(arg string) {
	tipBusy = true
	shared.Lock()
	shared.tipFail = ""
	shared.Unlock()
	refreshTip()
	go func() {
		err := tip.Elevate(arg)
		shared.Lock()
		if err != nil {
			shared.tipFail = err.Error()
		}
		shared.Unlock()
		postMessage.Call(mainWnd, wmTipDone, 0, 0)
	}()
}

func startTest() {
	ctx, cancel := context.WithCancel(context.Background())
	testCancel = cancel
	shared.Lock()
	shared.text, shared.level, shared.status, shared.running = "", 0, "正在连接…", true
	shared.Unlock()
	refreshTest()
	post := func(change func()) {
		shared.Lock()
		change()
		shared.Unlock()
		postMessage.Call(mainWnd, wmTestRefresh, 0, 0)
	}
	settings := cfg
	go func() {
		err := opts.Test(ctx, settings, func(text string) {
			post(func() { shared.text = text })
		}, func(level float64) {
			post(func() { shared.level, shared.status = level, "正在聆听，请说话…" })
		})
		post(func() {
			shared.running, shared.level, shared.status = false, 0, "测试结束。"+testHint
			if err != nil {
				shared.status = "测试失败：" + err.Error()
			}
		})
	}()
}

func stopTest() {
	if testCancel != nil {
		testCancel()
		testCancel = nil
	}
}

func refreshTest() {
	shared.Lock()
	text, level, status, running := shared.text, shared.level, shared.status, shared.running
	shared.Unlock()
	if !running {
		stopTest()
	}
	textChanged := text != testText
	testText, testLevel, testStatus, testRunning = text, level, status, running
	if textChanged && testEditWnd != 0 {
		setWindowText.Call(testEditWnd, uintptr(unsafe.Pointer(str(testText))))
		sendMessage.Call(testEditWnd, 0xb1, ^uintptr(0), ^uintptr(0)) // EM_SETSEL to the end
		sendMessage.Call(testEditWnd, 0xb7, 0, 0)                     // EM_SCROLLCARET
	}
	if visible(settingsWnd) {
		invalidateRect.Call(settingsWnd, 0, 0)
	}
}

func settingsHit(xPx, yPx int32) int {
	x, y := settingsCanvas.pixels(xPx), settingsCanvas.pixels(yPx)
	for i := range rows {
		if !form[i].number && rows[i].enabled && rows[i].box.contains(x, y) {
			return i
		}
	}
	switch {
	case cfgLocRect.contains(x, y):
		return ctrlCfgLoc
	case cfgDirRect.contains(x, y):
		return ctrlCfgDir
	case tipCanInstall && tipInstRect.contains(x, y):
		return ctrlTipInstall
	case tipCanUninst && tipUninRect.contains(x, y):
		return ctrlTipUninstall
	case testBtnRect.contains(x, y):
		return ctrlTest
	case aboutRect.contains(x, y):
		return ctrlAbout
	}
	return ctrlNone
}

func openConfigLocationPopup() {
	locs := []struct {
		value string
		label string
	}{
		{config.LocationAppData, "系统目录 (%AppData%)"},
		{config.LocationPortable, "程序同目录"},
	}
	cur := config.CurrentLocation()
	selected := 0
	items := make([]popupItem, len(locs))
	for i, l := range locs {
		items[i] = popupItem{label: l.label}
		if l.value == cur {
			selected = i
		}
	}
	scale := func(v float32) int32 { return int32(math.Round(float64(v) * float64(settingsDPI) / 96)) }
	tl := point{X: scale(cfgLocRect.L), Y: scale(cfgLocRect.T)}
	br := point{X: scale(cfgLocRect.R), Y: scale(cfgLocRect.B)}
	clientToScreen.Call(settingsWnd, uintptr(unsafe.Pointer(&tl)))
	clientToScreen.Call(settingsWnd, uintptr(unsafe.Pointer(&br)))
	anchor := windows.Rect{Left: tl.X, Top: tl.Y, Right: br.X, Bottom: br.Y}
	showPopup(settingsWnd, anchor, cfgLocRect.width(), settingsDPI, items, selected, ctrlCfgLoc, func(sel int) {
		if sel < 0 || sel >= len(locs) {
			return
		}
		target := locs[sel].value
		if target == config.CurrentLocation() {
			return
		}
		err := config.Move(target, cfg, false)
		if errors.Is(err, config.ErrExists) {
			dst, _ := config.PathFor(target)
			msg := "目标位置已存在配置文件：\n\n" + dst + "\n\n是否用当前配置覆盖它？"
			ans, _, _ := messageBox.Call(settingsWnd, uintptr(unsafe.Pointer(str(msg))), uintptr(unsafe.Pointer(str("移动配置文件"))), 0x4|0x20) // MB_YESNO | MB_ICONQUESTION
			if ans != 6 {                                                                                                                  // IDYES
				return
			}
			err = config.Move(target, cfg, true)
		}
		if err != nil {
			messageBox.Call(settingsWnd, uintptr(unsafe.Pointer(str("移动配置文件失败："+err.Error()))), uintptr(unsafe.Pointer(str("KotoType"))), 0x10) // MB_ICONERROR
			return
		}
		invalidateRect.Call(settingsWnd, 0, 0)
	})
	invalidateRect.Call(settingsWnd, 0, 0)
}

func openConfigDir() {
	p, err := config.FilePath()
	if err != nil {
		return
	}
	if _, err := os.Stat(p); err == nil {
		arg := `/select,"` + p + `"`
		shellExecute.Call(0, uintptr(unsafe.Pointer(str("open"))), uintptr(unsafe.Pointer(str("explorer.exe"))), uintptr(unsafe.Pointer(str(arg))), 0, 1)
		return
	}
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0o755)
	shellExecute.Call(0, uintptr(unsafe.Pointer(str("open"))), uintptr(unsafe.Pointer(str(dir))), 0, 0, 1)
}

func openFieldPopup(i int) {
	items := make([]popupItem, len(rows[i].shown))
	for idx, c := range rows[i].shown {
		items[idx] = popupItem{label: c.Name}
	}
	b := rows[i].box
	scale := func(v float32) int32 { return int32(math.Round(float64(v) * float64(settingsDPI) / 96)) }
	tl := point{X: scale(b.L), Y: scale(b.T)}
	br := point{X: scale(b.R), Y: scale(b.B)}
	clientToScreen.Call(settingsWnd, uintptr(unsafe.Pointer(&tl)))
	clientToScreen.Call(settingsWnd, uintptr(unsafe.Pointer(&br)))
	anchor := windows.Rect{Left: tl.X, Top: tl.Y, Right: br.X, Bottom: br.Y}
	showPopup(settingsWnd, anchor, b.width(), settingsDPI, items, rows[i].selected, i, func(sel int) {
		if sel >= 0 && sel < len(rows[i].shown) {
			rows[i].selected = sel
			form[i].set(&cfg, rows[i].shown[sel].Value)
			rebuilds := form[i].rebuilds
			changed()
			if rebuilds {
				rebuildSettingsForm()
			}
		}
	})
	invalidateRect.Call(settingsWnd, 0, 0)
}

func activateControl(id int) {
	switch {
	case id >= 0 && id < len(form):
		if form[id].hotkey {
			recordHotkey(id)
			return
		}
		if rows[id].toggle {
			next := rows[id].onIdx
			if rows[id].selected == rows[id].onIdx {
				next = rows[id].offIdx
			}
			rows[id].selected = next
			form[id].set(&cfg, rows[id].shown[next].Value)
			changed()
			return
		}
		openFieldPopup(id)
	case id == ctrlCfgLoc:
		openConfigLocationPopup()
	case id == ctrlCfgDir:
		openConfigDir()
	case id == ctrlTipInstall:
		runTip(tip.InstallArg)
	case id == ctrlTipUninstall:
		runTip(tip.UninstallArg)
	case id == ctrlTest:
		if testCancel != nil {
			stopTest()
		} else {
			startTest()
		}
	case id == ctrlAbout:
		showAbout()
	}
}

func drawButton(c *canvas, r rectF, label, icon string, primary, enabled, hovered, pressed bool) {
	var fill, border, fg rgba
	switch {
	case !enabled:
		fill, border, fg = th.control.alpha(th.control.A*0.5), th.border, th.disabled
	case primary:
		fill, fg = th.accent, th.onAccent
		if pressed && hovered {
			fill = th.accentPressed
		} else if hovered {
			fill = th.accentHover
		}
	default:
		fill, border, fg = th.control, th.controlBorder, th.text
		if pressed && hovered {
			fill, border = th.pressed, th.controlHoverBorder
		} else if hovered {
			fill, border = th.hover, th.controlHoverBorder
		}
	}
	c.fill(r, 4, fill)
	if !primary || !enabled {
		c.stroke(r.inset(0.5), 3.5, 1, border)
	}
	f := textFont{size: 13.5, align: 2, middle: true}
	if icon == "" {
		c.text(label, r, f, fg)
		return
	}
	tw, _ := measureText(label, f, r.width())
	const iconW, gap = 14, 6
	startX := r.L + (r.width()-(iconW+gap+tw))/2
	c.text(icon, rectF{startX, r.T, startX + iconW, r.B}, textFont{size: 12.5, icon: true, align: 2, middle: true}, fg)
	c.text(label, rectF{startX + iconW + gap, r.T, r.R, r.B}, textFont{size: 13.5, middle: true}, fg)
}

func drawToggle(c *canvas, r rectF, on, hovered, pressed bool) {
	track := rectF{r.L, r.T + (r.height()-20)/2, r.L + 40, r.T + (r.height()+20)/2}
	cy := (track.T + track.B) / 2
	if on {
		fill := th.accent
		if pressed && hovered {
			fill = th.accentPressed
		} else if hovered {
			fill = th.accentHover
		}
		c.fill(track, 10, fill)
		c.circle(track.R-10, cy, 6, th.onAccent)
		c.text("开启", rectF{track.R + 8, r.T, r.R, r.B}, textFont{size: 13, align: 1, middle: true}, th.text)
	} else {
		fill := th.control.alpha(th.control.A * 0.4)
		if pressed && hovered {
			fill = th.itemPressed
		} else if hovered {
			fill = th.itemHover
		}
		c.fill(track, 10, fill)
		c.stroke(track.inset(0.5), 9.5, 1, th.subtle)
		c.circle(track.L+10, cy, 6, th.subtle)
		c.text("关闭", rectF{track.R + 8, r.T, r.R, r.B}, textFont{size: 13, align: 1, middle: true}, th.subtle)
	}
}

func paintSettings() {
	c := settingsCanvas
	if !c.begin() {
		return
	}
	if settingsMica {
		c.clear(rgba{})
	} else {
		c.clear(th.window)
	}

	sectionFont := textFont{size: 13.5, bold: true, middle: true}
	labelFont := textFont{size: 13.5, middle: true}

	// Left column: grouped setting cards.
	for gi, g := range cards {
		c.text(g.title, rectF{g.rect.L + 2, g.titleY, g.rect.R, g.titleY + 20}, sectionFont, th.text)
		c.fill(g.rect, 8, th.card)
		c.stroke(g.rect.inset(0.5), 7.5, 1, th.border)

		for j := 0; j < g.count; j++ {
			i := g.first + j
			r := rows[i]
			if j > 0 {
				c.line(g.rect.L+14, r.rowRect.T+0.5, g.rect.R-14, r.rowRect.T+0.5, 1, th.divider)
			}
			lblCol := th.text
			if !r.enabled {
				lblCol = th.disabled
			}
			c.text(form[i].label, rectF{r.rowRect.L + 14, r.rowRect.T, r.rowRect.L + 126, r.rowRect.B}, labelFont, lblCol)

			hovered := settingsHover == i
			pressed := settingsPressed == i
			switch {
			case form[i].number:
				editBg, _, _ := testEditColors()
				c.fill(r.box, 4, editBg)
				if hideEditFocused {
					c.stroke(r.box.inset(0.75), 3.5, 1.5, th.accent)
				} else {
					c.stroke(r.box.inset(0.5), 3.5, 1, th.controlBorder)
				}
				c.text(form[i].unit, rectF{r.box.R + 10, r.rowRect.T, g.rect.R - 14, r.rowRect.B}, textFont{size: 12.5, middle: true}, th.subtle)
			case r.toggle:
				drawToggle(c, r.box, r.selected == r.onIdx, hovered, pressed)
			case form[i].hotkey:
				isRec := recording == i
				if isRec {
					c.fill(r.box, 4, th.accent.alpha(0.10))
					c.stroke(r.box.inset(0.75), 3.5, 1.5, th.accent)
					c.text("请按下新的快捷键（Esc 取消）", r.box.inset(4), textFont{size: 12.5, align: 2, middle: true}, th.accent)
				} else {
					drawButton(c, r.box, desktop.HotkeyName(form[i].get(&cfg)), "", false, r.enabled, hovered, pressed)
				}
			default:
				open := visible(popupWnd) && popupTag == i
				fill, border, fg, chev := th.control, th.controlBorder, th.text, th.subtle
				switch {
				case !r.enabled:
					fill, border, fg, chev = th.control.alpha(th.control.A*0.5), th.border, th.disabled, th.disabled
				case open:
					fill, border = th.hover, th.accent
				case pressed && hovered:
					fill, border = th.pressed, th.controlHoverBorder
				case hovered:
					fill, border = th.hover, th.controlHoverBorder
				}
				c.fill(r.box, 4, fill)
				c.stroke(r.box.inset(0.5), 3.5, 1, border)
				val := ""
				if r.selected >= 0 && r.selected < len(r.shown) {
					val = r.shown[r.selected].Name
				}
				c.text(val, rectF{r.box.L + 10, r.box.T, r.box.R - 26, r.box.B}, textFont{size: 13.5, middle: true}, fg)
				c.text("\ue70d", rectF{r.box.R - 24, r.box.T, r.box.R - 6, r.box.B}, textFont{size: 10, icon: true, align: 2, middle: true}, chev)
			}
		}

		// Config location selector + open-folder button at the bottom of the last card.
		if gi == len(cards)-1 {
			c.line(g.rect.L+14, cfgRowRect.T+0.5, g.rect.R-14, cfgRowRect.T+0.5, 1, th.divider)
			c.text("配置位置", rectF{cfgRowRect.L + 14, cfgRowRect.T, cfgRowRect.L + 126, cfgRowRect.B}, labelFont, th.text)

			open := visible(popupWnd) && popupTag == ctrlCfgLoc
			fill, border := th.control, th.controlBorder
			switch {
			case open:
				fill, border = th.hover, th.accent
			case settingsPressed == ctrlCfgLoc && settingsHover == ctrlCfgLoc:
				fill, border = th.pressed, th.controlHoverBorder
			case settingsHover == ctrlCfgLoc:
				fill, border = th.hover, th.controlHoverBorder
			}
			c.fill(cfgLocRect, 4, fill)
			c.stroke(cfgLocRect.inset(0.5), 3.5, 1, border)
			locName := "系统目录 (%AppData%)"
			if config.CurrentLocation() == config.LocationPortable {
				locName = "程序同目录"
			}
			c.text(locName, rectF{cfgLocRect.L + 10, cfgLocRect.T, cfgLocRect.R - 24, cfgLocRect.B}, textFont{size: 13, middle: true}, th.text)
			c.text("\ue70d", rectF{cfgLocRect.R - 22, cfgLocRect.T, cfgLocRect.R - 4, cfgLocRect.B}, textFont{size: 10, icon: true, align: 2, middle: true}, th.subtle)

			drawButton(c, cfgDirRect, "打开目录", "\ue838", false, true, settingsHover == ctrlCfgDir, settingsPressed == ctrlCfgDir)
		}
	}

	// Right top card: TSF text service status and actions.
	c.text("输入法服务", rectF{tipCardRect.L + 2, tipTitleY, tipCardRect.R, tipTitleY + 20}, sectionFont, th.text)
	c.fill(tipCardRect, 8, th.card)
	c.stroke(tipCardRect.inset(0.5), 7.5, 1, th.border)

	dotCol := th.disabled
	switch {
	case tipBusy || (tipState.Installed && !tipState.Current):
		dotCol = th.warn
	case tipState.Current:
		dotCol = th.ok
	}
	c.circle(tipCardRect.L+19, tipCardRect.T+22, 4, dotCol)
	c.text(tipHeadline, rectF{tipCardRect.L + 29, tipCardRect.T + 12, tipCardRect.R - 14, tipCardRect.T + 32}, textFont{size: 13.5, bold: true, middle: true}, th.text)

	detailRect := rectF{tipCardRect.L + 14, tipCardRect.T + 36, tipCardRect.R - 14, tipInstRect.T - 8}
	if tipError != "" {
		c.text(tipError, detailRect, textFont{size: 12, wrap: true, clipped: true}, th.danger)
	} else {
		c.text(tipDetail, detailRect, textFont{size: 12.5, wrap: true, clipped: true}, th.subtle)
	}
	drawButton(c, tipInstRect, tipInstallLbl, "", tipInstallPri, tipCanInstall, settingsHover == ctrlTipInstall, settingsPressed == ctrlTipInstall)
	drawButton(c, tipUninRect, "卸载", "", false, tipCanUninst, settingsHover == ctrlTipUninstall, settingsPressed == ctrlTipUninstall)

	// Right bottom card: live recognition test.
	c.text("实时测试", rectF{testCardR.L + 2, testTitleY, testCardR.R, testTitleY + 20}, sectionFont, th.text)
	c.fill(testCardR, 8, th.card)
	c.stroke(testCardR.inset(0.5), 7.5, 1, th.border)

	testLbl, testIcon := "开始测试", "\ue720"
	if testRunning {
		testLbl, testIcon = "停止测试", "\ue71a"
	}
	drawButton(c, testBtnRect, testLbl, testIcon, true, true, settingsHover == ctrlTest, settingsPressed == ctrlTest)
	c.text(testStatus, testStatR, textFont{size: 12, wrap: true, clipped: true}, th.subtle)

	// Smooth rounded audio level bar.
	c.fill(testLvlRect, 3, th.divider)
	if lvl := float32(max(0, min(testLevel, 1))); lvl > 0.01 {
		fillW := max(6, testLvlRect.width()*lvl)
		c.fill(rectF{testLvlRect.L, testLvlRect.T, testLvlRect.L + fillW, testLvlRect.B}, 3, th.accent)
	}

	// Rounded frame around the native editable test EDIT control.
	editBg, _, _ := testEditColors()
	c.fill(testBoxRect, 6, editBg)
	if testEditFocused {
		c.stroke(testBoxRect.inset(0.75), 5.5, 1.5, th.accent)
	} else {
		c.stroke(testBoxRect.inset(0.5), 5.5, 1, th.controlBorder)
	}

	// Footer: unclipped note on the left, dynamically sized About link on the right.
	c.text("更改立即保存，从下一次听写开始生效。", rectF{20, footerY, aboutRect.L - 12, footerY + 32}, textFont{size: 12.5, middle: true}, th.subtle)
	if settingsHover == ctrlAbout {
		bg := th.itemHover
		if settingsPressed == ctrlAbout {
			bg = th.itemPressed
		}
		c.fill(aboutRect, 4, bg)
	}
	c.text("\ue946", rectF{aboutRect.L + 8, aboutRect.T, aboutRect.L + 24, aboutRect.B}, textFont{size: 13, icon: true, align: 2, middle: true}, th.accent)
	c.text("关于 KotoType "+opts.Version, rectF{aboutRect.L + 26, aboutRect.T, aboutRect.R - 8, aboutRect.B}, textFont{size: 12.5, middle: true}, th.accent)

	c.end()
}

func settingsProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmActivate:
		if wParam&0xffff != 0 {
			if opts.Providers != nil {
				if latest := opts.Providers(); !providersEqual(latest, providers) {
					providers = latest
					rebuildSettingsForm()
				}
			}
			reloadChoices()
			refreshTip()
		} else {
			cancelRecording()
			closePopup()
		}
		return 0
	case wmMove, wmNCLButtonDown, wmRButtonDown:
		closePopup()
	case wmCommand:
		id, code := int(wParam&0xffff), wParam>>16
		if id == idTestEdit {
			switch code {
			case 0x0100: // EN_SETFOCUS
				testEditFocused = true
				invalidateRect.Call(hwnd, 0, 0)
			case 0x0200: // EN_KILLFOCUS
				testEditFocused = false
				invalidateRect.Call(hwnd, 0, 0)
			}
			return 0
		}
		if id == idHideEdit && hideFieldIdx >= 0 && hideFieldIdx < len(form) {
			switch code {
			case 0x0100: // EN_SETFOCUS
				hideEditFocused = true
				invalidateRect.Call(hwnd, 0, 0)
			case 0x0200: // EN_KILLFOCUS
				hideEditFocused = false
				syncHideEditText()
				invalidateRect.Call(hwnd, 0, 0)
			case 0x0300: // EN_CHANGE
				if hideEditSyncing {
					return 0
				}
				var buf [32]uint16
				n, _, _ := getWindowText.Call(hideEditWnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
				raw := strings.TrimSpace(windows.UTF16ToString(buf[:n]))
				if raw == "" {
					form[hideFieldIdx].set(&cfg, "")
					changed()
				} else if sec, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(sec) && !math.IsInf(sec, 0) && sec >= 0 && sec <= 3600 {
					form[hideFieldIdx].set(&cfg, raw)
					changed()
				}
			}
			return 0
		}
	case wmCtlColorEdit:
		if lParam == testEditWnd || lParam == hideEditWnd {
			_, bgRef, fgRef := testEditColors()
			setTextColor.Call(wParam, fgRef)
			setBkColor.Call(wParam, bgRef)
			return testEditBrush
		}
	case wmDPIChanged:
		closePopup()
		settingsDPI = max(int(hiword(wParam)), 96)
		settingsCanvas.setDPI(settingsDPI)
		layoutTestEdit()
		sizeSettingsWindow()
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmMouseMove:
		if hit := settingsHit(loword(lParam), hiword(lParam)); hit != settingsHover {
			settingsHover = hit
			invalidateRect.Call(hwnd, 0, 0)
			trackMouseLeave(hwnd)
		}
		return 0
	case wmMouseLeave:
		if settingsHover != ctrlNone || settingsPressed != ctrlNone {
			settingsHover = ctrlNone
			settingsPressed = ctrlNone
			invalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmLButtonDown:
		setFocus.Call(hwnd)
		hit := settingsHit(loword(lParam), hiword(lParam))
		if visible(popupWnd) {
			same := hit == popupTag
			closePopup()
			if same {
				return 0
			}
		}
		if recording >= 0 && hit != recording {
			cancelRecording()
		}
		settingsPressed = hit
		invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmLButtonUp:
		hit := settingsHit(loword(lParam), hiword(lParam))
		pressed := settingsPressed
		settingsPressed = ctrlNone
		invalidateRect.Call(hwnd, 0, 0)
		if hit != ctrlNone && hit == pressed {
			activateControl(hit)
		}
		return 0
	case wmMouseWheel:
		if visible(popupWnd) {
			popupScrollWheel(wParam)
			return 0
		}
		return 0
	case wmKeyDown:
		if visible(popupWnd) && popupHandleKey(wParam) {
			return 0
		}
		if wParam == 0x1b { // VK_ESCAPE
			cancelRecording()
			closePopup()
		}
		return 0
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		var ps paintStruct
		beginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		paintSettings()
		endPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmClose:
		cancelRecording()
		closePopup()
		stopTest()
		showWindow.Call(hwnd, swHide)
		settingsCanvas.release()
		trimIdleMemory()
		return 0
	}
	return defProc(hwnd, message, wParam, lParam)
}
