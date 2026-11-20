package ui

import (
	"errors"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KotoType/KotoType/internal/config"
	"github.com/KotoType/KotoType/internal/desktop"
	"github.com/KotoType/KotoType/internal/session"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	registerClass      = user32.NewProc("RegisterClassExW")
	createWindow       = user32.NewProc("CreateWindowExW")
	defWindowProc      = user32.NewProc("DefWindowProcW")
	destroyWindow      = user32.NewProc("DestroyWindow")
	showWindow         = user32.NewProc("ShowWindow")
	isWindowVisible    = user32.NewProc("IsWindowVisible")
	setWindowPos       = user32.NewProc("SetWindowPos")
	moveWindow         = user32.NewProc("MoveWindow")
	getWindowRect      = user32.NewProc("GetWindowRect")
	findWindow         = user32.NewProc("FindWindowW")
	getWindowProcess   = user32.NewProc("GetWindowThreadProcessId")
	allowSetForeground = user32.NewProc("AllowSetForegroundWindow")
	getMessage         = user32.NewProc("GetMessageW")
	translateMessage   = user32.NewProc("TranslateMessage")
	dispatchMessage    = user32.NewProc("DispatchMessageW")
	postMessage        = user32.NewProc("PostMessageW")
	sendMessage        = user32.NewProc("SendMessageW")
	postQuitMessage    = user32.NewProc("PostQuitMessage")
	registerMessage    = user32.NewProc("RegisterWindowMessageW")
	messageBox         = user32.NewProc("MessageBoxW")
	invalidateRect     = user32.NewProc("InvalidateRect")
	redrawWindow       = user32.NewProc("RedrawWindow")
	beginPaint         = user32.NewProc("BeginPaint")
	endPaint           = user32.NewProc("EndPaint")
	getClientRect      = user32.NewProc("GetClientRect")
	clientToScreen     = user32.NewProc("ClientToScreen")
	loadCursor         = user32.NewProc("LoadCursorW")
	setCursor          = user32.NewProc("SetCursor")
	setCapture         = user32.NewProc("SetCapture")
	releaseCapture     = user32.NewProc("ReleaseCapture")
	setFocus           = user32.NewProc("SetFocus")
	setWindowText      = user32.NewProc("SetWindowTextW")
	setLayeredAttr     = user32.NewProc("SetLayeredWindowAttributes")
	loadIcon           = user32.NewProc("LoadIconW")
	loadImage          = user32.NewProc("LoadImageW")
	getSystemMetrics   = user32.NewProc("GetSystemMetrics")
	setTimer           = user32.NewProc("SetTimer")
	killTimer          = user32.NewProc("KillTimer")
	setForeground      = user32.NewProc("SetForegroundWindow")
	getForeground      = user32.NewProc("GetForegroundWindow")
	getCursorPos       = user32.NewProc("GetCursorPos")
	trackMouseEvent    = user32.NewProc("TrackMouseEvent")
	monitorFromPoint   = user32.NewProc("MonitorFromPoint")
	monitorFromWindow  = user32.NewProc("MonitorFromWindow")
	getMonitorInfo     = user32.NewProc("GetMonitorInfoW")
	getDpiForWindow    = user32.NewProc("GetDpiForWindow")
	adjustWindowRect   = user32.NewProc("AdjustWindowRectExForDpi")
	setWinEventHook    = user32.NewProc("SetWinEventHook")
	getClassName       = user32.NewProc("GetClassNameW")
	getWindowText      = user32.NewProc("GetWindowTextW")
	getDpiForMonitor   = shcore.NewProc("GetDpiForMonitor")
	createFont         = gdi32.NewProc("CreateFontW")
	deleteObject       = gdi32.NewProc("DeleteObject")
	createSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	setTextColor       = gdi32.NewProc("SetTextColor")
	setBkColor         = gdi32.NewProc("SetBkColor")
	notifyIcon         = shell32.NewProc("Shell_NotifyIconW")
	shellExecute       = shell32.NewProc("ShellExecuteW")
	getModuleHandle    = kernel32.NewProc("GetModuleHandleW")
	createMutex        = kernel32.NewProc("CreateMutexW")
	lstrcmpi           = kernel32.NewProc("lstrcmpiW")
	setProcessWorkSet  = kernel32.NewProc("SetProcessWorkingSetSize")
	dwmSetAttribute    = dwmapi.NewProc("DwmSetWindowAttribute")
	dwmExtendFrame     = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
)

