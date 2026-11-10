package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// napcat is a NapCat process managed by this plugin.
type napcat struct {
	cmd   *exec.Cmd
	lines chan string // console output, line by line
	done  chan struct{}
}

var qrURL = regexp.MustCompile(`https://txz\.qq\.com/\S+`)

// installNapCat copies the downloaded release into the instance's own
// directory (NapCat keeps its config and login state next to its files, so
// each instance needs a private copy). Re-copying only happens when the
// release changes; config and login state are kept.
func installNapCat(depDir, dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "napcat")
	marker := filepath.Join(dir, ".cat-source")
	if b, err := os.ReadFile(marker); err == nil && string(b) == depDir {
		return dir, nil
	}
	err := filepath.WalkDir(depDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(depDir, path)
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if rel == ".installed" || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		return "", fmt.Errorf("准备 NapCat 失败：%w", err)
	}
	return dir, os.WriteFile(marker, []byte(depDir), 0o644)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// writeNapCatConfig writes the default OneBot config NapCat applies to any
// account that logs in (config/onebot11.json), exposing one forward WebSocket
// server on loopback, and confines the WebUI to loopback too.
func writeNapCatConfig(dir string, wsPort int, token string, webuiPort int, webuiToken string) error {
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	onebot := map[string]any{
		"network": map[string]any{
			"httpServers": []any{}, "httpClients": []any{}, "websocketClients": []any{},
			"websocketServers": []any{map[string]any{
				"name": "cat", "enable": true, "host": "127.0.0.1", "port": wsPort, "token": token,
				"messagePostFormat": "array", "reportSelfMessage": false, "enableForcePushEvent": true,
				"debug": false, "heartInterval": 30000,
			}},
		},
		"musicSignUrl": "", "enableLocalFile2Url": false, "parseMultMsg": false,
	}
	webui := map[string]any{"host": "127.0.0.1", "port": webuiPort, "token": webuiToken, "loginRate": 3}
	for name, v := range map[string]any{"onebot11.json": onebot, "webui.json": webui} {
		b, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(filepath.Join(cfgDir, name), b, 0o600); err != nil {
			return err
		}
	}
	// Per-account files written by an earlier login would override the
	// default; rewrite their network section so the ports stay in step.
	matches, _ := filepath.Glob(filepath.Join(cfgDir, "onebot11_*.json"))
	for _, m := range matches {
		b, _ := json.MarshalIndent(onebot, "", "  ")
		_ = os.WriteFile(m, b, 0o600)
	}
	return nil
}

// launcherCommand picks how to start NapCat from its directory.
func launcherCommand(dir, uin string) (*exec.Cmd, error) {
	var args []string
	if uin != "" {
		args = append(args, uin) // quick login for an account that logged in before
	}
	if runtime.GOOS == "windows" {
		name := "launcher.bat"
		if windowsBuild() < 22000 {
			name = "launcher-win10.bat"
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("NapCat 目录里没有 %s", name)
		}
		return exec.Command("cmd", append([]string{"/C", name}, args...)...), nil
	}
	script := filepath.Join(dir, "launcher.sh")
	if _, err := os.Stat(script); err != nil {
		return nil, errors.New("这个系统上暂不支持自动启动 NapCat")
	}
	return exec.Command("sh", append([]string{script}, args...)...), nil
}

func windowsBuild() int {
	out, err := exec.Command("cmd", "/C", "ver").Output()
	if err != nil {
		return 99999
	}
	if m := regexp.MustCompile(`\d+\.\d+\.(\d+)`).FindSubmatch(out); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	return 99999
}

// qqInstalled reports whether the QQ client NapCat's Shell needs is present.
func qqInstalled() bool {
	if runtime.GOOS != "windows" {
		return true
	}
	for _, key := range []string{
		`HKLM\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\QQ`,
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\QQ`,
	} {
		if exec.Command("reg", "query", key).Run() == nil {
			return true
		}
	}
	return false
}

func startNapCat(dir, uin string) (*napcat, error) {
	cmd, err := launcherCommand(dir, uin)
	if err != nil {
		return nil, err
	}
	cmd.Dir = dir
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	isolate(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 NapCat 失败：%w", err)
	}
	n := &napcat{cmd: cmd, lines: make(chan string, 256), done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		pw.Close()
	}()
	go func() {
		defer close(n.done)
		defer close(n.lines)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			select {
			case n.lines <- sc.Text():
			default: // nobody is reading: drop rather than block NapCat
			}
		}
	}()
	return n, nil
}

func (n *napcat) stop() {
	if n == nil {
		return
	}
	killTree(n.cmd)
	<-n.done
}

// awaitLogin watches NapCat until its OneBot server accepts our connection
// and reports a logged-in account. onQR is called with each login QR code
// link NapCat prints.
func awaitLogin(ctx context.Context, n *napcat, c *client, onQR func(url string), onLine func(string)) (loginInfo, error) {
	var tail []string
	for {
		select {
		case <-ctx.Done():
			return loginInfo{}, ctx.Err()
		case line, ok := <-n.lines:
			if !ok {
				return loginInfo{}, errors.New("NapCat 意外退出：\n" + strings.Join(tail, "\n"))
			}
			if onLine != nil {
				onLine(line)
			}
			if tail = append(tail, line); len(tail) > 12 {
				tail = tail[1:]
			}
			if u := qrURL.FindString(line); u != "" && onQR != nil {
				onQR(u)
			}
			continue
		default:
		}
		// The OneBot server only starts listening once an account is logged in.
		if done, err := c.connect(ctx, nil); err == nil {
			info, lerr := c.login(ctx)
			c.close()
			<-done
			if lerr == nil && info.UserID.String() != "" && info.UserID.String() != "0" {
				return info, nil
			}
		}
		select {
		case <-ctx.Done():
			return loginInfo{}, ctx.Err()
		case line, ok := <-n.lines:
			if ok {
				if onLine != nil {
					onLine(line)
				}
				if tail = append(tail, line); len(tail) > 12 {
					tail = tail[1:]
				}
				if u := qrURL.FindString(line); u != "" && onQR != nil {
					onQR(u)
				}
			}
		case <-n.done:
			// drain what is left on the next loop iteration
		case <-timeAfter():
		}
	}
}
