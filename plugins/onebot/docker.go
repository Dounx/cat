package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// dockerImage is NapCat's official image (github.com/NapNeko/NapCat-Docker),
// pinned to the same release as the "napcat" dependency in plugin.json. It
// bundles Linux QQ, so nothing else has to be installed on the machine.
const dockerImage = "mlikiowa/napcat-docker:v4.18.42"

// Where things live inside the container.
const (
	containerWSPort    = 3001
	containerWebUIPort = 6099
	containerConfigDir = "/app/napcat/config"
	containerQQDir     = "/app/.config/QQ"
)

// containerName is stable per instance, so a container left behind by a
// crash is found and replaced on the next start.
func containerName(dataDir string) string {
	sum := instanceHash(dataDir)
	return "cat-napcat-" + hex.EncodeToString(sum[:6])
}

// containerMAC is a fixed, locally administered address for the instance's
// container. QQ ties a login to the machine it was made on: with the random
// address (and host name) Docker hands out on every run, each start looks
// like a new device and the saved login is refused.
func containerMAC(dataDir string) string {
	sum := instanceHash(dataDir)
	return fmt.Sprintf("02:42:%02x:%02x:%02x:%02x", sum[6], sum[7], sum[8], sum[9])
}

func instanceHash(dataDir string) [sha256.Size]byte {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return sha256.Sum256([]byte(dataDir))
}

// dockerDataDir holds what the container keeps between runs: NapCat's config
// and QQ's login state, each mounted into the container.
func dockerDataDir(dataDir string) string { return filepath.Join(dataDir, "napcat-docker") }

// startDockerNapCat runs NapCat in a container attached to our pipes, so its
// console (QR code links included) is read like a local process's. Only the
// OneBot port is published, and only on loopback; the WebUI stays inside.
func startDockerNapCat(dataDir string, wsPort int, uin string) (*napcat, error) {
	dir, err := filepath.Abs(dockerDataDir(dataDir))
	if err != nil {
		return nil, err
	}
	qqDir := filepath.Join(dir, "qq")
	if err := os.MkdirAll(qqDir, 0o755); err != nil {
		return nil, err
	}
	name := containerName(dataDir)
	_ = exec.Command("docker", "rm", "-f", name).Run() // left over from an unclean exit
	args := []string{"run", "--rm", "--name", name,
		"--hostname", "napcat", "--mac-address", containerMAC(dataDir),
		"-p", "127.0.0.1:" + strconv.Itoa(wsPort) + ":" + strconv.Itoa(containerWSPort),
		"-v", filepath.Join(dir, "config") + ":" + containerConfigDir,
		"-v", qqDir + ":" + containerQQDir,
	}
	if runtime.GOOS != "windows" {
		// Run as the current user, so the mounted files stay ours.
		args = append(args, "-e", "NAPCAT_UID="+strconv.Itoa(os.Getuid()), "-e", "NAPCAT_GID="+strconv.Itoa(os.Getgid()))
	}
	if uin != "" {
		args = append(args, "-e", "ACCOUNT="+uin) // quick login for an account that logged in before
	}
	return startProcess(exec.Command("docker", append(args, dockerImage)...), name)
}

// adoptDockerNapCat takes over the container the setup wizard left running
// and logged in, if there is one. Starting a new one instead would log in a
// second time moments after the first, which QQ answers by asking for
// confirmation on the phone.
func adoptDockerNapCat(ctx context.Context, dataDir string, c *client, uin string) *napcat {
	name := containerName(dataDir)
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done, err := c.connect(actx, nil)
	if err != nil {
		return nil
	}
	info, err := c.login(actx)
	c.close()
	<-done
	if err != nil || uin == "" || info.UserID.String() != uin {
		return nil
	}
	n, err := startProcess(exec.Command("docker", "logs", "-f", "--tail", "0", name), name)
	if err != nil {
		return nil
	}
	return n
}
