//go:build !windows

package pluginhost

import (
	"os/exec"
	"syscall"
)

// Plugins run in their own process group so that stopping one also stops
// whatever it spawned (a managed dependency, a harness CLI).
func isolate(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
