// Package provider discovers and runs external ASR provider executables.
//
// Protocol:
//   - `<plugin> --manifest` prints a JSON Manifest to stdout and exits 0.
//   - `<plugin> --recognize` reads one JSON Request line from stdin, followed by
//     raw 16 kHz mono 16-bit LE PCM bytes until EOF, and writes newline-delimited
//     JSON Event lines to stdout.
package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/KotoType/KotoType/internal/config"
)

// ErrSilence ends dictation when the provider reports prolonged silence.
var ErrSilence = errors.New("长时间没有说话，已自动停止")

// Recorder streams 16 kHz mono 16-bit PCM to send until ctx ends, calling ready
// once the microphone is open.
type Recorder func(ctx context.Context, send func([]byte) error, ready func()) error

// Condition enables a field only when another field equals Value.
type Condition struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Field describes one provider-specific setting rendered dynamically in the UI.
type Field struct {
	Key     string          `json:"key"`
	Label   string          `json:"label"`
	Type    string          `json:"type,omitempty"` // "select" (default)
	Choices []config.Choice `json:"choices,omitempty"`
	When    *Condition      `json:"when,omitempty"`
}

// Manifest is returned by `<plugin> --manifest`.
type Manifest struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Version string  `json:"version,omitempty"`
	Fields  []Field `json:"fields,omitempty"`
}

// Info pairs a discovered provider's manifest with its executable path.
type Info struct {
	Manifest
	Path string
}

// Request is the first JSON line written to `<plugin> --recognize` stdin.
type Request struct {
	Options    map[string]string `json:"options,omitempty"`
	EndPauseMs int64             `json:"end_pause_ms,omitempty"`
}

// Event is one JSON line written by `<plugin> --recognize` to stdout.
type Event struct {
	Text    string `json:"text,omitempty"`
	Stable  int    `json:"stable,omitempty"`
	Error   string `json:"error,omitempty"`
	Silence bool   `json:"silence,omitempty"`
}

// SearchDirs returns the directories scanned for provider executables in
// priority order: `<exe dir>/providers` first, then `<config dir>/providers`.
func SearchDirs() []string {
	var dirs []string
	add := func(d string) {
		if d == "" {
			return
		}
		d = filepath.Clean(d)
		for _, existing := range dirs {
			if strings.EqualFold(existing, d) {
				return
			}
		}
		dirs = append(dirs, d)
	}
	if exe, err := os.Executable(); err == nil {
		add(filepath.Join(filepath.Dir(exe), "providers"))
	}
	if cfgFile, err := config.FilePath(); err == nil {
		add(filepath.Join(filepath.Dir(cfgFile), "providers"))
	}
	return dirs
}

// Discover scans SearchDirs for `kototype-provider-*` executables and probes
// each via `--manifest`. Duplicate IDs keep the higher-priority directory entry.
func Discover() []Info { return DiscoverIn(SearchDirs()) }

// DiscoverIn scans dirs in order and returns all valid provider manifests.
func DiscoverIn(dirs []string) []Info {
	seen := map[string]bool{}
	var list []Info
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			low := strings.ToLower(name)
			if !strings.HasPrefix(low, "kototype-provider-") {
				continue
			}
			if runtime.GOOS == "windows" && !strings.HasSuffix(low, ".exe") {
				continue
			}
			full := filepath.Join(dir, name)
			m, err := probeManifest(full)
			if err != nil || m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			if m.Name == "" {
				m.Name = m.ID
			}
			list = append(list, Info{Manifest: m, Path: full})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

func probeManifest(exePath string) (Manifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exePath, "--manifest")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(out, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Resolve picks the configured provider by id, or the first discovered one
// when id is empty.
func Resolve(list []Info, id string) (Info, bool) {
	if len(list) == 0 {
		return Info{}, false
	}
	if id != "" {
		for _, p := range list {
			if p.ID == id {
				return p, true
			}
		}
	}
	return list[0], true
}

// Recognize runs the provider executable for one dictation session.
func Recognize(ctx context.Context, exePath string, req Request, record Recorder, onText func(text string, stable int), ready func()) error {
	cmd := exec.CommandContext(ctx, exePath, "--recognize")
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("初始化识别模块输入失败：%w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("初始化识别模块输出失败：%w", err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("启动识别模块失败：%w", err)
	}

	header, err := json.Marshal(req)
	if err != nil {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	if _, err := stdin.Write(append(header, '\n')); err != nil {
		stdin.Close()
		_ = cmd.Wait()
		return fmt.Errorf("发送识别参数失败：%w", err)
	}

	readDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var lastErr error
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			if ev.Silence {
				lastErr = ErrSilence
				continue
			}
			if ev.Error != "" {
				lastErr = errors.New(ev.Error)
				continue
			}
			if ev.Text != "" {
				onText(ev.Text, ev.Stable)
			}
		}
		if lastErr == nil {
			if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
				lastErr = err
			}
		}
		readDone <- lastErr
	}()

	recErr := record(ctx, func(pcm []byte) error {
		if len(pcm) == 0 {
			return nil
		}
		_, err := stdin.Write(pcm)
		return err
	}, ready)
	_ = stdin.Close()

	evErr := <-readDone
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil
	}
	if evErr != nil {
		return evErr
	}
	// Broken pipe on stdin is expected when the provider ended early (e.g. EndPause).
	if recErr != nil && !errors.Is(recErr, os.ErrClosed) && !strings.Contains(strings.ToLower(recErr.Error()), "pipe") && !strings.Contains(strings.ToLower(recErr.Error()), "closed") {
		return recErr
	}
	if waitErr != nil {
		return fmt.Errorf("识别模块异常退出：%w", waitErr)
	}
	return nil
}
