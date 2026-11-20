// Package ui draws the floating dictation bar and the settings window with
// each platform's own window API. Run owns the calling OS thread.
package ui

import (
	"context"

	"github.com/KotoType/KotoType/internal/audio"
	"github.com/KotoType/KotoType/internal/config"
	"github.com/KotoType/KotoType/internal/provider"
)

// aboutURL is the project page the About entries open.
const aboutURL = "https://github.com/KotoType/KotoType"

type Options struct {
	Version   string // Shown in the About entries.
	Config    config.Config
	OnChange  func(config.Config) // A setting changed; called on the UI thread.
	Toggle    func()              // Start dictation into the focused field, or finish it.
	Stop      func()              // Abort dictation.
	Devices   func() ([]audio.Device, error)
	Providers func() []provider.Info // Discovers installed external ASR provider plugins.
	// Test recognizes with cfg into the settings window only.
	Test         func(ctx context.Context, cfg config.Config, onText func(string), onLevel func(float64)) error
	ShowSettings bool // Open the settings window right away.
}
