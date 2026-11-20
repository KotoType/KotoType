package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDiscoverAndRecognizeMockProvider(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mockSrc := `package main
import (
	"bufio"
	"fmt"
	"io"
	"os"
)
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--manifest" {
		fmt.Println(` + "`" + `{"id":"mock","name":"Mock Provider","version":"0.1.0","fields":[{"key":"lang","label":"语言","choices":[{"value":"","name":"默认"}]}]}` + "`" + `)
		return
	}
	r := bufio.NewReader(os.Stdin)
	_, _ = r.ReadBytes('\n')
	_, _ = io.Copy(io.Discard, r)
	fmt.Println(` + "`" + `{"text":"hello","stable":5}` + "`" + `)
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(mockSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	provDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(provDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exeName := "kototype-provider-mock"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	out := filepath.Join(provDir, exeName)
	cmd := exec.Command("go", "build", "-o", out, filepath.Join(srcDir, "main.go"))
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock provider: %v\n%s", err, msg)
	}

	list := DiscoverIn([]string{provDir})
	if len(list) != 1 || list[0].ID != "mock" || len(list[0].Fields) != 1 {
		t.Fatalf("unexpected providers: %+v", list)
	}
	var gotText string
	var gotStable int
	err := Recognize(context.Background(), list[0].Path, Request{Options: map[string]string{"lang": ""}},
		func(ctx context.Context, send func([]byte) error, ready func()) error {
			ready()
			return send(make([]byte, 640))
		},
		func(text string, stable int) { gotText, gotStable = text, stable },
		func() {},
	)
	if err != nil || gotText != "hello" || gotStable != 5 {
		t.Fatalf("Recognize: err=%v text=%q stable=%d", err, gotText, gotStable)
	}
}
