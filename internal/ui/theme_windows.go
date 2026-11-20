package ui

import (
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// Windows 11 Fluent colours, following the system light/dark setting and accent.

type theme struct {
	dark               bool
	accent             rgba // Primary buttons, switches, active indicators.
	accentHover        rgba
	accentPressed      rgba
	onAccent           rgba // Text and glyphs on accent.
	window             rgba // Fallback background where Mica is unavailable.
	card               rgba // Grouped settings card background.
	cardAlt            rgba // Recessed panel (test transcript box).
	border             rgba // Card outlines.
	divider            rgba // Hairline separators inside cards and menus.
	control            rgba // Drop-downs and standard buttons.
	controlBorder      rgba // Outline for interactive controls.
	controlHoverBorder rgba // Highlighted outline when a control is hovered.
	hover              rgba // Control hover fill.
	pressed            rgba // Control pressed fill.
	itemHover          rgba // Subtle list/menu/icon hover fill.
	itemPressed        rgba // Subtle list/menu/icon pressed fill.
	text               rgba
	subtle             rgba // Secondary text.
	disabled           rgba
	flyout             rgba // Floating bar and popup menu background.
	ok                 rgba // Positive status dot.
	warn               rgba // Attention status dot.
	danger             rgba // Error message text.
}

var th theme

// loadTheme reads the current Windows light/dark preference and accent palette.
func loadTheme() {
	light := uint64(1)
	if key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE); err == nil {
		if v, _, err := key.GetIntegerValue("AppsUseLightTheme"); err == nil {
			light = v
		}
		key.Close()
	}
	// AccentPalette holds eight RGBA shades from lightest to darkest; the
	// light theme uses Dark1, the dark theme Light2, like Windows itself.
	accentLight, accentDark := hex(0x005fb8), hex(0x60cdff)
	if key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Explorer\Accent`, registry.QUERY_VALUE); err == nil {
		if palette, _, err := key.GetBinaryValue("AccentPalette"); err == nil && len(palette) >= 32 {
			shade := func(i int) rgba {
				return hex(uint32(palette[i*4])<<16 | uint32(palette[i*4+1])<<8 | uint32(palette[i*4+2]))
			}
			accentDark, accentLight = shade(1), shade(4)
		}
		key.Close()
	}
	if light == 0 {
		th = theme{
			dark:               true,
			accent:             accentDark,
			accentHover:        accentDark.mix(hex(0xffffff), 0.14),
			accentPressed:      accentDark.mix(hex(0x000000), 0.18),
			onAccent:           hex(0x000000),
			window:             hex(0x202020),
			card:               hex(0xffffff).alpha(0.055),
			cardAlt:            hex(0x000000).alpha(0.22),
			border:             hex(0xffffff).alpha(0.085),
			divider:            hex(0xffffff).alpha(0.075),
			control:            hex(0xffffff).alpha(0.065),
			controlBorder:      hex(0xffffff).alpha(0.13),
			controlHoverBorder: accentDark.alpha(0.55),
			hover:              hex(0xffffff).alpha(0.135),
			pressed:            hex(0xffffff).alpha(0.045),
			itemHover:          hex(0xffffff).alpha(0.10),
			itemPressed:        hex(0xffffff).alpha(0.06),
			text:               hex(0xffffff),
			subtle:             hex(0xffffff).alpha(0.76),
			disabled:           hex(0xffffff).alpha(0.36),
			flyout:             hex(0x2c2c2c),
			ok:                 hex(0x6ccb5f),
			warn:               hex(0xfce100),
			danger:             hex(0xff99a4),
		}
	} else {
		th = theme{
			accent:             accentLight,
			accentHover:        accentLight.mix(hex(0xffffff), 0.16),
			accentPressed:      accentLight.mix(hex(0x000000), 0.14),
			onAccent:           hex(0xffffff),
			window:             hex(0xf3f3f3),
			card:               hex(0xffffff).alpha(0.45),
			cardAlt:            hex(0x000000).alpha(0.03),
			border:             hex(0x000000).alpha(0.075),
			divider:            hex(0x000000).alpha(0.06),
			control:            hex(0xffffff).alpha(0.56),
			controlBorder:      hex(0x000000).alpha(0.14),
			controlHoverBorder: accentLight.alpha(0.50),
			hover:              hex(0x000000).alpha(0.055),
			pressed:            hex(0x000000).alpha(0.095),
			itemHover:          hex(0x000000).alpha(0.065),
			itemPressed:        hex(0x000000).alpha(0.105),
			text:               hex(0x000000).alpha(0.896),
			subtle:             hex(0x000000).alpha(0.606),
			disabled:           hex(0x000000).alpha(0.361),
			flyout:             hex(0xfcfcfc),
			ok:                 hex(0x0f7b0f),
			warn:               hex(0x9d5d00),
			danger:             hex(0xc42b1c),
		}
	}
}

// styleFrame applies the current dark/light DWM frame and optional Mica backdrop.
// It reports whether Mica is active.
func styleFrame(hwnd uintptr, mica bool) bool {
	if hwnd == 0 {
		return false
	}
	dark := int32(0)
	if th.dark {
		dark = 1
	}
	dwmSetAttribute.Call(hwnd, 20, uintptr(unsafe.Pointer(&dark)), 4) // DWMWA_USE_IMMERSIVE_DARK_MODE
	if !mica {
		return false
	}
	backdrop := int32(2)                                                                                  // DWMSBT_MAINWINDOW
	if hr, _, _ := dwmSetAttribute.Call(hwnd, 38, uintptr(unsafe.Pointer(&backdrop)), 4); int32(hr) < 0 { // DWMWA_SYSTEMBACKDROP_TYPE
		return false // Before Windows 11 22H2.
	}
	margins := [4]int32{-1, -1, -1, -1}
	dwmExtendFrame.Call(hwnd, uintptr(unsafe.Pointer(&margins)))
	return true
}

// themeChanged reloads system colours and updates every window.
func themeChanged() {
	loadTheme()
	updateTestEditTheme()
	settingsMica = styleFrame(settingsWnd, true)
	aboutMica = styleFrame(aboutWnd, true)
	styleFrame(barWnd, false)
	styleFrame(popupWnd, false)
	for _, hwnd := range []uintptr{settingsWnd, aboutWnd, barWnd, popupWnd, testEditWnd, hideEditWnd} {
		if hwnd != 0 {
			invalidateRect.Call(hwnd, 0, 1)
		}
	}
}
