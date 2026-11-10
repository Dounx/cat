package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/dounx/cat/core/plugin"
)

func shellEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: "shell", Name: "运行脚本", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "terminal",
			Description:  "在本机运行一条命令。事件以 JSON 形式通过环境变量 CAT_EVENT 和标准输入传入。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "run", Title: "运行命令",
				Description: "不要把事件内容直接拼进命令（会被注入），请在脚本里读取 CAT_EVENT。输出：stdout、stderr、exit_code。",
				Params: map[string]any{
					"type":     "object",
					"required": []string{"command"},
					"x-order":  []string{"command", "cwd"},
					"properties": map[string]any{
						"command": map[string]any{"type": "string", "title": "命令", "format": "textarea"},
						"cwd":     map[string]any{"type": "string", "title": "工作目录"},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) { return &shell{}, nil },
	}
}

type shell struct{ plugin.Base }

func (s *shell) Invoke(ctx context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != "run" {
		return nil, plugin.ErrUnknownAction(action)
	}
	command := str(p, "command")
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("command is required")
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = str(p, "cwd")
	evJSON, _ := json.Marshal(ictx.Event)
	cmd.Env = append(os.Environ(), "CAT_EVENT="+string(evJSON))
	cmd.Stdin = bytes.NewReader(evJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	out := map[string]any{
		"stdout":    truncate(stdout.String(), 64<<10),
		"stderr":    truncate(stderr.String(), 64<<10),
		"exit_code": exitCode,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, fmt.Errorf("exit code %d: %s", exitErr.ExitCode(), truncate(strings.TrimSpace(stderr.String()), 200))
	}
	return out, err
}
