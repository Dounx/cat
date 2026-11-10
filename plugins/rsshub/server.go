package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/sidecar"
)

// commit is the RSSHub revision both managed ways run: the Docker image is
// tagged with it, and the "rsshub" dependency in plugin.json is its source.
const commit = "bbc351a29d44f58aa66cfaf4eec5a6af61205ffb"

// dockerImage is RSSHub's official image for that revision. It has no browser
// bundled, so the few routes that need one do not work.
const dockerImage = "diygod/rsshub:" + commit

// containerPort is where RSSHub listens inside the container.
const containerPort = 1200

// builtMarker is written into the source directory once it has been built.
const builtMarker = ".cat-built"

func baseURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// portFree reports whether nothing listens on the loopback port yet. Checking
// first keeps us from mistaking somebody else's server for our RSSHub.
func portFree(port int) error {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("端口 %d 已被占用，请关闭占用它的程序，或重新运行接入向导换一个端口", port)
	}
	return l.Close()
}

// startDocker runs RSSHub in a container. Its port is published on loopback
// only; the access key is required even there.
func startDocker(dataDir string, port int, key string) (*sidecar.Process, error) {
	if err := portFree(port); err != nil {
		return nil, err
	}
	p, err := sidecar.DockerRun(sidecar.ContainerName("cat-rsshub", dataDir), []string{"ACCESS_KEY=" + key},
		"-p", "127.0.0.1:"+strconv.Itoa(port)+":"+strconv.Itoa(containerPort), dockerImage)
	if err != nil {
		return nil, fmt.Errorf("启动 RSSHub 失败：%w", err)
	}
	return p, nil
}

