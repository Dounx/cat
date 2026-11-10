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
	"sync"
	"time"
)

// napcat is a NapCat process managed by this plugin.
type napcat struct {
	cmd       *exec.Cmd
	container string      // Docker container name, when it runs in one
	lines     chan string // console output, line by line
	done      chan struct{}
	stopOnce  sync.Once
}

// live holds every NapCat this process started and has not stopped, so that
// none outlives it: the host stops a plugin without waiting for a wizard in
// progress to unwind (the process would exit halfway through the wizard's own
// cleanup), and NapCat runs outside the plugin's process group.
var live = struct {
	sync.Mutex
	m map[*napcat]struct{}
}{m: map[*napcat]struct{}{}}

func stopAllNapCat() {
	live.Lock()
	all := make([]*napcat, 0, len(live.m))
	for n := range live.m {
		all = append(all, n)
	}
	live.Unlock()
	for _, n := range all {
		n.stop()
	}
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
// server on host:wsPort, and confines the WebUI to loopback. host is loopback
// except inside a container, where the port is published on loopback instead.
func writeNapCatConfig(dir, host string, wsPort int, token string, webuiPort int, webuiToken string) error {
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	onebot := map[string]any{
		"network": map[string]any{
			"httpServers": []any{}, "httpClients": []any{}, "websocketClients": []any{},
			"websocketServers": []any{map[string]any{
				"name": "cat", "enable": true, "host": host, "port": wsPort, "token": token,
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
	return startProcess(cmd, "")
}

// startProcess starts cmd and collects its console output. container names
// the Docker container cmd runs, if it runs one.
func startProcess(cmd *exec.Cmd, container string) (*napcat, error) {
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	isolate(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 NapCat 失败：%w", err)
	}
	n := &napcat{cmd: cmd, container: container, lines: make(chan string, 256), done: make(chan struct{})}
	live.Lock()
	live.m[n] = struct{}{}
	live.Unlock()
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

// stop takes NapCat down and waits until it is gone. It may be called more
// than once, and concurrently: every call returns only once NapCat is gone.
func (n *napcat) stop() {
	if n == nil {
		return
	}
	n.stopOnce.Do(func() {
		if n.container != "" {
			// Let QQ quit by itself first, so a login made a moment ago is
			// on disk; the container ends with it.
			if exec.Command("docker", "exec", n.container, "pkill", "-TERM", "-x", "qq").Run() == nil {
				select {
				case <-n.done:
				case <-time.After(2 * time.Second):
				}
			}
			// Killing the docker client would leave the container running.
			_ = exec.Command("docker", "rm", "-f", n.container).Run()
		}
		killTree(n.cmd)
		<-n.done
		live.Lock()
		delete(live.m, n)
		live.Unlock()
	})
}

// release lets go of a container without stopping it, for the instance that
// starts next to take over (see adoptDockerNapCat). Only our view of its
// console goes away.
func (n *napcat) release() {
	n.stopOnce.Do(func() {
		killTree(n.cmd) // the attached docker client; the container runs on
		select {
		case <-n.done:
		case <-time.After(time.Second): // whatever still holds the console open, we are done with it
		}
		live.Lock()
		delete(live.m, n)
		live.Unlock()
	})
}

// loginFailed is a login QQ turned down, or a QR code that was declined.
// NapCat does not offer a new QR code after either, so it has to be restarted.
type loginFailed struct{ reason string }

func (e *loginFailed) Error() string { return "登录失败：" + e.reason }

// quickLoginErr is what NapCat prints when the saved login cannot be reused.
var quickLoginErr = regexp.MustCompile(`快速登录错误[:：]\s*(.+?)\s*$`)

var (
	loginErrMessage = regexp.MustCompile(`"message":"((?:[^"\\]|\\.)*)"`)
	qrSessionFailed = regexp.MustCompile(`Login Error,ErrType:\s*(\d+)\s+ErrCode:\s*(\d+)`)
)

// loginFailure recognises the console lines NapCat prints when a login
// attempt is over. An expired QR code is not one: NapCat replaces it itself.
func loginFailure(line string) error {
	if strings.Contains(line, "Login Error , ErrInfo:") {
		reason := "QQ 拒绝了这次登录"
		if m := loginErrMessage.FindStringSubmatch(line); m != nil && m[1] != "" {
			reason = m[1]
			if s, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
				reason = s
			}
		}
		return &loginFailed{reason}
	}
	if m := qrSessionFailed.FindStringSubmatch(line); m != nil && !(m[1] == "1" && m[2] == "3") {
		return &loginFailed{"扫码没有完成（可能在手机上取消了）"}
	}
	return nil
}

// awaitLogin watches NapCat until its OneBot server accepts our connection
// and reports a logged-in account. onQR is called with each login QR code
// link NapCat prints. A login that QQ turns down ends it with a *loginFailed.
//
// The connection that finds the account logged in is returned open (its
// channel closes when it ends), bound to live rather than to ctx, with
// onEvent receiving its events: QQ delivers what was sent while the account
// was offline the moment it logs in, and a second connection would miss it.
func awaitLogin(ctx, live context.Context, n *napcat, c *client, onEvent func(json.RawMessage), onQR func(url string), onLine func(string)) (loginInfo, <-chan struct{}, error) {
	var tail []string
	read := func(line string) error {
		if onLine != nil {
			onLine(line)
		}
		if tail = append(tail, line); len(tail) > 12 {
			tail = tail[1:]
		}
		if u := qrURL.FindString(line); u != "" && onQR != nil {
			onQR(u)
		}
		return loginFailure(line)
	}
	for {
		select {
		case <-ctx.Done():
			return loginInfo{}, nil, ctx.Err()
		case line, ok := <-n.lines:
			if !ok {
				return loginInfo{}, nil, errors.New("NapCat 意外退出：\n" + strings.Join(tail, "\n"))
			}
			if err := read(line); err != nil {
				return loginInfo{}, nil, err
			}
			continue
		default:
		}
		// The OneBot server only starts listening once an account is logged in.
		if done, err := c.connect(live, onEvent); err == nil {
			info, lerr := c.login(ctx)
			if lerr == nil && info.UserID.String() != "" && info.UserID.String() != "0" {
				return info, done, nil
			}
			c.close()
			<-done
		}
		select {
		case <-ctx.Done():
			return loginInfo{}, nil, ctx.Err()
		case line, ok := <-n.lines:
			if ok {
				if err := read(line); err != nil {
					return loginInfo{}, nil, err
				}
			}
		case <-n.done:
			// drain what is left on the next loop iteration
		case <-timeAfter():
		}
	}
}
