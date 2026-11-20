package desktop

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	setWindowsHook   = user32.NewProc("SetWindowsHookExW")
	callNextHook     = user32.NewProc("CallNextHookEx")
	unhookWindows    = user32.NewProc("UnhookWindowsHookEx")
	setWinEvent      = user32.NewProc("SetWinEventHook")
	unhookWinEvent   = user32.NewProc("UnhookWinEvent")
	getMessage       = user32.NewProc("GetMessageW")
	getAsyncKey      = user32.NewProc("GetAsyncKeyState")
	setTimer         = user32.NewProc("SetTimer")
	killTimer        = user32.NewProc("KillTimer")
	sendInput        = user32.NewProc("SendInput")
	registerHotKey   = user32.NewProc("RegisterHotKey")
	unregisterHotKey = user32.NewProc("UnregisterHotKey")
)

const (
	vkShift = 0x10
	vkCtrl  = 0x11
	vkAlt   = 0x12
	vkLWin  = 0x5b
	vkRWin  = 0x5c
	vkMask  = 0xe8 // Assigned to no key; see sendMask.
)

// Idle, the hotkey is registered with Windows and nothing in this program
// sees ordinary typing. The low-level keyboard hook is installed only while
// it is needed (see reconcile): from the hotkey until all its keys are up, so
// the start waits for the release and the hotkey's key-up is swallowed; while
// dictating, so that any key stops it; while recording a new hotkey; and for
// good if Windows refuses to register the hotkey, in which case the hook
// detects it instead.
//
// chord is the hotkey for the hook thread: its key code in the low byte and
// one bit per required modifier.
var chord atomic.Uint32

const (
	chordCtrl = 1 << (8 + iota)
	chordAlt
	chordShift
	chordWin
)

// SetHotkey installs a chord such as "ctrl+alt+space", "win+alt+h" or "f9".
// A value it cannot read selects the default chord.
func SetHotkey(value string) {
	c, ok := parseHotkey(value)
	if !ok {
		c, _ = parseHotkey("ctrl+alt+space")
	}
	chord.Store(c)
	update()
}

func parseHotkey(value string) (c uint32, ok bool) {
	if value == "" {
		value = "ctrl+alt+space"
	}
	for _, part := range strings.Split(strings.ToLower(value), "+") {
		switch {
		case part == "ctrl":
			c |= chordCtrl
		case part == "alt":
			c |= chordAlt
		case part == "shift":
			c |= chordShift
		case part == "win":
			c |= chordWin
		case c&0xff != 0:
			return 0, false // A second key.
		case part == "space":
			c |= 0x20
		case len(part) == 1 && (unicode.IsLower(rune(part[0])) || unicode.IsDigit(rune(part[0]))):
			c |= uint32(unicode.ToUpper(rune(part[0]))) // Letter and digit key codes are their ASCII.
		case strings.HasPrefix(part, "f"):
			n, err := strconv.Atoi(part[1:])
			if err != nil || n < 1 || n > 24 {
				return 0, false
			}
			c |= uint32(0x70 + n - 1)
		default:
			return 0, false
		}
	}
	return c, c&0xff != 0
}

// capture, while set, receives the next key combination in place of the
// normal hotkey handling.
var capture atomic.Pointer[func(string)]

// CaptureHotkey calls done once with the next key combination the user
// presses, in SetHotkey's form, or with "" if they press Esc. Combinations a
// hotkey cannot use (a bare letter, Shift plus a letter) are ignored. Passing
// nil stops waiting.
func CaptureHotkey(done func(value string)) {
	if done == nil {
		capture.Store(nil)
	} else {
		capture.Store(&done)
	}
	update()
}

// heldChord names the combination of the held modifiers and key vk, or
// returns "" if it would not make a usable hotkey.
func heldChord(vk uint32) string {
	var key string
	switch {
	case vk == 0x20:
		key = "space"
	case vk >= '0' && vk <= '9' || vk >= 'A' && vk <= 'Z':
		key = strings.ToLower(string(rune(vk)))
	case vk >= 0x70 && vk <= 0x87:
		key = "f" + strconv.Itoa(int(vk-0x70)+1)
	default:
		return ""
	}
	win := held[vkLWin] || held[vkRWin]
	if vk < 0x70 && !held[vkCtrl] && !held[vkAlt] && !win {
		return "" // Would swallow ordinary typing.
	}
	value := ""
	for _, m := range []struct {
		held bool
		name string
	}{{held[vkCtrl], "ctrl+"}, {held[vkAlt], "alt+"}, {held[vkShift], "shift+"}, {win, "win+"}} {
		if m.held {
			value += m.name
		}
	}
	return value + key
}

type keyboardEvent struct {
	VK, Scan, Flags, Time uint32
	Extra                 uintptr
}

// Hook state lives on the hook thread only.
var (
	held         [256]bool
	pendingStart bool   // Hotkey pressed while idle; fires once every key is up.
	interrupted  bool   // A key stopped dictation; ignore the hotkey until all keys are up.
	swallowed    uint32 // Key whose key-down was eaten, so its key-up is too.
	onHotkeyFunc func()
)