// startLocal runs the RSSHub built in dir with the machine's Node.js,
// listening on loopback only.
func startLocal(dir string, port int, key string) (*sidecar.Process, error) {
	if _, err := os.Stat(filepath.Join(dir, builtMarker)); err != nil {
		return nil, errors.New("RSSHub 还没有构建好，请重新运行接入向导")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errors.New("没有找到 node 命令，请先安装 Node.js")
	}
	if err := portFree(port); err != nil {
		return nil, err
	}
	cmd := exec.Command(node, filepath.Join("dist", "index.mjs"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"NODE_ENV=production", "NODE_OPTIONS=--max-http-header-size=32768",
		"PORT="+strconv.Itoa(port), "LISTEN_INADDR_ANY=0", "ACCESS_KEY="+key,
		"NO_LOGFILES=1", // its console is already in Cat's log
	)
	p, err := sidecar.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("启动 RSSHub 失败：%w", err)
	}
	return p, nil
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func plain(line string) string { return strings.TrimSpace(reANSI.ReplaceAllString(line, "")) }

// awaitReady watches a starting RSSHub until it answers on its port.
func awaitReady(ctx context.Context, p *sidecar.Process, port int, key string, onLine func(string)) error {
	check := baseURL(port) + "/healthz?key=" + url.QueryEscape(key)
	client := &http.Client{Timeout: 3 * time.Second}
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	var tail []string
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.New("RSSHub 迟迟没有启动：\n" + strings.Join(tail, "\n"))
			}
			return ctx.Err()
		case line, ok := <-p.Lines:
			if !ok {
				return errors.New("RSSHub 意外退出：\n" + strings.Join(tail, "\n"))
			}
			if line = plain(line); line == "" {
				continue
			}
			if onLine != nil {
				onLine(line)
			}
			if tail = append(tail, line); len(tail) > 12 {
				tail = tail[1:]
			}
		case <-tick.C:
			req, _ := http.NewRequestWithContext(ctx, "GET", check, nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

// toolchain is what building RSSHub from source needs, as its package.json
// asks for it.
type toolchain struct {
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
	PackageManager string `json:"packageManager"`
}

// nodeSatisfies checks a Node.js version ("v24.15.0") against the simple
// ranges RSSHub uses ("^24.15.0 || ^26.13.0"). A range it cannot read passes.
func nodeSatisfies(version, ranges string) bool {
	triple := func(s string) (v [3]int, ok bool) {
		parts := strings.SplitN(strings.TrimLeft(strings.TrimSpace(s), "v^>=~"), ".", 3)
		for i := range parts {
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return v, false
			}
			v[i] = n
		}
		return v, true
	}
	have, ok := triple(version)
	if !ok || strings.TrimSpace(ranges) == "" {
		return true
	}
	for _, alt := range strings.Split(ranges, "||") {
		alt = strings.TrimSpace(alt)
		want, ok := triple(alt)
		if !ok || !(strings.HasPrefix(alt, "^") || strings.HasPrefix(alt, ">=")) {
			return true
		}
		newer := have[0] > want[0] || (have[0] == want[0] && (have[1] > want[1] || (have[1] == want[1] && have[2] >= want[2])))
		if newer && (have[0] == want[0] || strings.HasPrefix(alt, ">=")) {
			return true
		}
	}
	return false
}

// pnpmCommand finds pnpm, or falls back to fetching the version RSSHub pins
// through npx.
func pnpmCommand(tc toolchain) ([]string, error) {
	if path, err := exec.LookPath("pnpm"); err == nil {
		return []string{path}, nil
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		return nil, errors.New("没有找到 pnpm 或 npx 命令，请先安装 Node.js")
	}
	pkg := "pnpm"
	if v, _, _ := strings.Cut(tc.PackageManager, "+"); strings.HasPrefix(v, "pnpm@") {
		pkg = v
	}
	return []string{npx, "--yes", pkg}, nil
}

// buildLocal installs RSSHub's packages and builds it inside dir, the source
// tree Cat downloaded. That takes a few minutes and is done once per release:
// the directory is shared by every instance of the plugin, and RSSHub keeps
// nothing in it at run time.
func buildLocal(ctx context.Context, dir string, progress func(stage, line string)) error {
	if _, err := os.Stat(filepath.Join(dir, builtMarker)); err == nil {
		return nil
	}
	var tc toolchain
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil || json.Unmarshal(raw, &tc) != nil {
		return errors.New("下载的 RSSHub 源码不完整：读不到 package.json")
	}
	out, err := exec.CommandContext(ctx, "node", "--version").Output()
	if err != nil {
		return errors.New("没有找到 node 命令，请先安装 Node.js " + tc.Engines.Node)
	}
	if have := strings.TrimSpace(string(out)); !nodeSatisfies(have, tc.Engines.Node) {
		return fmt.Errorf("RSSHub 需要 Node.js %s，本机是 %s", tc.Engines.Node, have)
	}
	pnpm, err := pnpmCommand(tc)
	if err != nil {
		return err
	}
	steps := []struct {
		stage string
		args  []string
	}{
		{"正在安装 RSSHub 的依赖包", []string{"install", "--frozen-lockfile"}},
		{"正在构建 RSSHub", []string{"build"}},
	}
	for _, st := range steps {
		progress(st.stage, "")
		cmd := exec.Command(pnpm[0], append(pnpm[1:], st.args...)...)
		cmd.Dir = dir
		// No prompts, and no browser downloads: they are large and optional.
		cmd.Env = append(os.Environ(), "CI=1", "PUPPETEER_SKIP_DOWNLOAD=1", "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1")
		p, err := sidecar.Start(cmd)
		if err != nil {
			return fmt.Errorf("%s失败：%w", strings.TrimPrefix(st.stage, "正在"), err)
		}
		var tail []string
		lines := p.Lines
		for lines != nil {
			select {
			case <-ctx.Done():
				p.Stop()
				return ctx.Err()
			case line, ok := <-lines:
				if !ok {
					lines = nil
					break
				}
				if line = plain(line); line == "" {
					continue
				}
				progress(st.stage, line)
				if tail = append(tail, line); len(tail) > 12 {
					tail = tail[1:]
				}
			}
		}
		if err := p.Wait(); err != nil {
			return fmt.Errorf("%s失败（%v）：\n%s", strings.TrimPrefix(st.stage, "正在"), err, strings.Join(tail, "\n"))
		}
	}
	return os.WriteFile(filepath.Join(dir, builtMarker), []byte(commit), 0o644)
}
