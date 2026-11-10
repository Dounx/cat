//go:build !windows

package main

import (
	"os/signal"
	"syscall"
)

// detach makes the process behave like a container rather than a child of
// the docker client: it leaves the client's process group, and survives the
// client going away with its console.
func detach() {
	_, _ = syscall.Setsid()
	signal.Ignore(syscall.SIGPIPE)
}
