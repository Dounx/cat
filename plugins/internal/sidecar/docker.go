package sidecar

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// dockerWait is how long a Docker we launched gets to come up. Docker Desktop
// boots a VM, which takes a while on a cold machine.
const dockerWait = 2 * time.Minute

// DockerUp makes sure containers can be run, launching Docker when it is
// installed but not running and waiting for it to come up. onLaunch, if not
// nil, is called once when a launch was attempted, before the wait.
func DockerUp(ctx context.Context, onLaunch func()) error {
	err := DockerReady()
	if err == nil {
		return nil
	}
	if _, lerr := exec.LookPath("docker"); lerr != nil || !launchDocker() {
		return err
	}
	if onLaunch != nil {
		onLaunch()
	}
	deadline := time.NewTimer(dockerWait)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return err
		case <-tick.C:
			if DockerReady() == nil {
				return nil
			}
		}
	}
}

// launchDocker asks whichever Docker is installed here to start, and reports
// whether something took the request. It does not wait for the daemon.
func launchDocker() bool {
	for _, c := range dockerLaunchers(runtime.GOOS, dockerContext()) {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if exec.Command(c[0], c[1:]...).Run() == nil {
			return true
		}
	}
	return false
}

// dockerContext is the docker CLI's current context ("desktop-linux",
// "orbstack", "colima", …), which says whose daemon it is waiting for.
func dockerContext() string {
	out, err := exec.Command("docker", "context", "show").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerLaunchers lists the commands that may start Docker on this system,
// the one matching the current context first.
func dockerLaunchers(goos, dockerCtx string) [][]string {
	switch goos {
	case "darwin":
		desktop := []string{"open", "-ga", "Docker"}
		orbstack := []string{"open", "-ga", "OrbStack"}
		rancher := []string{"open", "-ga", "Rancher Desktop"}
		colima := []string{"colima", "start"}
		switch {
		case strings.HasPrefix(dockerCtx, "orbstack"):
			return [][]string{orbstack}
		case strings.HasPrefix(dockerCtx, "colima"):
			return [][]string{colima}
		case strings.HasPrefix(dockerCtx, "rancher-desktop"):
			return [][]string{rancher}
		case strings.HasPrefix(dockerCtx, "desktop"):
			return [][]string{desktop}
		}
		return [][]string{desktop, orbstack, rancher, colima}
	case "windows":
		l := [][]string{{"docker", "desktop", "start"}}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			exe := filepath.Join(pf, "Docker", "Docker", "Docker Desktop.exe")
			l = append(l, []string{"cmd", "/c", "start", "", exe})
		}
		return l
	default:
		desktop := [][]string{
			{"docker", "desktop", "start"},
			{"systemctl", "--user", "start", "docker-desktop"},
		}
		engine := [][]string{
			{"systemctl", "--user", "start", "docker"}, // rootless
			{"systemctl", "--no-ask-password", "start", "docker"},
		}
		if strings.HasPrefix(dockerCtx, "desktop") {
			return append(desktop, engine...)
		}
		return append(engine, desktop...)
	}
}
