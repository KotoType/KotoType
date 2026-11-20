package desktop

import (
	"errors"
	"image"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KotoType/KotoType/internal/tip"
	"github.com/KotoType/KotoType/internal/winapi"
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	oleacc            = windows.NewLazySystemDLL("oleacc.dll")
	getForeground     = user32.NewProc("GetForegroundWindow")
	getThreadProcess  = user32.NewProc("GetWindowThreadProcessId")
	getGUIThreadInfo  = user32.NewProc("GetGUIThreadInfo")
	clientToScreen    = user32.NewProc("ClientToScreen")
	postThreadMessage = user32.NewProc("PostThreadMessageW")
	accFromWindow     = oleacc.NewProc("AccessibleObjectFromWindow")
	iidIAccessible    = windows.GUID{Data1: 0x618736e0, Data2: 0x3c3d, Data3: 0x11cf, Data4: [8]byte{0x81, 0x0c, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
)

// target is a text field reached through the XVoice text service, which
// edits it as a composition on the application's UI thread.
type target struct {
	window      uintptr
	tip         uintptr // Text service window on the thread that owns the focus.
	state       uintptr // Its QUERY answer when acquired; any change means the focus moved.
	composed    []rune  // The composition so far.
	anchor      image.Rectangle
	anchored    bool
	anchorDirty bool // Set after Type so the next poll refreshes caret position once laid out.
	done        chan struct{}
	reason      string // Written once under mu before done closes.
}

// Text service calls run on one worker fed through jobs, which also polls the
// active target. The active target is shared with hook callbacks, which only
// stop it.
var (
	jobs       = make(chan func())
	quit       chan struct{}
	finished   sync.WaitGroup
	mu         sync.Mutex
	active     *target
	hookThread uint32
)

func current() *target {
	mu.Lock()
	defer mu.Unlock()
	return active
}

// stopIf ends t if it is still the active target; later stops are no-ops.
func stopIf(t *target, reason string) {
	mu.Lock()
	defer mu.Unlock()
	if t == nil || active != t {
		return
	}
	active = nil
	t.reason = reason
	close(t.done)
	update() // The keyboard hook is no longer needed for it.
}

func stopActive(reason string) { stopIf(current(), reason) }

// do runs f on the worker and waits for it.
func do(f func()) {
	done := make(chan struct{})
	jobs <- func() {
		defer close(done)
		f()
	}
	<-done
}

// Start runs the worker and the global keyboard hook. onHotkey runs on its own
// goroutine after the hotkey chord is fully released.
func Start(hotkey string, onHotkey func()) error {
	SetHotkey(hotkey)
	quit = make(chan struct{})
	ready := make(chan error, 1)
	finished.Add(2)
	go func() {
		defer finished.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if winapi.InitMTA() == nil {
			defer winapi.Uninit()
		}
		// The focus is polled only while a target is active; idle, the worker
		// sleeps until a job arrives.
		var poll *time.Ticker
		var tick <-chan time.Time
		for {
			select {
			case <-quit:
				stopActive("已退出")
				return
			case job := <-jobs:
				job()
			case <-tick:
				if t := current(); t != nil {
					if !stillFocused(t) {
						stopIf(t, "输入框已切换")
					} else if t.anchorDirty {
						t.anchorDirty = false
						if rect, ok := locateAnchor(t.window, t.tip); ok {
							mu.Lock()
							t.anchor, t.anchored = rect, true
							mu.Unlock()
						}
					}
				}
			}
			switch dictating := current() != nil; {
			case dictating && poll == nil:
				poll = time.NewTicker(100 * time.Millisecond)
				tick = poll.C
			case !dictating && poll != nil:
				poll.Stop()
				poll, tick = nil, nil
			}
		}
	}()
	go func() {
		defer finished.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hookThread = windows.GetCurrentThreadId()
		runHooks(onHotkey, ready)
	}()
	err := <-ready
	if err != nil {
		Close()
	}
	return err
}

func Close() {
	if quit == nil {
		return
	}
	close(quit)
	if hookThread != 0 {
		postThreadMessage.Call(uintptr(hookThread), 0x12, 0, 0) // WM_QUIT
	}
	finished.Wait()
	quit = nil
}

// stillFocused reports whether t's window is in front and the text service
// still reports the same focus. A busy application that does not answer in
// time keeps its target.
func stillFocused(t *target) bool {
	if w, _, _ := getForeground.Call(); w != t.window {
		return false
	}
	state, ok := tipSend(t.tip, tipQuery, "")
	return !ok || state == t.state
}

// Acquire locks the focused text field of another process. Only the text
// service can tell a text field from anything else and write into it, so
// without it nothing is acquired.
func Acquire() (Target, error) {
	var t *target
	var err error
	do(func() {
		window, _, _ := getForeground.Call()
		if window == 0 {
			err = errors.New("请先点击输入框")
			return
		}
		if !tip.Installed() {
			err = errors.New("请先在设置里安装输入法服务")
			return
		}
		service := findTip(window)
		if service == 0 {
			// The service creates its window on a thread's first text focus.
			err = errors.New("当前焦点不是输入框")
			return
		}
		state, ok := tipSend(service, tipQuery, "")
		switch {
		case !ok:
			err = errors.New("目标程序没有响应")
		case state&3 == 0:
			err = errors.New("当前焦点不是输入框")
		case state&3 == 2:
			err = errors.New("当前输入框不能输入")
		default:
			t = &target{window: window, tip: service, state: state, done: make(chan struct{})}
			t.anchor, t.anchored = locateAnchor(window, service)
			// Made active here, on the worker, so that it starts polling at
			// once; the hook thread installs the keyboard hook that stops it.
			stopActive("已切换到新的输入")
			mu.Lock()
			active = t
			mu.Unlock()
			update()
		}
	})
	if t == nil {
		return nil, err
	}
	return t, nil
}

func locateAnchor(window, service uintptr) (image.Rectangle, bool) {
	tipRect, exact, hasTip := tipCaretRect(service)
	if hasTip && exact {
		return tipRect, true
	}
	if caretRect, ok := systemCaret(window); ok {
		return caretRect, true
	}
	if accRect, ok := accessibleCaret(window); ok {
		return accRect, true
	}
	if hasTip {
		return tipRect, true
	}
	return image.Rectangle{}, false
}

// accessibleCaret reads the screen caret rectangle via lightweight MSAA
// OBJID_CARET, covering Chromium/Electron apps even before they reload the TIP.
func accessibleCaret(window uintptr) (image.Rectangle, bool) {
	targets := []uintptr{window}
	if info, ok := threadInfo(window); ok && info.Focus != 0 && info.Focus != window {
		targets = []uintptr{info.Focus, window}
	}
	for _, hwnd := range targets {
		var acc *winapi.Object
		hr, _, _ := accFromWindow.Call(hwnd, uintptr(uint32(0xfffffff8)), uintptr(unsafe.Pointer(&iidIAccessible)), uintptr(unsafe.Pointer(&acc)))
		if winapi.Failed(hr) || acc == nil {
			continue
		}
		var l, t, w, h int32
		child := winapi.Variant{VT: 3} // VT_I4, CHILDID_SELF (0)
		hr = acc.Call(22, uintptr(unsafe.Pointer(&l)), uintptr(unsafe.Pointer(&t)),
			uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&child)))
		acc.Release()
		if !winapi.Failed(hr) && (h >= 6 || w >= 2) {
			return image.Rect(int(l), int(t), int(l)+max(int(w), 1), int(t)+max(int(h), 1)), true
		}
	}
	return image.Rectangle{}, false
}

