// Package faketest provides cross-platform stand-ins for harness CLIs so the
// harness plugins can be tested without the real tools (or their accounts).
package faketest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binBytes  []byte
	buildErr  error
)

// Bin builds the fake CLI binary once per test run and writes a copy named
// name (with .exe on Windows) into a fresh temp dir. Supported names:
// "claude", "codex", "dsh".
func Bin(t *testing.T, name string) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cat-fakecli-*")
		if err != nil {
			buildErr = err
			return
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "fakecli")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		if b, err := exec.Command("go", "build", "-o", out, "github.com/dounx/cat/plugins/internal/faketest/cmd/fakecli").CombinedOutput(); err != nil {
			buildErr = &execError{err: err, out: string(b)}
			return
		}
		binBytes, buildErr = os.ReadFile(out)
	})
	if buildErr != nil {
		t.Fatalf("build fakecli: %v", buildErr)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, binBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type execError struct {
	err error
	out string
}

func (e *execError) Error() string { return e.err.Error() + "\n" + e.out }

// Log returns a fresh log file path exported to fake CLIs as $FAKE_LOG, and a
// function that reads its lines.
func Log(t *testing.T) (read func() []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_LOG", path)
	return func() []string {
		b, _ := os.ReadFile(path)
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		return strings.Split(strings.TrimSpace(s), "\n")
	}
}
