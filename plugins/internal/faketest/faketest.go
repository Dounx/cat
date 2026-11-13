// Package faketest provides stand-ins for harness CLIs so the harness plugins can
// be tested without the real tools (or their accounts).
package faketest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Script writes an executable shell script and returns its path. Tests using
// it are skipped on Windows.
func Script(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fakes are not supported on Windows")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Log returns a fresh log file path exported to scripts as $FAKE_LOG, and a
// function that reads its lines.
func Log(t *testing.T) (read func() []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_LOG", path)
	return func() []string {
		b, _ := os.ReadFile(path)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// Claude is a fake `claude`: it logs its arguments, prompt and endpoint, and
// answers with a result object. Resuming session "gone" fails the way the real
// CLI does; a prompt containing "explode" yields an error result.
const Claude = `
if [ "$1" = "--version" ]; then echo "9.9.9 (Claude Code)"; exit 0; fi
prompt=$(cat)
echo "ARGS:$*" >> "$FAKE_LOG"
echo "PROMPT:$prompt" >> "$FAKE_LOG"
echo "ENV:$ANTHROPIC_BASE_URL|$ANTHROPIC_AUTH_TOKEN|$ANTHROPIC_MODEL" >> "$FAKE_LOG"
echo "CWD:$(pwd)" >> "$FAKE_LOG"
case "$*" in *"--resume gone"*)
  echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: gone","session_id":""}'
  exit 1;;
esac
case "$prompt" in *explode*)
  echo '{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate","session_id":"sess-err"}'
  exit 1;;
esac
n=$(grep -c '^ARGS:' "$FAKE_LOG")
echo "some warning on stdout"
echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"reply $n\",\"session_id\":\"sess-$n\",\"total_cost_usd\":0.01,\"num_turns\":1,\"duration_ms\":5}"
`