// modifier folds left/right variants into the generic virtual key.
func modifier(vk uint32) uint32 {
	switch vk {
	case 0xa0, 0xa1:
		return vkShift
	case 0xa2, 0xa3:
		return vkCtrl
	case 0xa4, 0xa5:
		return vkAlt
	}
	return vk
}

// maskTag marks sendMask's events so the hook lets them through untouched.
const maskTag uintptr = 0x584d << 48

type keyInput struct {
	Kind        uint32
	_           uint32
	VK, Scan    uint16
	Flags, Time uint32
	Extra       uintptr
	_           [8]byte
}

// sendMask taps a key no keyboard has. Because the hotkey's own key is
// swallowed, Windows would otherwise see the modifiers of chord c pressed and
// released alone and react to that: Alt opens the menu bar, Win the Start
// menu, Ctrl+Shift or Alt+Shift switches the input method. One tap of any
// other key prevents it. Ctrl+Alt alone triggers nothing and needs no mask.
func sendMask(c uint32) {
	if c&(chordWin|chordShift) == 0 && c&(chordCtrl|chordAlt) != chordAlt {
		return
	}
	input := [2]keyInput{{Kind: 1, VK: vkMask, Extra: maskTag}, {Kind: 1, VK: vkMask, Flags: 2, Extra: maskTag}}
	sendInput.Call(2, uintptr(unsafe.Pointer(&input[0])), unsafe.Sizeof(input[0]))
}

var keyboardCallback = syscall.NewCallback(func(code, message uintptr, event *keyboardEvent) uintptr {
	if int32(code) < 0 || event.Extra == maskTag {
		return next(code, message, event)
	}
	down := message == 0x100 || message == 0x104 // WM_KEYDOWN, WM_SYSKEYDOWN
	vk := modifier(event.VK)
	if vk > 255 {
		return next(code, message, event)
	}
	wasHeld := held[vk]
	held[vk] = down
	if !down {
		swallow := vk == swallowed
		if swallow {
			swallowed = 0
		}
		if !anyHeld(vk) {
			interrupted = false
			if pendingStart {
				pendingStart = false
				// Fire only after release so the modifiers cannot combine with typed text.
				go onHotkeyFunc()
			}
			update() // The hook may no longer be needed.
		}
		if swallow {
			return 1
		}
		return next(code, message, event)
	}
	anyHeld(vk) // Drop stale modifiers before they are compared with the hotkey.
	if done := capture.Load(); done != nil {
		if vk == vkCtrl || vk == vkAlt || vk == vkShift || vk == vkLWin || vk == vkRWin {
			return next(code, message, event)
		}
		swallowed = vk
		if value := heldChord(vk); value != "" || vk == 0x1b { // Esc cancels
			capture.Store(nil)
			go (*done)(value)
		}
		return 1
	}
	if t := current(); t != nil {
		stopIf(t, "检测到键盘输入")
		interrupted = true
	}
	c := chord.Load()
	if vk == c&0xff &&
		held[vkCtrl] == (c&chordCtrl != 0) && held[vkAlt] == (c&chordAlt != 0) &&
		held[vkShift] == (c&chordShift != 0) && (held[vkLWin] || held[vkRWin]) == (c&chordWin != 0) {
		if !wasHeld {
			pendingStart = !interrupted
			sendMask(c)
		}
		swallowed = vk
		return 1
	}
	if vk != vkCtrl && vk != vkAlt && vk != vkShift && vk != vkLWin && vk != vkRWin {
		pendingStart = false
	}
	return next(code, message, event)
})

// anyHeld reports whether a key other than current, the one this event is
// about, is still down. Key-ups never arrive when another desktop takes the
// keyboard mid-press (lock screen, UAC prompt, Ctrl+Alt+Del), so every key
// believed held is confirmed with the system; otherwise one stale entry
// would keep the hotkey from ever firing again.
func anyHeld(current uint32) bool {
	any := false
	for vk := range held {
		if held[vk] && uint32(vk) != current {
			state, _, _ := getAsyncKey.Call(uintptr(vk))
			held[vk] = state&0x8000 != 0
			any = any || held[vk]
		}
	}
	return any
}

func next(code, message uintptr, event *keyboardEvent) uintptr {
	r, _, _ := callNextHook.Call(0, code, message, uintptr(unsafe.Pointer(event)))
	return r
}

// The event only prompts a look at the real foreground window. Its own window
// argument is not trustworthy: apps report helper windows (a UWP CoreWindow
// under its frame, accessibility windows created on the first UI Automation
// query), and events queued before the target was locked arrive late.
var foregroundCallback = syscall.NewCallback(func(_ uintptr, _ uint32, _ uintptr, _, _ int32, _, _ uint32) uintptr {
	t := current()
	if t == nil {
		return 0
	}
	foreground, _, _ := getForeground.Call()
	if foreground == 0 || foreground == t.window {
		return 0 // Zero is a transition in progress; the next event settles it.
	}
	var process uint32
	getThreadProcess.Call(foreground, uintptr(unsafe.Pointer(&process)))
	if process != windows.GetCurrentProcessId() {
		stopIf(t, "窗口已切换")
	}
	return 0
})