const (
	wmDestroy        = 0x0002
	wmMove           = 0x0003
	wmActivate       = 0x0006
	wmPaint          = 0x000f
	wmClose          = 0x0010
	wmEraseBkgnd     = 0x0014
	wmSettingChange  = 0x001a
	wmSetCursor      = 0x0020
	wmMouseActivate  = 0x0021
	wmSetFont        = 0x0030
	wmNCHitTest      = 0x0084
	wmNCLButtonDown  = 0x00a1
	wmKeyDown        = 0x0100
	wmCommand        = 0x0111
	wmTimer          = 0x0113
	wmCtlColorEdit   = 0x0133
	wmMouseMove      = 0x0200
	wmLButtonDown    = 0x0201
	wmLButtonUp      = 0x0202
	wmRButtonDown    = 0x0204
	wmRButtonUp      = 0x0205
	wmMouseWheel     = 0x020a
	wmCaptureChanged = 0x0215
	wmExitSizeMove   = 0x0232
	wmMouseLeave     = 0x02a3
	wmDPIChanged     = 0x02e0
	wmApp            = 0x8000
	wmBarRefresh     = wmApp + 1 // A new session.State is waiting in latest.
	wmTestRefresh    = wmApp + 2 // The settings test progressed.
	wmChoices        = wmApp + 3 // wParam: index of the field whose choices loaded.
	wmShowSettings   = wmApp + 4 // Also posted by a second instance.
	wmHotkey         = wmApp + 5 // A recorded hotkey is waiting in shared.
	wmTipDone        = wmApp + 6 // An elevated text service install or uninstall finished.
	wmTray           = wmApp + 10

	wsPopup        = 0x80000000
	wsChild        = 0x40000000
	wsVisible      = 0x10000000
	wsClipChildren = 0x02000000
	wsVScroll      = 0x00200000
	wsCaption      = 0x00c00000
	wsSysMenu      = 0x00080000
	wsMinimizeBox  = 0x00020000
	wsExTopmost    = 0x00000008
	wsExToolWin    = 0x00000080
	wsExLayered    = 0x00080000
	wsExNoActive   = 0x08000000

	swHide         = 0
	swShowNoActive = 4
)

