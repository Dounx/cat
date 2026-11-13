// Command cat-plugin-claude-desktop hands events to Claude Code running on this machine.
package main

import (
	"context"
	"errors"
	"time"

	"github.com/dounx/cat/plugins/internal/claudecode"
	"github.com/dounx/cat/plugins/internal/harness"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func options(h *cat.Host) (claudecode.Options, error) {
	bin, err := harness.FindBin("claude", harness.Str(h.Config, "bin"))
	if err != nil {
		return claudecode.Options{}, errors.New(err.Error() + "。请先安装 Claude Code：https://claude.com/claude-code")
	}
	cwd, err := harness.EnsureDir(harness.Str(h.Config, "cwd"), h.DataDir)
	if err != nil {
		return claudecode.Options{}, err
	}
	tools := harness.CommaList(h.Config, "allowed_tools")
	if _, set := h.Config["allowed_tools"]; !set {
		tools = []string{"Read", "Glob", "Grep"}
	}
	return claudecode.Options{
		Bin: bin, Cwd: cwd, AllowedTools: tools,
		PermissionMode: harness.Str(h.Config, "permission_mode"),
		Model:          harness.Str(h.Config, "model"),
		SystemPrompt:   harness.Str(h.Config, "system_prompt"),
	}, nil
}

func New() cat.Plugin {
	var (
		opts  claudecode.Options
		guard harness.Guard
		locks harness.Locks
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) (err error) {
			if opts, err = options(h); err != nil {
				return err
			}
			guard = harness.NewGuard(h.Config, false)
			vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			v, err := claudecode.Version(vctx, opts.Bin)
			if err != nil {
				return errors.New("无法运行 claude 命令：" + err.Error())
			}
			h.Log("info", "Claude Code %s，工作目录 %s", v, opts.Cwd)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"chat": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				if err := guard.Check(actx); err != nil {
					return nil, err
				}
				prompt := harness.Str(p, "prompt")
				if prompt == "" {
					return nil, errors.New("提示词为空")
				}
				key := harness.SessionKey(p, actx)
				defer locks.Lock(key)()
				resume := ""
				if key != "" && p["new_session"] != true {
					resume, _ = h.StateGet("session:" + key)
				}
				o := opts
				if extra := harness.Str(p, "system_prompt"); extra != "" {
					o.SystemPrompt = (o.SystemPrompt + "\n\n" + extra)
				}
				res, err := claudecode.Run(ctx, o, prompt, resume)
				if res.SessionID != "" && key != "" {
					_ = h.StateSet("session:"+key, res.SessionID)
				}
				return res, err
			},
		},
		Setup: func(ctx context.Context, s *cat.Setup) (map[string]any, error) {
			return setup(ctx, s)
		},
	}
}

func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	for {
		s.Progress("正在检测 Claude Code…", -1)
		bin, err := harness.FindBin("claude", harness.Str(cfg, "bin"))
		if err == nil {
			vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			v, verr := claudecode.Version(vctx, bin)
			cancel()
			if verr == nil {
				s.Show(cat.Step{Kind: cat.StepInfo, Title: "已找到 Claude Code " + v})
				break
			}
			err = verr
		}
		ok, aerr := s.Ask(ctx, cat.Step{
			Kind: cat.StepConfirm, Title: "没有找到 Claude Code", URL: "https://claude.com/claude-code",
			Description: err.Error() + "\n\n请先安装并在终端里运行一次 claude 完成登录，然后点确认重新检测。",
		})
		if aerr != nil {
			return nil, aerr
		}
		if c, _ := ok["confirmed"].(bool); !c {
			return nil, cat.ErrCancelled
		}
	}
	values := map[string]any{"permission_mode": "dontAsk", "allowed_tools": "Read,Glob,Grep"}
	for _, k := range []string{"cwd", "permission_mode", "allowed_tools", "allowed_actors", "allow_anyone"} {
		if v, ok := cfg[k]; ok {
			values[k] = v
		}
	}
	guardSchema := harness.GuardSchema()
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "它能做什么、谁能用它",
		Description: "聊天消息会原样成为给 Agent 的指令，所以默认只允许只读工具，并且只接受白名单里的发送者。",
		Values:      values,
		Schema: map[string]any{
			"type": "object", "x-order": []string{"cwd", "permission_mode", "allowed_tools", "allowed_actors", "allow_anyone"},
			"properties": map[string]any{
				"cwd":             map[string]any{"type": "string", "title": "工作目录", "description": "留空使用实例自己的空目录。"},
				"permission_mode": map[string]any{"type": "string", "title": "权限模式", "enum": []string{"dontAsk", "acceptEdits", "auto", "bypassPermissions"}, "x-enum-titles": []string{"只用下面允许的工具（推荐）", "可以改文件", "由 Claude Code 自动判断", "不做任何限制（危险）"}, "default": "dontAsk", "description": "「不做任何限制」只在你完全信任能触发它的人时使用。"},
				"allowed_tools":   map[string]any{"type": "string", "title": "允许的工具", "description": "逗号分隔。"},
				"allowed_actors":  guardSchema["allowed_actors"],
				"allow_anyone":    guardSchema["allow_anyone"],
			},
		},
	})
	if err != nil {
		return nil, err
	}
	for k, v := range out {
		cfg[k] = v
	}
	if _, err := harness.EnsureDir(harness.Str(cfg, "cwd"), s.DataDir); err != nil {
		return nil, err
	}
	return cfg, nil
}
