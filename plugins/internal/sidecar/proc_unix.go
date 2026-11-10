//go:build !windows

package sidecar

import (
	"os/exec"
	"syscall"
)

// Isolate puts cmd in its own process group, so KillTree reaches everything
// it starts.
func Isolate(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// KillTree kills cmd and everything it started.
func KillTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
