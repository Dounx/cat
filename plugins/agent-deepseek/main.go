// Command cat-plugin-agent-deepseek chats with DeepSeek models, either through
// the API alone or by driving Claude Code against DeepSeek's
// Anthropic-compatible endpoint for tool use.
package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/claudecode"
	"github.com/dounx/cat/plugins/internal/harness"
	"github.com/dounx/cat/plugins/internal/openaichat"
	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultBase  = "https://api.deepseek.com"
	defaultModel = "deepseek-flash"
)

func main() { cat.Run(New()) }

type settings struct {
	key, base, model, mode, system string
}

func read(cfg map[string]any) settings {
	s := settings{
		key: harness.Str(cfg, "api_key"), base: strings.TrimRight(harness.Str(cfg, "api_base"), "/"),
		model: harness.Str(cfg, "model"), mode: harness.Str(cfg, "mode"), system: harness.Str(cfg, "system_prompt"),
	}
	if s.base == "" {
		s.base = defaultBase
	}
	if s.model == "" {
		s.model = defaultModel
	}
	if s.mode == "" {
		s.mode = "api"
	}
	return s
}

// claudeEnv points Claude Code at DeepSeek's Anthropic-compatible endpoint.
func claudeEnv(s settings) []string {
	return []string{
		"ANTHROPIC_BASE_URL=" + s.base + "/anthropic",
		"ANTHROPIC_AUTH_TOKEN=" + s.key,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_MODEL=" + s.model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + s.model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + s.model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + s.model,
	}
}

// verifyKey checks the key with a free call (listing models).
func verifyKey(ctx context.Context, s settings) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("无法连接 DeepSeek：" + err.Error())
	}
	resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("API Key 无效")
	}
	return nil
}

func New() cat.Plugin {
	var (
		s       settings
		client  *openaichat.Client
		history openaichat.History
		claude  claudecode.Options
		guard   harness.Guard
		locks   harness.Locks
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			s = read(h.Config)
			if s.key == "" {
				return errors.New("缺少 API Key")
			}
			switch s.mode {
			case "api":
				client = &openaichat.Client{Base: s.base, Key: s.key, Model: s.model}
				history = openaichat.History{Dir: filepath.Join(h.DataDir, "sessions")}
				guard = harness.NewGuard(h.Config, true)
			case "claude-code":
				bin, err := harness.FindBin("claude", harness.Str(h.Config, "bin"))
				if err != nil {
					return errors.New(err.Error() + "。claude-code 方式需要先安装 Claude Code：https://claude.com/claude-code")
				}
				cwd, err := harness.EnsureDir(harness.Str(h.Config, "cwd"), h.DataDir)
				if err != nil {
					return err
				}
				tools := harness.CommaList(h.Config, "allowed_tools")
				if _, set := h.Config["allowed_tools"]; !set {
					tools = []string{"Read", "Glob", "Grep"}
				}
				claude = claudecode.Options{
					Bin: bin, Cwd: cwd, AllowedTools: tools, Model: s.model, SystemPrompt: s.system,
					PermissionMode: harness.Str(h.Config, "permission_mode"), Env: claudeEnv(s),
				}
				guard = harness.NewGuard(h.Config, false)
			default:
				return errors.New("未知的方式：" + s.mode)
			}
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
				extra := harness.Str(p, "system_prompt")
				if s.mode == "api" {
					sys := strings.TrimSpace(s.system + "\n\n" + extra)
					text, sid, usage, err := openaichat.Converse(ctx, client, history, key, sys, prompt, p["new_session"] == true)
					if err != nil {
						return nil, err
					}
					return map[string]any{"text": text, "session_id": sid, "usage": usage}, nil
				}
				resume := ""
				if key != "" && p["new_session"] != true {
					resume, _ = h.StateGet("session:" + key)
				}
				o := claude
				if extra != "" {
					o.SystemPrompt = strings.TrimSpace(o.SystemPrompt + "\n\n" + extra)
				}
				res, err := claudecode.Run(ctx, o, prompt, resume)
				if res.SessionID != "" && key != "" {
					_ = h.StateSet("session:"+key, res.SessionID)
				}
				return res, err
			},
		},
		Setup: setup,
	}
}

func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	problem := ""
	for {
		desc := "在 DeepSeek 开放平台创建一个 API Key，粘贴到下面。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		mode := harness.Str(cfg, "mode")
		if mode == "" {
			mode = "api"
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 DeepSeek", Description: desc, URL: "https://platform.deepseek.com/api_keys",
			Values: map[string]any{"mode": mode},
			Schema: map[string]any{
				"type": "object", "required": []string{"api_key"}, "x-order": []string{"api_key", "mode"},
				"properties": map[string]any{
					"api_key": map[string]any{"type": "string", "title": "API Key", "format": "password"},
					"mode": map[string]any{"type": "string", "title": "方式", "enum": []string{"api", "claude-code"}, "default": "api",
						"description": "api：纯对话，不带工具。claude-code：用 DeepSeek 模型驱动本机的 Claude Code，可读写文件（需已安装 Claude Code）。"},
				},
			},
		})
		if err != nil {
			return nil, err
		}
		for k, v := range out {
			cfg[k] = v
		}
		s.Progress("正在验证 API Key…", -1)
		set := read(cfg)
		if err := verifyKey(ctx, set); err != nil {
			problem = err.Error()
			continue
		}
		if set.mode == "claude-code" {
			if _, err := harness.FindBin("claude", harness.Str(cfg, "bin")); err != nil {
				problem = err.Error() + "。请先安装 Claude Code，或改用 api 方式"
				continue
			}
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 DeepSeek（" + set.model + "）",
			Description: "在规则里添加一个动作，目标选这个实例，动作选「对话」。"})
		return cfg, nil
	}
}
