// Command kototype dictates into whatever text field has the focus.
//
// The icon and the Windows manifest (visual styles, per-monitor DPI) are
// compiled into rsrc_windows_*.syso; regenerate them after changing the logo.
//
//go:generate go -C ../../tools run ./genicon ../assets/logo.svg ../assets/icon.png
//go:generate go -C ../../tools tool go-winres simply --arch amd64,arm64 --icon ../assets/icon.png --manifest gui --file-description KotoType --product-name KotoType --out ../cmd/kototype/rsrc
package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/KotoType/KotoType/internal/audio"
	"github.com/KotoType/KotoType/internal/config"
	"github.com/KotoType/KotoType/internal/desktop"
	"github.com/KotoType/KotoType/internal/provider"
	"github.com/KotoType/KotoType/internal/session"
	"github.com/KotoType/KotoType/internal/tip"
	"github.com/KotoType/KotoType/internal/ui"
)

// version is set by build.sh.
var version = "dev"

func main() {
	switch {
	case slices.Contains(os.Args[1:], tip.InstallArg):
		elevated(tip.Install)
	case slices.Contains(os.Args[1:], tip.UninstallArg):
		elevated(tip.Uninstall)
	}
	if !ui.SingleInstance(version) {
		return
	}
	// A missing or unreadable file means default settings.
	cfg, _ := config.Load()
	var mu sync.Mutex // Guards cfg: the UI thread writes it, dictations read it.
	recognize := func(ctx context.Context, c config.Config, endPause time.Duration, record provider.Recorder, onText func(string, int), ready func()) error {
		info, ok := provider.Resolve(provider.Discover(), c.Provider)
		if !ok {
			return errors.New("未找到语音识别模块，请将 kototype-provider-*.exe 放入 providers 目录")
		}
		req := provider.Request{
			Options:    c.ProviderOptionMap(info.ID),
			EndPauseMs: endPause.Milliseconds(),
		}
		return provider.Recognize(ctx, info.Path, req, record, onText, ready)
	}
	controller := &session.Controller{
		Acquire:   desktop.Acquire,
		Recognize: recognize,
		Config: func() config.Config {
			mu.Lock()
			defer mu.Unlock()
			return cfg
		},
		OnState: ui.Update,
	}
	if err := desktop.Start(cfg.Hotkey, controller.Toggle); err != nil {
		ui.Alert("初始化失败：" + err.Error())
		os.Exit(1)
	}
	defer desktop.Close()
	if cfg.Autostart == "on" {
		desktop.SetAutostart(true) // The executable may have moved since it was registered.
	}
	err := ui.Run(ui.Options{
		Version: version,
		Config:  cfg,
		OnChange: func(next config.Config) {
			mu.Lock()
			previous := cfg
			cfg = next
			mu.Unlock()
			config.Save(next)
			if next.Hotkey != previous.Hotkey {
				desktop.SetHotkey(next.Hotkey)
			}
			if next.Autostart != previous.Autostart {
				desktop.SetAutostart(next.Autostart == "on")
			}
		},
		Toggle:    controller.Toggle,
		Stop:      controller.Stop,
		Devices:   audio.Devices,
		Providers: provider.Discover,
		Test: func(ctx context.Context, cfg config.Config, onText func(string), onLevel func(float64)) error {
			return recognize(ctx, cfg, 0, func(ctx context.Context, send func([]byte) error, ready func()) error {
				return audio.Capture(ctx, cfg.Microphone, func(pcm []byte) error {
					onLevel(audio.Level(pcm))
					return send(pcm)
				}, ready)
			}, func(text string, _ int) { onText(session.Format(text, cfg.Punctuation)) }, func() {})
		},
		ShowSettings: slices.Contains(os.Args[1:], "--settings"),
	})
	if err != nil {
		ui.Alert(err.Error())
		os.Exit(1)
	}
}

// elevated runs an install step the settings window started as administrator,
// then exits; the exit code tells the settings window whether it worked.
func elevated(step func() error) {
	if err := step(); err != nil {
		ui.Alert("输入法服务：" + err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}
