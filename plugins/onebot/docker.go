package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// dockerReady reports why NapCat cannot be run in Docker here, if it cannot.
func dockerReady() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("没有找到 docker 命令。请先安装并启动 Docker（Docker Desktop、OrbStack 等）")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return errors.New("Docker 没有在运行，请先启动它：" + lastLine(out))
	}
	return nil
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func dockerImagePresent(image string) bool {
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

// dockerPull downloads the image, reporting docker's progress lines.
func dockerPull(ctx context.Context, image string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("拉取镜像 %s 失败：%w", image, err)
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if onLine != nil {
			onLine(sc.Text())
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("拉取镜像 %s 失败：%s", image, lastLine([]byte(stderr.String())))
	}
	return nil
}

// containerName is stable per instance, so a container left behind by a
// crash is found and replaced on the next start.
func containerName(dataDir string) string {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	sum := sha256.Sum256([]byte(dataDir))
	return "cat-napcat-" + hex.EncodeToString(sum[:6])
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
	n, err := startProcess(exec.Command("docker", append(args, dockerImage)...))
	if err != nil {
		return nil, err
	}
	n.container = name
	return n, nil
}