type point struct{ X, Y int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

type paintStruct struct {
	HDC       uintptr
	Erase     int32
	Paint     windows.Rect
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

type wndClass struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

var (
	opts      Options
	cfg       config.Config // The UI thread's copy of the settings.
	instance  uintptr
	mainWnd   uintptr // Hidden; owns the tray icon and receives posted work.
	iconSmall uintptr
	iconBig   uintptr
	stateMu   sync.Mutex
	latest    session.State
	taskbarUp uintptr // "TaskbarCreated": Explorer restarted, re-add the tray icon.
)

func str(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

func loword(v uintptr) int32 { return int32(int16(v)) }
func hiword(v uintptr) int32 { return int32(int16(v >> 16)) }

func trackMouseLeave(hwnd uintptr) {
	track := struct {
		Size, Flags uint32
		Wnd         uintptr
		Hover       uint32
	}{Flags: 2, Wnd: hwnd} // TME_LEAVE
	track.Size = uint32(unsafe.Sizeof(track))
	trackMouseEvent.Call(uintptr(unsafe.Pointer(&track)))
}

func register(name string, proc func(hwnd, message, wParam, lParam uintptr) uintptr) {
	cursor, _, _ := loadCursor.Call(0, 32512) // IDC_ARROW
	class := wndClass{WndProc: syscall.NewCallback(proc), Instance: instance, Cursor: cursor, Icon: iconBig, IconSm: iconSmall, ClassName: str(name)}
	class.Size = uint32(unsafe.Sizeof(class))
	registerClass.Call(uintptr(unsafe.Pointer(&class)))
}

func defProc(hwnd, message, wParam, lParam uintptr) uintptr {
	r, _, _ := defWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

func visible(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := isWindowVisible.Call(hwnd)
	return r != 0
}

// workArea is the usable rectangle of a monitor.
func workArea(monitor uintptr) windows.Rect {
	info := struct {
		Size          uint32
		Monitor, Work windows.Rect
		Flags         uint32
	}{}
	info.Size = uint32(unsafe.Sizeof(info))
	getMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&info)))
	return info.Work
}

// appIcon loads the executable's icon at a system metric size (SM_CXICON or
// SM_CXSMICON), falling back to the stock one for builds without resources.
func appIcon(metric uintptr) uintptr {
	size, _, _ := getSystemMetrics.Call(metric)
	icon, _, _ := loadImage.Call(instance, 1, 1, size, size, 0) // Resource 1, IMAGE_ICON
	if icon == 0 {
		icon, _, _ = loadIcon.Call(0, 32512) // IDI_APPLICATION
	}
	return icon
}

// trimIdleMemory returns unused heap and working-set pages once every visible
// window has been dismissed.
func trimIdleMemory() {
	if visible(settingsWnd) || visible(aboutWnd) || visible(barWnd) || visible(popupWnd) {
		return
	}
	debug.FreeOSMemory()
	setProcessWorkSet.Call(uintptr(windows.CurrentProcess()), ^uintptr(0), ^uintptr(0))
}

func acquireInstanceMutex() (windows.Handle, bool) {
	h, _, err := createMutex.Call(0, 0, uintptr(unsafe.Pointer(str("Local\\KotoType.SingleInstance"))))
	if h == 0 {
		return 0, false
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(windows.Handle(h))
		return 0, false
	}
	return windows.Handle(h), true
}

func sameRunningExe(hwnd uintptr, pid uint32, version string) bool {
	var titleBuf [128]uint16
	n, _, _ := getWindowText.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
	if windows.UTF16ToString(titleBuf[:n]) != "KotoType "+version {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var buf [windows.MAX_LONG_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return false
	}
	return strings.EqualFold(windows.UTF16ToString(buf[:size]), self)
}

// SingleInstance reports whether this process should continue starting. If the
// exact same executable and version is already running, its settings window is
// brought to the front; if a different version or path is running, the older
// instance is asked to exit so this copy can take over cleanly.
func SingleInstance(version string) bool {
	if _, ok := acquireInstanceMutex(); ok {
		return true
	}
	for attempt := 0; attempt < 20; attempt++ {
		running, _, _ := findWindow.Call(uintptr(unsafe.Pointer(str("KotoTypeMain"))), 0)
		if running == 0 {
			time.Sleep(50 * time.Millisecond)
			if _, ok := acquireInstanceMutex(); ok {
				return true
			}
			continue
		}
		var pid uint32
		getWindowProcess.Call(running, uintptr(unsafe.Pointer(&pid)))
		if pid != 0 && sameRunningExe(running, pid, version) {
			allowSetForeground.Call(uintptr(pid))
			postMessage.Call(running, wmShowSettings, 0, 0)
			return false
		}
		// Different version or path: close the previous instance gracefully
		// (WM_CLOSE -> DefWindowProc -> DestroyWindow -> removes tray icon).
		var proc windows.Handle
		if pid != 0 {
			proc, _ = windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
		}
		postMessage.Call(running, wmClose, 0, 0)
		if proc != 0 {
			if event, _ := windows.WaitForSingleObject(proc, 3000); event != windows.WAIT_OBJECT_0 {
				windows.TerminateProcess(proc, 0)
				windows.WaitForSingleObject(proc, 500)
			}
			windows.CloseHandle(proc)
		}
		for wait := 0; wait < 20; wait++ {
			if _, ok := acquireInstanceMutex(); ok {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}
	return false
}

// Alert shows a blocking error box, for failures before the UI is up.
func Alert(message string) {
	messageBox.Call(0, uintptr(unsafe.Pointer(str(message))), uintptr(unsafe.Pointer(str("KotoType"))), 0x10) // MB_ICONERROR
}

// Update publishes a session state from any goroutine.
func Update(s session.State) {
	stateMu.Lock()
	latest = s
	stateMu.Unlock()
	postMessage.Call(mainWnd, wmBarRefresh, 0, 0)
}

// Run creates the tray icon, bar, settings window and popup flyout and pumps
// messages until the user chooses Exit.
func Run(o Options) error {
	runtime.LockOSThread()
	opts, cfg = o, o.Config
	instance, _, _ = getModuleHandle.Call(0)
	loadTheme()
	if !initDirect2D() {
		return errors.New("初始化 Direct2D 失败")
	}
	defer releaseDirect2D()
	iconSmall, iconBig = appIcon(49), appIcon(11) // SM_CXSMICON, SM_CXICON
	taskbarUp, _, _ = registerMessage.Call(uintptr(unsafe.Pointer(str("TaskbarCreated"))))
	register("KotoTypeMain", mainProc)
	register("KotoTypeBar", barProc)
	register("KotoTypeSettings", settingsProc)
	register("KotoTypeAbout", aboutProc)
	register("KotoTypePopup", popupProc)
	mainWnd, _, _ = createWindow.Call(0, uintptr(unsafe.Pointer(str("KotoTypeMain"))), uintptr(unsafe.Pointer(str("KotoType "+o.Version))), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if mainWnd == 0 {
		return errors.New("创建主窗口失败")
	}
	createBar()
	createPopup()
	createSettings()
	createAbout()
	tray(0) // NIM_ADD
	if o.ShowSettings {
		showSettings()
	} else {
		showBarIdle()
	}
	var m msg
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return nil
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

type notifyData struct {
	Size        uint32
	Wnd         uintptr
	ID          uint32
	Flags       uint32
	CallbackMsg uint32
	Icon        uintptr
	Tip         [128]uint16
	State       uint32
	StateMask   uint32
	Info        [256]uint16
	Version     uint32
	InfoTitle   [64]uint16
	InfoFlags   uint32
	GUID        windows.GUID
	BalloonIcon uintptr
}

// tray adds (0), updates (1) or removes (2) the notification-area icon.
func tray(action uintptr) {
	data := notifyData{Wnd: mainWnd, ID: 1, Flags: 1 | 2 | 4, CallbackMsg: wmTray, Icon: iconSmall} // MESSAGE | ICON | TIP
	data.Size = uint32(unsafe.Sizeof(data))
	copy(data.Tip[:], windows.StringToUTF16("KotoType · "+desktop.HotkeyName(cfg.Hotkey)))
	notifyIcon.Call(action, uintptr(unsafe.Pointer(&data)))
}

// openAbout opens the project page in the default browser.
func openAbout() {
	shellExecute.Call(0, uintptr(unsafe.Pointer(str("open"))), uintptr(unsafe.Pointer(str(aboutURL))), 0, 0, 1) // SW_SHOWNORMAL
}

func trayMenu() {
	var p point
	getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	monitor, _, _ := monitorFromPoint.Call(uintptr(uint32(p.X))|uintptr(uint32(p.Y))<<32, 2)
	var dpi, dpiY uint32
	getDpiForMonitor.Call(monitor, 0, uintptr(unsafe.Pointer(&dpi)), uintptr(unsafe.Pointer(&dpiY)))
	barLabel := "显示语音条"
	if visible(barWnd) {
		barLabel = "隐藏语音条"
	}
	items := []popupItem{
		{label: barLabel, icon: "\ue720"},
		{label: "设置…", icon: "\ue713"},
		{label: "关于 KotoType " + opts.Version, icon: "\ue946"},
		{separator: true},
		{label: "退出", icon: "\ue7e8"},
	}
	anchor := windows.Rect{Left: p.X, Top: p.Y, Right: p.X, Bottom: p.Y}
	showPopup(0, anchor, 180, max(int(dpi), 96), items, -1, -2, func(choice int) {
		switch choice {
		case 0:
			if visible(barWnd) {
				hideBar()
			} else {
				showBarIdle()
			}
		case 1:
			showSettings()
		case 2:
			showAbout()
		case 4:
			destroyWindow.Call(mainWnd)
		}
	})
}

func mainProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmSettingChange:
		if lParam != 0 {
			if eq, _, _ := lstrcmpi.Call(lParam, uintptr(unsafe.Pointer(str("ImmersiveColorSet")))); eq == 0 {
				themeChanged()
			}
		}
		return 0
	case wmBarRefresh:
		stateMu.Lock()
		s := latest
		stateMu.Unlock()
		applyState(s)
		return 0
	case wmTestRefresh:
		refreshTest()
		return 0
	case wmChoices:
		choicesLoaded(int(wParam))
		return 0
	case wmShowSettings:
		showSettings()
		return 0
	case wmHotkey:
		hotkeyRecorded()
		return 0
	case wmTipDone:
		tipBusy = false
		refreshTip()
		return 0
	case wmTray:
		switch lParam & 0xffff {
		case wmLButtonUp:
			if visible(barWnd) {
				hideBar()
			} else {
				showBarIdle()
			}
		case wmRButtonUp:
			trayMenu()
		}
		return 0
	case wmDestroy:
		tray(2) // NIM_DELETE
		closePopup()
		stopTest()
		opts.Stop()
		barCanvas.release()
		settingsCanvas.release()
		releaseAboutSurface()
		popupCanvas.release()
		if testEditFont != 0 {
			deleteObject.Call(testEditFont)
		}
		if testEditBrush != 0 {
			deleteObject.Call(testEditBrush)
		}
		postQuitMessage.Call(0)
		return 0
	}
	if message == taskbarUp && taskbarUp != 0 {
		tray(0)
		return 0
	}
	return defProc(hwnd, message, wParam, lParam)
}