const (
	wmTimer  = 0x0113
	wmHotkey = 0x0312
	wmUpdate = 0x8001 // WM_APP + 1: whether the keyboard hook is needed may have changed.
)

// update asks the hook thread to reconcile; any thread may call it.
func update() {
	if hookThread != 0 {
		postThreadMessage.Call(uintptr(hookThread), wmUpdate, 0, 0)
	}
}

// Hook thread state besides the callback's.
var (
	module          uintptr
	keyboard        uintptr // The low-level hook while installed.
	reinstallTimer  uintptr
	registeredChord uint32
	registered      bool // Windows accepted registeredChord as a hotkey.
)

// reconcile registers the current hotkey and installs the keyboard hook
// exactly while it is needed.
func reconcile() {
	if c := chord.Load(); c != registeredChord {
		unregisterHotKey.Call(0, 1)
		var modifiers uintptr = 0x4000 // MOD_NOREPEAT
		for _, m := range [...]struct {
			bit uint32
			mod uintptr
		}{{chordAlt, 1}, {chordCtrl, 2}, {chordShift, 4}, {chordWin, 8}} {
			if c&m.bit != 0 {
				modifiers |= m.mod
			}
		}
		r, _, _ := registerHotKey.Call(0, 1, modifiers, uintptr(c&0xff))
		registered, registeredChord = r != 0, c
	}
	needed := !registered || pendingStart || interrupted || current() != nil || capture.Load() != nil
	switch {
	case needed && keyboard == 0:
		keyboard, _, _ = setWindowsHook.Call(13, keyboardCallback, module, 0) // WH_KEYBOARD_LL
		// Nothing tracked the keys while the hook was out. Only the hotkey's
		// keys are taken from the system: some keyboards report a key down
		// that never comes up, and believing it held would block the start.
		held = [256]bool{}
		for _, vk := range [...]uint32{vkShift, vkCtrl, vkAlt, vkLWin, vkRWin, chord.Load() & 0xff} {
			state, _, _ := getAsyncKey.Call(uintptr(vk))
			held[vk] = state&0x8000 != 0
		}
		// Windows silently removes a low-level hook whose callback once
		// failed to answer in time (a stalled machine, a resume from sleep).
		// Reinstalling every minute bounds how long it can be dead.
		reinstallTimer, _, _ = setTimer.Call(0, 0, 60_000, 0)
	case !needed && keyboard != 0:
		unhookWindows.Call(keyboard)
		killTimer.Call(0, reinstallTimer)
		keyboard, reinstallTimer = 0, 0
	}
}

// hotkeyPressed handles the registered hotkey: like the hook path, the start
// waits until every key is up.
func hotkeyPressed() {
	c := chord.Load()
	sendMask(c)
	pendingStart = true
	reconcile() // Installs the hook to see the release.
	if state, _, _ := getAsyncKey.Call(uintptr(c & 0xff)); state&0x8000 != 0 {
		swallowed = c & 0xff // Its key-down went to Windows; keep the key-up from the application too.
	}
	if !anyHeld(0) {
		pendingStart = false
		go onHotkeyFunc()
		reconcile()
	}
}

// runHooks registers the hotkey and the foreground hook and pumps messages
// until WM_QUIT.
func runHooks(onHotkey func(), ready chan<- error) {
	onHotkeyFunc = onHotkey
	module, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	// EVENT_SYSTEM_FOREGROUND, WINEVENT_OUTOFCONTEXT
	foreground, _, foregroundErr := setWinEvent.Call(3, 3, 0, foregroundCallback, 0, 0, 0)
	reconcile()
	defer func() {
		if keyboard != 0 {
			unhookWindows.Call(keyboard)
			keyboard = 0
		}
		unregisterHotKey.Call(0, 1)
		registeredChord = 0
		if foreground != 0 {
			unhookWinEvent.Call(foreground)
		}
	}()
	if foreground == 0 {
		ready <- fmt.Errorf("安装窗口切换钩子失败：%v", foregroundErr)
		return
	}
	if !registered && keyboard == 0 {
		ready <- errors.New("快捷键注册失败，也无法安装键盘钩子")
		return
	}
	ready <- nil
	var m struct {
		window  uintptr
		message uint32
		_       [36]byte
	}
	for {
		if r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
			return
		}
		switch m.message {
		case wmHotkey:
			hotkeyPressed()
		case wmUpdate:
			reconcile()
		case wmTimer:
			if keyboard == 0 {
				break
			}
			if fresh, _, _ := setWindowsHook.Call(13, keyboardCallback, module, 0); fresh != 0 {
				unhookWindows.Call(keyboard)
				keyboard = fresh
			}
		}
	}
}
