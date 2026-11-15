//go:build !windows

package update

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Replace turns this process into a fresh run of exe: the same process id,
// terminal and supervisor, so a service manager sees nothing stop. It only
// returns when that failed. The caller has shut everything down first.
func Replace(exe string, args []string) error {
	return syscall.Exec(exe, append([]string{exe}, args...), os.Environ())
}

// A session of its own: closing the terminal the old process was started
// from does not take the new one with it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func awaitExit(pid int) {
	for {
		// EPERM means it is there, and someone else's.
		if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
