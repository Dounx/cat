// Package autostart registers the app to launch when the user logs in.
package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const label = "com.dounx.cat"

func exe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "autostart", "cat.desktop"), nil
	}
	return "", nil
}

// Enabled reports whether launch-at-login is currently registered.
func Enabled() bool {
	if runtime.GOOS == "windows" {
		return exec.Command("reg", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "Cat").Run() == nil
	}
	p, err := path()
	if err != nil || p == "" {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Set registers or removes launch-at-login. The app is started with --hidden
// so it comes up in the tray without opening a window.
func Set(on bool) error {
	bin, err := exe()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
		if !on {
			_ = exec.Command("reg", "delete", key, "/v", "Cat", "/f").Run()
			return nil
		}
		return exec.Command("reg", "add", key, "/v", "Cat", "/t", "REG_SZ", "/d", `"`+bin+`" --hidden`, "/f").Run()
	}
	p, err := path()
	if err != nil {
		return err
	}
	if p == "" {
		return fmt.Errorf("launch at login is not supported on %s", runtime.GOOS)
	}
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var content string
	if runtime.GOOS == "darwin" {
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(bin)
		content = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + label + `</string>
  <key>ProgramArguments</key><array><string>` + esc + `</string><string>--hidden</string></array>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Interactive</string>
</dict></plist>
`
	} else {
		content = "[Desktop Entry]\nType=Application\nName=Cat\nExec=\"" + bin + "\" --hidden\nX-GNOME-Autostart-enabled=true\n"
	}
	return os.WriteFile(p, []byte(content), 0o644)
}
