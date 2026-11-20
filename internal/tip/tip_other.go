//go:build !windows

package tip

import "errors"

var errUnsupported = errors.New("当前平台没有输入法服务")

func Check() Status            { return Status{} }
func Installed() bool          { return false }
func Install() error           { return errUnsupported }
func Uninstall() error         { return errUnsupported }
func Elevate(arg string) error { return errUnsupported }
