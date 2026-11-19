//go:build windows

package builtin

import (
	"os/exec"
	"time"
)

func configureShellCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = 500 * time.Millisecond
}
