// Package sidecar runs the programs some plugins manage on the user's behalf
// (an RSSHub instance for RSSHub): as a local process or in
// a Docker container, with the console output collected line by line.
package sidecar

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Process is a running program, or the docker client attached to a container.
type Process struct {
	cmd       *exec.Cmd
	container string // Docker container name, when it runs in one
	err       error  // how the program exited; valid once Done is closed

	// Lines carries the console output. Nothing blocks when it is not read:
	// lines are dropped instead. It is closed when the program exits.
	Lines chan string
	// Done is closed once the program has exited and Lines is drained.
	Done chan struct{}
}

// Start starts cmd in its own process group and collects its output.
func Start(cmd *exec.Cmd) (*Process, error) {
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	Isolate(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, Lines: make(chan string, 256), Done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		pw.Close()
	}()
	go func() {
		defer close(p.Done)
		defer close(p.Lines)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			select {
			case p.Lines <- sc.Text():
			default: // nobody is reading: drop rather than block the program
			}
		}
	}()
	return p, nil
}

// Stop kills the program and everything it started, and removes its
// container. It is safe on a nil Process.
func (p *Process) Stop() {
	if p == nil {
		return
	}
	if p.container != "" {
		// Killing the docker client would leave the container running.
		_ = exec.Command("docker", "rm", "-f", p.container).Run()
	}
	KillTree(p.cmd)
	<-p.Done
}

// Wait blocks until the program exits and reports how it did.
func (p *Process) Wait() error {
	<-p.Done
	return p.err
}

// FreePort returns a loopback TCP port nothing is listening on right now.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// DockerReady reports why containers cannot be run here, if they cannot.
func DockerReady() error {
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

// DockerImagePresent reports whether the image has been pulled already.
func DockerImagePresent(image string) bool {
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

// DockerPull downloads the image, reporting docker's progress lines.
func DockerPull(ctx context.Context, image string, onLine func(string)) error {
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

// ContainerName is stable per instance, so a container left behind by a
// crash is found and replaced on the next start.
func ContainerName(prefix, dataDir string) string {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	sum := sha256.Sum256([]byte(dataDir))
	return prefix + "-" + hex.EncodeToString(sum[:6])
}

// DockerRun runs a container attached to our pipes, so its console is read
// like a local process's. args are `docker run` options followed by the image
// (and its command); the container is removed when it stops. env ("KEY=value")
// is handed to the container through the docker client's own environment, so
// secrets stay off the command line.
func DockerRun(name string, env []string, args ...string) (*Process, error) {
	_ = exec.Command("docker", "rm", "-f", name).Run() // left over from an unclean exit
	run := []string{"run", "--rm", "--name", name}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		run = append(run, "-e", k)
	}
	cmd := exec.Command("docker", append(run, args...)...)
	cmd.Env = append(os.Environ(), env...)
	p, err := Start(cmd)
	if err != nil {
		return nil, err
	}
	p.container = name
	return p, nil
}
