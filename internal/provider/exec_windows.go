//go:build windows

package provider

import (
	"os/exec"
	"syscall"
)

// hideWindow prevents console flashes when launching provider subprocesses.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
