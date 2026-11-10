//go:build windows

package sidecar

import (
	"os/exec"
	"strconv"
)

// Isolate is a no-op on Windows: taskkill /T follows the process tree.
func Isolate(cmd *exec.Cmd) {}

// KillTree kills cmd and everything it started.
func KillTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
