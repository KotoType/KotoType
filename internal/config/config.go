// Package config holds the user's settings and persists them as JSON under
// either the OS config directory or alongside the executable (portable mode).
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Choice is one value a setting can take, with its display name. The first
// choice of every setting has Value "" and is the default, so the zero Config
// is the default configuration and old files keep working as settings grow.
type Choice struct{ Value, Name string }

const (
	LocationAppData  = "appdata"
	LocationPortable = "portable"
)

// ErrExists is returned by Move when overwrite is false and a config file
// already exists at the destination.
var ErrExists = errors.New("目标位置已存在配置文件")

// Config stores the chosen Value of each setting; the package that implements
// a setting also lists its choices.
type Config struct {
	Microphone      string                       `json:"microphone,omitempty"`       // Device ID; empty is the system default.
	Provider        string                       `json:"provider,omitempty"`         // Active ASR provider ID; empty picks first available.
	ProviderOptions map[string]map[string]string `json:"provider_options,omitempty"` // Per-provider option values.
	Typing          string                       `json:"typing,omitempty"`           // session.Typings
	Punctuation     string                       `json:"punctuation,omitempty"`      // session.Punctuations
	AutoStop        string                       `json:"auto_stop,omitempty"`        // session.AutoStops
	HideDelay       string                       `json:"hide_delay,omitempty"`       // Seconds before hiding the bar after dictation; "" defaults to 3, "0" hides immediately.
	Hotkey          string                       `json:"hotkey,omitempty"`           // e.g. "ctrl+alt+space"; see desktop.SetHotkey
	Sound           string                       `json:"sound,omitempty"`            // "" plays chimes, "off" is silent.
	Autostart       string                       `json:"autostart,omitempty"`        // "on" launches at sign-in.
	BarWidth        int                          `json:"bar_width,omitempty"`        // Floating bar width in 96-DPI DIPs; 0 uses default.
	BarHeight       int                          `json:"bar_height,omitempty"`       // Floating bar height in 96-DPI DIPs; 0 uses default.
}

// ProviderOption returns the stored option value for a provider field.
func (c *Config) ProviderOption(providerID, key string) string {
	if c.ProviderOptions == nil {
		return ""
	}
	return c.ProviderOptions[providerID][key]
}

// SetProviderOption stores an option value for a provider field.
func (c *Config) SetProviderOption(providerID, key, value string) {
	if c.ProviderOptions == nil {
		c.ProviderOptions = map[string]map[string]string{}
	}
	if c.ProviderOptions[providerID] == nil {
		c.ProviderOptions[providerID] = map[string]string{}
	}
	if value == "" {
		delete(c.ProviderOptions[providerID], key)
	} else {
		c.ProviderOptions[providerID][key] = value
	}
}

// ProviderOptionMap returns a shallow copy of providerID's options.
func (c Config) ProviderOptionMap(providerID string) map[string]string {
	src := c.ProviderOptions[providerID]
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

var (
	mu       sync.Mutex
	location string // Active location once detected or switched.
	// Overridable in tests.
	userConfigDir = os.UserConfigDir
	executable    = os.Executable
)

func pathFor(loc string) (string, error) {
	if loc == LocationPortable {
		exe, err := executable()
		if err != nil {
			return "", err
		}
		return filepath.Join(filepath.Dir(exe), "config.json"), nil
	}
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "kototype", "config.json"), nil
}

func detectLocationLocked() string {
	if location != "" {
		return location
	}
	if p, err := pathFor(LocationPortable); err == nil {
		if _, err := os.Stat(p); err == nil {
			location = LocationPortable
			return location
		}
	}
	location = LocationAppData
	return location
}

// CurrentLocation returns LocationPortable when using the file next to the
// executable, or LocationAppData otherwise.
func CurrentLocation() string {
	mu.Lock()
	defer mu.Unlock()
	return detectLocationLocked()
}

// PathFor returns the full config file path for loc (LocationAppData or
// LocationPortable).
func PathFor(loc string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	return pathFor(loc)
}

// FilePath returns the full path of the active config file.
func FilePath() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	return pathFor(detectLocationLocked())
}

func writeFile(p string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Load reads from the active config file, returning defaults when absent.
func Load() (Config, error) {
	mu.Lock()
	defer mu.Unlock()
	var c Config
	p, err := pathFor(detectLocationLocked())
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(data, &c)
}

func Save(c Config) error {
	mu.Lock()
	defer mu.Unlock()
	p, err := pathFor(detectLocationLocked())
	if err != nil {
		return err
	}
	return writeFile(p, c)
}

// Move relocates the active config file to target (LocationAppData or
// LocationPortable), persisting c there and removing the previous file.
// If a file already exists at target and overwrite is false, Move returns
// ErrExists without modifying either file.
func Move(target string, c Config, overwrite bool) error {
	mu.Lock()
	defer mu.Unlock()
	cur := detectLocationLocked()
	if target == cur {
		return nil
	}
	src, err := pathFor(cur)
	if err != nil {
		return err
	}
	dst, err := pathFor(target)
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Clean(src), filepath.Clean(dst)) {
		location = target
		return nil
	}
	if _, err := os.Stat(dst); err == nil && !overwrite {
		return ErrExists
	}
	if err := writeFile(dst, c); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if cur == LocationAppData {
		_ = os.Remove(filepath.Dir(src)) // Remove now-empty %AppData%\xvoice if possible.
	}
	location = target
	return nil
}
