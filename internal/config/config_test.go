package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMoveLocation(t *testing.T) {
	tmp := t.TempDir()
	appDir := filepath.Join(tmp, "appdata")
	exeDir := filepath.Join(tmp, "exe")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	origUser, origExe := userConfigDir, executable
	t.Cleanup(func() {
		mu.Lock()
		userConfigDir, executable, location = origUser, origExe, ""
		mu.Unlock()
	})
	userConfigDir = func() (string, error) { return appDir, nil }
	executable = func() (string, error) { return filepath.Join(exeDir, "kototype.exe"), nil }
	location = ""

	if got := CurrentLocation(); got != LocationAppData {
		t.Fatalf("initial location = %q, want %q", got, LocationAppData)
	}
	cfg := Config{Hotkey: "ctrl+alt+h", BarWidth: 320, BarHeight: 120}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	appFile := filepath.Join(appDir, "kototype", "config.json")
	portFile := filepath.Join(exeDir, "config.json")

	// Move to portable directory.
	if err := Move(LocationPortable, cfg, false); err != nil {
		t.Fatalf("Move to portable: %v", err)
	}
	if got := CurrentLocation(); got != LocationPortable {
		t.Fatalf("location after move = %q, want %q", got, LocationPortable)
	}
	if _, err := os.Stat(portFile); err != nil {
		t.Fatalf("portable file missing: %v", err)
	}
	if _, err := os.Stat(appFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("appdata file should have been removed, err=%v", err)
	}

	// Place a conflicting file in appdata and verify ErrExists guard.
	if err := os.MkdirAll(filepath.Dir(appFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appFile, []byte(`{"hotkey":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Move(LocationAppData, cfg, false); !errors.Is(err, ErrExists) {
		t.Fatalf("Move without overwrite = %v, want ErrExists", err)
	}
	if got := CurrentLocation(); got != LocationPortable {
		t.Fatalf("location changed after refused overwrite: %q", got)
	}

	// Confirm overwrite and verify reload picks up appdata.
	if err := Move(LocationAppData, cfg, true); err != nil {
		t.Fatalf("Move with overwrite: %v", err)
	}
	if _, err := os.Stat(portFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("portable file should have been removed, err=%v", err)
	}
	location = "" // Simulate fresh startup.
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, cfg) || CurrentLocation() != LocationAppData {
		t.Fatalf("reloaded = %+v (%q), want %+v (%q)", loaded, CurrentLocation(), cfg, LocationAppData)
	}
}
