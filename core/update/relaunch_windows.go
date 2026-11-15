package update

import (
	"os"
	"os/exec"
)

// Replace starts exe anew and ends this process: Windows cannot swap the
// program of a running process. It only returns when that failed. The caller
// has shut everything down first.
func Replace(exe string, args []string) error {
	if err := Spawn(exe, args); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}

func detach(*exec.Cmd) {}

func awaitExit(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_, _ = p.Wait()
}
