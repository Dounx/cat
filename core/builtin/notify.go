package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/dounx/cat/core/plugin"
)

const defaultNotifyTitle = "Cat 事件中心"

func notifyEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: "notify", Name: "本机通知", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "bell",
			Description:  "在这台电脑上弹出系统通知，不用先接聊天软件。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "send", Title: "弹出系统通知",
				Description: "在这台电脑上弹出系统通知。输出：title、text。",
				Params: map[string]any{
					"type":     "object",
					"required": []string{"text"},
					"x-order":  []string{"title", "text", "sound"},
					"properties": map[string]any{
						"title": map[string]any{"type": "string", "title": "标题", "default": defaultNotifyTitle},
						"text":  map[string]any{"type": "string", "title": "内容", "format": "textarea"},
						"sound": map[string]any{"type": "boolean", "title": "提示音", "default": false},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) {
			return &notify{deliver: osDeliver}, nil
		},
	}
}

type notify struct {
	plugin.Base
	deliver func(ctx context.Context, title, body string, sound bool) error
}

func (n *notify) Invoke(ctx context.Context, action string, p map[string]any, _ plugin.InvokeContext) (map[string]any, error) {
	if action != "send" {
		return nil, plugin.ErrUnknownAction(action)
	}
	title := strings.TrimSpace(str(p, "title"))
	if title == "" {
		title = defaultNotifyTitle
	}
	body := text(p, "text")
	if body == "" && p["text"] != nil {
		body = strings.TrimSpace(fmt.Sprint(p["text"]))
	}
	if body == "" {
		return nil, errors.New("通知内容为空")
	}
	if r := []rune(body); len(r) > 500 {
		body = string(r[:500]) + "…"
	}
	sound, _ := p["sound"].(bool)
	if err := n.deliver(ctx, title, body, sound); err != nil {
		return nil, err
	}
	return map[string]any{"title": title, "text": body}, nil
}

func osDeliver(ctx context.Context, title, body string, sound bool) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		soundFlag := "0"
		if sound {
			soundFlag = "1"
		}
		script := `on run argv
if (item 3 of argv) is "1" then
  display notification (item 2 of argv) with title (item 1 of argv) sound name "default"
else
  display notification (item 2 of argv) with title (item 1 of argv)
end if
end run`
		cmd = exec.CommandContext(ctx, "osascript", "-e", script, "--", title, body, soundFlag)
	case "linux":
		cmd = exec.CommandContext(ctx, "notify-send", "-a", "Cat", "--", title, body)
	case "windows":
		ps := `Add-Type -AssemblyName System.Windows.Forms
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::Information
$n.Visible = $true
$n.ShowBalloonTip(5000, $env:CAT_NOTIFY_TITLE, $env:CAT_NOTIFY_BODY, [System.Windows.Forms.ToolTipIcon]::Info)
Start-Sleep -Milliseconds 300
$n.Dispose()`
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
		cmd.Env = append(os.Environ(), "CAT_NOTIFY_TITLE="+title, "CAT_NOTIFY_BODY="+body)
	default:
		return fmt.Errorf("当前系统（%s）不支持弹出系统通知", runtime.GOOS)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("弹出系统通知失败：%s", msg)
		}
		return fmt.Errorf("弹出系统通知失败：%w", err)
	}
	return nil
}
