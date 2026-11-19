package builtin

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/dounx/cat/core/plugin"
)

func TestShellCancelKillsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process-group kill test")
	}
	s := &shell{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := s.Invoke(ctx, "run", map[string]any{"command": "sleep 60"}, plugin.InvokeContext{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected cancelled error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancel took too long (%v); child process leaked", elapsed)
	}
}
