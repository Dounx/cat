package update

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

// waitEnv names the process a newly started copy waits for before it goes
// on: the one it replaces, which still holds the listening address and the
// single-instance lock until it is gone.
const waitEnv = "CAT_WAIT_PID"

// Spawn starts exe as a process of its own that outlives this one. The copy
// waits in AwaitPredecessor until this process has exited, so the caller
// quits right after.
func Spawn(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), waitEnv+"="+strconv.Itoa(os.Getpid()))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// AwaitPredecessor is called first thing by a shell: when this process was
// started by Spawn it waits here, for at most half a minute, until the
// process it replaces is gone.
func AwaitPredecessor() {
	pid, err := strconv.Atoi(os.Getenv(waitEnv))
	_ = os.Unsetenv(waitEnv)
	if err != nil || pid <= 0 {
		return
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		awaitExit(pid)
	}()
	select {
	case <-gone:
	case <-time.After(30 * time.Second):
	}
}
