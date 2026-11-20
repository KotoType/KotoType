package desktop

import (
	"image"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/KotoType/KotoType/internal/tip"
)

// The XVoice text service (tip/xvoice_tip.cpp) is a TSF speech TIP that TSF
// loads into applications next to the keyboard IME. On every thread that gets
// a text focus it keeps a message-only window; commands sent to that window
// run on the application's UI thread, where the service edits the field as a
// composition. It is the only way text is written.

var (
	findWindowEx       = user32.NewProc("FindWindowExW")
	sendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
	tipClass           = windows.StringToUTF16Ptr(tip.WindowClass)
)

// Commands of the text service window, see tip/xvoice_tip.cpp.
const (
	tipQuery   = 1 // -> generation<<2 | state (1 editable, 2 not writable, 0 no text focus)
	tipCompose = 2 // show text as the composition
	tipCommit  = 3 // replace the composition with text, if any, and end it
	tipCaret   = 4 // read selection (1) or view (2) screen rect into service cache
	tipCaretLT = 5 // packed int16 left | top<<16
	tipCaretRB = 6 // packed int16 right | bottom<<16
)

type guiThreadInfo struct {
	Size, Flags                                         uint32
	Active, Focus, Capture, MenuOwner, MoveSize, CaretW uintptr
	Caret                                               windows.Rect
}

func threadInfo(window uintptr) (guiThreadInfo, bool) {
	thread, _, _ := getThreadProcess.Call(window, 0)
	info := guiThreadInfo{}
	info.Size = uint32(unsafe.Sizeof(info))
	r, _, _ := getGUIThreadInfo.Call(thread, uintptr(unsafe.Pointer(&info)))
	return info, r != 0
}

func findTipByClass(thread uintptr, class *uint16) uintptr {
	var hwnd uintptr
	for {
		hwnd, _, _ = findWindowEx.Call(^uintptr(2), hwnd, uintptr(unsafe.Pointer(class)), 0) // HWND_MESSAGE
		if hwnd == 0 {
			return 0
		}
		if t, _, _ := getThreadProcess.Call(hwnd, 0); t == thread {
			return hwnd
		}
	}
}

// findTip returns the text service window on the thread that owns the
// keyboard focus of window, preferring this build's versioned window class.
func findTip(window uintptr) uintptr {
	owner := window
	if info, ok := threadInfo(window); ok && info.Focus != 0 {
		owner = info.Focus // Another thread may have attached its input.
	}
	thread, _, _ := getThreadProcess.Call(owner, 0)
	return findTipByClass(thread, tipClass)
}

// tipSend delivers a command and returns its answer; ok is false when the
// application did not answer in time.
func tipSend(tip, command uintptr, text string) (answer uintptr, ok bool) {
	chars := utf16.Encode([]rune(text))
	data := struct {
		Command uintptr
		Size    uint32
		Text    unsafe.Pointer
	}{Command: command, Size: uint32(len(chars) * 2)}
	if len(chars) > 0 {
		data.Text = unsafe.Pointer(&chars[0])
	}
	// WM_COPYDATA; SMTO_BLOCK | SMTO_ABORTIFHUNG
	r, _, _ := sendMessageTimeout.Call(tip, 0x4a, 0, uintptr(unsafe.Pointer(&data)), 0x1|0x2, 500, uintptr(unsafe.Pointer(&answer)))
	return answer, r != 0
}

// tipCaretRect asks the text service for the focused selection's screen rect
// (exact=true) or the text view's screen rect (exact=false).
func tipCaretRect(service uintptr) (rect image.Rectangle, exact bool, ok bool) {
	kind, sent := tipSend(service, tipCaret, "")
	if !sent || kind == 0 {
		return image.Rectangle{}, false, false
	}
	lt, ok1 := tipSend(service, tipCaretLT, "")
	rb, ok2 := tipSend(service, tipCaretRB, "")
	if !ok1 || !ok2 {
		return image.Rectangle{}, false, false
	}
	l, t := int(int16(uint16(lt))), int(int16(uint16(lt>>16)))
	r, b := int(int16(uint16(rb))), int(int16(uint16(rb>>16)))
	return image.Rect(l, t, max(r, l+1), max(b, t+1)), kind == 1, true
}