// systemCaret is where the application says its caret is, for placing the bar.
func systemCaret(window uintptr) (image.Rectangle, bool) {
	info, ok := threadInfo(window)
	if !ok || info.CaretW == 0 || info.Caret.Bottom <= info.Caret.Top {
		return image.Rectangle{}, false
	}
	points := [2]struct{ X, Y int32 }{{info.Caret.Left, info.Caret.Top}, {info.Caret.Right, info.Caret.Bottom}}
	for i := range points {
		clientToScreen.Call(info.CaretW, uintptr(unsafe.Pointer(&points[i])))
	}
	return image.Rect(int(points[0].X), int(points[0].Y), int(points[1].X)+1, int(points[1].Y)), true
}

func (t *target) Done() <-chan struct{} { return t.done }

func (t *target) Reason() string {
	mu.Lock()
	defer mu.Unlock()
	return t.reason
}

func (t *target) Anchor() (image.Rectangle, bool) {
	mu.Lock()
	defer mu.Unlock()
	return t.anchor, t.anchored
}

// Release ends the target and commits the composition so that it stays in
// the field.
func (t *target) Release() {
	do(func() { tipSend(t.tip, tipCommit, "") })
	stopIf(t, "已停止")
}

// Type replaces the last erase characters of the composition with text.
func (t *target) Type(erase int, text string) error {
	var err error
	do(func() {
		if current() != t || !stillFocused(t) {
			stopIf(t, "输入框已切换")
			err = errors.New("输入框已失去焦点")
			return
		}
		if erase == 0 && text == "" {
			return
		}
		t.composed = append(t.composed[:max(0, len(t.composed)-erase)], []rune(text)...)
		if queued, _ := tipSend(t.tip, tipCompose, string(t.composed)); queued != 1 {
			stopIf(t, "文字输入失败")
			err = errors.New("输入法服务没有响应")
			return
		}
		t.anchorDirty = true
	})
	return err
}
