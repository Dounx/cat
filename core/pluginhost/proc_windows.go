//go:build windows

package pluginhost

import (
	"os/exec"
	"strconv"
)

func isolate(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
