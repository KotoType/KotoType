package ui

import (
	"github.com/KotoType/KotoType/internal/audio"
	"github.com/KotoType/KotoType/internal/config"
	"github.com/KotoType/KotoType/internal/provider"
	"github.com/KotoType/KotoType/internal/session"
)

// field is one row of the settings form: a label and a drop-down list, toggle
// switch, numeric input or hotkey button. Dynamic provider fields live under "识别".
type field struct {
	section  string // Heading shown above this row.
	hotkey   bool   // A button that records a key combination, instead of a list.
	number   bool   // A numeric text input with a trailing unit hint.
	rebuilds bool   // Changing this field rebuilds the dynamic form rows.
	label    string
	unit     string // Trailing hint for number fields.
	fallback string // Default display value for number fields when stored empty.
	choices  []config.Choice
	// load fetches choices that are slow to get or change at runtime; it runs
	// off the UI thread and its result replaces choices.
	load    func() ([]config.Choice, error)
	missing string // Name for a stored value that is not among the choices.
	get     func(*config.Config) string
	set     func(*config.Config, string)
	enabled func(*config.Config) bool // nil: always.
}

// options returns the choices to list for a stored value, which stays
// selectable even when it is not (or no longer) offered: an unplugged
// microphone or an uninstalled provider.
func (f field) options(choices []config.Choice, value string) (list []config.Choice, selected int) {
	for i, c := range choices {
		if c.Value == value {
			return choices, i
		}
	}
	name := f.missing
	if name == "" {
		name = value
	}
	return append(choices[:len(choices):len(choices)], config.Choice{Value: value, Name: name}), len(choices)
}

func settingsFields(devices func() ([]audio.Device, error), providers []provider.Info, cfg *config.Config) []field {
	var fields []field

	// "识别" section: provider selector + dynamic fields declared by the active provider.
	if len(providers) == 0 {
		fields = append(fields, field{
			section: "识别",
			label:   "识别引擎",
			choices: []config.Choice{{Name: "未检测到模块（请放入 providers 目录）"}},
			get:     func(*config.Config) string { return "" },
			set:     func(*config.Config, string) {},
			enabled: func(*config.Config) bool { return false },
		})
	} else {
		pChoices := make([]config.Choice, len(providers))
		for i, p := range providers {
			pChoices[i] = config.Choice{Value: p.ID, Name: p.Name}
		}
		active, _ := provider.Resolve(providers, cfg.Provider)
		fields = append(fields, field{
			section:  "识别",
			label:    "识别引擎",
			rebuilds: true,
			choices:  pChoices,
			missing:  "已保存的识别引擎（未安装）",
			get: func(c *config.Config) string {
				if c.Provider == "" {
					return active.ID
				}
				return c.Provider
			},
			set: func(c *config.Config, v string) { c.Provider = v },
		})
		pid := active.ID
		for _, pf := range active.Fields {
			pf := pf
			var en func(*config.Config) bool
			if pf.When != nil {
				cond := *pf.When
				en = func(c *config.Config) bool { return c.ProviderOption(pid, cond.Key) == cond.Value }
			}
			fields = append(fields, field{
				label:   pf.Label,
				choices: pf.Choices,
				get:     func(c *config.Config) string { return c.ProviderOption(pid, pf.Key) },
				set:     func(c *config.Config, v string) { c.SetProviderOption(pid, pf.Key, v) },
				enabled: en,
			})
		}
	}

	fields = append(fields,
		field{
			section: "输入", label: "麦克风",
			choices: []config.Choice{{Name: "系统默认麦克风"}},
			load: func() ([]config.Choice, error) {
				list, err := devices()
				choices := []config.Choice{{Name: "系统默认麦克风"}}
				for _, d := range list {
					if d.Default {
						d.Name += "（当前默认）"
					}
					choices = append(choices, config.Choice{Value: d.ID, Name: d.Name})
				}
				return choices, err
			},
			missing: "已保存的麦克风（未连接）",
			get:     func(c *config.Config) string { return c.Microphone },
			set:     func(c *config.Config, v string) { c.Microphone = v },
		},
		field{
			label: "上屏方式", choices: session.Typings,
			get: func(c *config.Config) string { return c.Typing },
			set: func(c *config.Config, v string) { c.Typing = v },
		},
		field{
			label: "标点", choices: session.Punctuations,
			get: func(c *config.Config) string { return c.Punctuation },
			set: func(c *config.Config, v string) { c.Punctuation = v },
		},
		field{
			label: "自动结束", choices: session.AutoStops,
			get: func(c *config.Config) string { return c.AutoStop },
			set: func(c *config.Config, v string) { c.AutoStop = v },
		},
		field{
			label: "自动隐藏", number: true, unit: "秒（0 立即隐藏）", fallback: "3",
			get: func(c *config.Config) string { return c.HideDelay },
			set: func(c *config.Config, v string) { c.HideDelay = v },
		},
		field{
			section: "常规", label: "快捷键", hotkey: true,
			get: func(c *config.Config) string { return c.Hotkey },
			set: func(c *config.Config, v string) { c.Hotkey = v },
		},
		field{
			label:   "提示音",
			choices: []config.Choice{{Name: "开启"}, {Value: "off", Name: "关闭"}},
			get:     func(c *config.Config) string { return c.Sound },
			set:     func(c *config.Config, v string) { c.Sound = v },
		},
		field{
			label:   "开机启动",
			choices: []config.Choice{{Name: "关闭"}, {Value: "on", Name: "开启"}},
			get:     func(c *config.Config) string { return c.Autostart },
			set:     func(c *config.Config, v string) { c.Autostart = v },
		},
	)
	return fields
}
