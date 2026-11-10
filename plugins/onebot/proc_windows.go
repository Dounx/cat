//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

func isolate(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
