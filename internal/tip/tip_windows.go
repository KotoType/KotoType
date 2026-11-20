//go:build windows

package tip

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	stateKey   = `SOFTWARE\KotoType`
	clsidValue = "TipCLSID"
)

var (
	shellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	findWindowEx   = windows.NewLazySystemDLL("user32.dll").NewProc("FindWindowExW")
	postMessageW   = windows.NewLazySystemDLL("user32.dll").NewProc("PostMessageW")
)

// root holds every installed copy, one directory per version.
func root() string { return filepath.Join(os.Getenv("ProgramFiles"), "KotoType", "tip") }

// files are where the copy built into this executable is installed, x64 then x86.
func files() [2]string {
	sum := sha256.New()
	sum.Write(dll64)
	sum.Write(dll32)
	dir := filepath.Join(root(), hex.EncodeToString(sum.Sum(nil))[:12])
	return [2]string{filepath.Join(dir, "kototype_tip_x64.dll"), filepath.Join(dir, "kototype_tip_x86.dll")}
}

// registered returns the DLL path registered under clsid in the given registry view.
func registered(clsid string, view uint32) string {
	if clsid == "" {
		return ""
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Classes\CLSID\`+clsid+`\InprocServer32`, registry.QUERY_VALUE|view)
	if err != nil {
		return ""
	}
	defer key.Close()
	path, _, _ := key.GetStringValue("")
	return path
}

func installedCLSID() string {
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, stateKey, registry.QUERY_VALUE|registry.WOW64_64KEY); err == nil {
		v, _, err := key.GetStringValue(clsidValue)
		key.Close()
		if err == nil && v != "" && registered(v, registry.WOW64_64KEY) != "" {
			return v
		}
	}
	if registered(currentCLSID, registry.WOW64_64KEY) != "" {
		return currentCLSID
	}
	return ""
}

func windowClassForCLSID(clsid string) string {
	if len(clsid) >= 10 && clsid[0] == '{' {
		return "KotoTypeTipWindow." + strings.ToLower(clsid[1:9])
	}
	return ""
}

func countWindows(className string) int {
	if className == "" {
		return 0
	}
	class := windows.StringToUTF16Ptr(className)
	var window uintptr
	n := 0
	for {
		window, _, _ = findWindowEx.Call(^uintptr(2), window, uintptr(unsafe.Pointer(class)), 0) // HWND_MESSAGE
		if window == 0 {
			return n
		}
		n++
	}
}

// Installed reports whether any copy of the text service is currently registered.
func Installed() bool { return installedCLSID() != "" }

func closeTipWindows(className string) {
	if className == "" {
		return
	}
	class := windows.StringToUTF16Ptr(className)
	var window uintptr
	for {
		window, _, _ = findWindowEx.Call(^uintptr(2), window, uintptr(unsafe.Pointer(class)), 0) // HWND_MESSAGE
		if window == 0 {
			return
		}
		postMessageW.Call(window, 0x0010, 0, 0) // WM_CLOSE
	}
}

func Check() Status {
	own := files()
	clsid := installedCLSID()
	s := Status{
		Supported: len(dll64) > 0,
		Installed: clsid != "",
		Current: len(dll64) > 0 &&
			strings.EqualFold(clsid, currentCLSID) &&
			strings.EqualFold(registered(clsid, registry.WOW64_64KEY), own[0]) &&
			strings.EqualFold(registered(clsid, registry.WOW64_32KEY), own[1]),
	}
	s.Active = countWindows(WindowClass)
	if !s.Current && clsid != "" {
		if oldClass := windowClassForCLSID(clsid); oldClass != WindowClass {
			s.Active += countWindows(oldClass)
		}
	}
	return s
}

// extract writes the built-in copy to its directory. A file already there
// has the same content (the directory is named after it) and may be loaded,
// so it is left alone.
func extract() ([2]string, error) {
	paths := files()
	if len(dll64) == 0 {
		return paths, errors.New("这个版本没有附带输入法服务")
	}
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0o755); err != nil {
		return paths, err
	}
	for i, data := range [][]byte{dll64, dll32} {
		if info, err := os.Stat(paths[i]); err == nil && info.Size() == int64(len(data)) {
			continue
		}
		if err := os.WriteFile(paths[i], data, 0o644); err != nil {
			return paths, err
		}
	}
	return paths, nil
}

// regsvr runs the x64 and x86 regsvr32 on the two DLLs.
func regsvr(paths [2]string, unregister bool) error {
	system := os.Getenv("SystemRoot")
	for i, exe := range []string{filepath.Join(system, "System32", "regsvr32.exe"), filepath.Join(system, "SysWOW64", "regsvr32.exe")} {
		if paths[i] == "" {
			continue
		}
		args := []string{"/s", paths[i]}
		if unregister {
			args = append([]string{"/u"}, args...)
		}
		if err := exec.Command(exe, args...).Run(); err != nil {
			return fmt.Errorf("regsvr32 %s：%w", filepath.Base(paths[i]), err)
		}
	}
	return nil
}

func saveInstalledCLSID(clsid string) {
	if clsid == "" {
		if key, err := registry.OpenKey(registry.LOCAL_MACHINE, stateKey, registry.SET_VALUE|registry.WOW64_64KEY); err == nil {
			key.DeleteValue(clsidValue)
			key.Close()
		}
		registry.DeleteKey(registry.LOCAL_MACHINE, stateKey)
		return
	}
	if key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, stateKey, registry.SET_VALUE|registry.WOW64_64KEY); err == nil {
		key.SetStringValue(clsidValue, clsid)
		key.Close()
	}
}

// Install registers the built-in copy under its version-specific CLSID,
// deactivating any older CLSID first so TSF hot-loads the new DLL into
// already-running processes without requiring application restarts.
func Install() error {
	paths, err := extract()
	if err != nil {
		return err
	}
	if oldCLSID := installedCLSID(); oldCLSID != "" {
		oldPaths := [2]string{registered(oldCLSID, registry.WOW64_64KEY), registered(oldCLSID, registry.WOW64_32KEY)}
		for i := range oldPaths {
			if _, statErr := os.Stat(oldPaths[i]); statErr != nil {
				oldPaths[i] = ""
			}
		}
		if oldPaths[0] != "" || oldPaths[1] != "" {
			regsvr(oldPaths, true)
			closeTipWindows(windowClassForCLSID(oldCLSID))
			// Let TSF notify running threads to deactivate the previous service.
			time.Sleep(time.Second)
		}
	}
	if err := regsvr(paths, false); err != nil {
		return err
	}
	saveInstalledCLSID(currentCLSID)
	removeOthers(filepath.Dir(paths[0]))
	return nil
}

// Uninstall unregisters the active service and removes every installed copy.
func Uninstall() error {
	oldCLSID := installedCLSID()
	paths := [2]string{registered(oldCLSID, registry.WOW64_64KEY), registered(oldCLSID, registry.WOW64_32KEY)}
	_, missing64 := os.Stat(paths[0])
	_, missing32 := os.Stat(paths[1])
	if missing64 != nil || missing32 != nil {
		if strings.EqualFold(oldCLSID, currentCLSID) || oldCLSID == "" {
			var err error
			if paths, err = extract(); err != nil {
				return err
			}
		} else {
			if missing64 != nil {
				paths[0] = ""
			}
			if missing32 != nil {
				paths[1] = ""
			}
		}
	}
	if err := regsvr(paths, true); err != nil {
		return err
	}
	closeTipWindows(WindowClass)
	if oldClass := windowClassForCLSID(oldCLSID); oldClass != WindowClass {
		closeTipWindows(oldClass)
	}
	saveInstalledCLSID("")
	removeOthers("")
	return nil
}

// removeOthers deletes every installed copy except the one in keep. Copies
// still loaded in running applications are deleted at the next restart.
func removeOthers(keep string) {
	dirs, _ := os.ReadDir(root())
	for _, entry := range dirs {
		dir := filepath.Join(root(), entry.Name())
		if strings.EqualFold(dir, keep) || os.RemoveAll(dir) == nil {
			continue
		}
		left, _ := os.ReadDir(dir)
		for _, file := range left {
			windows.MoveFileEx(windows.StringToUTF16Ptr(filepath.Join(dir, file.Name())), nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
		}
		windows.MoveFileEx(windows.StringToUTF16Ptr(dir), nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
	}
	if keep == "" {
		for _, dir := range []string{root(), filepath.Dir(root())} {
			if os.Remove(dir) != nil {
				windows.MoveFileEx(windows.StringToUTF16Ptr(dir), nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
			}
		}
	}
}

// Elevate runs this executable with arg as administrator, which Windows
// confirms with the user, and waits for it to finish.
func Elevate(arg string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	info := struct {
		Size, Mask                        uint32
		Window                            uintptr
		Verb, File, Parameters, Directory *uint16
		Show                              int32
		Instance, IDList                  uintptr
		Class                             *uint16
		ClassKey                          uintptr
		HotKey                            uint32
		Icon                              uintptr
		Process                           windows.Handle
	}{
		Mask:       0x40 | 0x100, // SEE_MASK_NOCLOSEPROCESS | SEE_MASK_NOASYNC
		Verb:       windows.StringToUTF16Ptr("runas"),
		File:       windows.StringToUTF16Ptr(exe),
		Parameters: windows.StringToUTF16Ptr(arg),
	}
	info.Size = uint32(unsafe.Sizeof(info))
	if r, _, err := shellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if err == windows.ERROR_CANCELLED {
			return errors.New("已取消")
		}
		return err
	}
	defer windows.CloseHandle(info.Process)
	windows.WaitForSingleObject(info.Process, windows.INFINITE)
	var code uint32
	windows.GetExitCodeProcess(info.Process, &code)
	if code != 0 {
		return fmt.Errorf("没有完成（代码 %d）", code)
	}
	return nil
}
