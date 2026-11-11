// Command cat-plugin-agent-deepseek chats with DeepSeek models, either through
// the API alone or by driving Claude Code against DeepSeek's
// Anthropic-compatible endpoint for tool use.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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

// findDsh locates the DeepSeek Harness CLI, including inside the desktop app.
func findDsh(explicit string) (string, error) {
	if bin, err := harness.FindBin("dsh", explicit); err == nil {
		return bin, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/Applications/DeepSeek Harness.app/Contents/Resources/runtime/cli/bin/dsh",
		filepath.Join(home, "Applications/DeepSeek Harness.app/Contents/Resources/runtime/cli/bin/dsh"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("没有找到 DeepSeek Harness（dsh 命令）。请先安装它，或改用 api 方式")
}

type dshOptions struct {
	bin, cwd, permission, key string
	// profile lends its model setup to the run and model ("provider/model")
	// overrides its choice; dataDir holds the patch file.
	profile, model, dataDir string
}

type dshResult struct {
	Text      string `json:"text"`
	SessionID string `json:"session_id"`
}

// dshArgs builds a headless invocation. The task is read from stdin ("-").
// Approval is never asked for: nobody is there to answer.
func dshArgs(o dshOptions, session, patch string) []string {
	a := []string{"--profile", runProfile}
	if patch != "" {
		a = append(a, "--patch", patch)
	}
	a = append(a, "--json", "--approval-policy", "never")
	if o.permission != "" {
		a = append(a, "--permission-mode", o.permission)
	}
	if session != "" {
		a = append(a, "--session-id", session)
	}
	return append(a, "-")
}

// parseDsh reads the newline-delimited run events of `dsh headless --json`
// (shape captured from DeepSeek Harness's CLI).
func parseDsh(stdout []byte) (res dshResult, failure string) {
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Text      string `json:"text"`
			Reason    struct {
				Kind  string `json:"kind"`
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			} `json:"reason"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "session":
			res.SessionID = ev.SessionID
		case "final":
			res.Text = ev.Text
			if ev.SessionID != "" {
				res.SessionID = ev.SessionID
			}
			if ev.Reason.Kind == "error" {
				failure = ev.Reason.Error.Message
				if ev.Reason.Error.Code == "MISSING_CREDENTIAL" {
					failure = "它要用的模型没有可用的密钥（" + ev.Reason.Error.Message + "）。请在这个接入的配置里选一个已经配好模型的档案，或填写 DeepSeek 的 API Key"
				}
				if failure == "" {
					failure = "运行出错"
				}
			}
		}
	}
	return res, failure
}

func runDsh(ctx context.Context, o dshOptions, prompt, session string) (dshResult, error) {
	var env []string
	if o.key != "" {
		env = append(env, "DEEPSEEK_API_KEY="+o.key)
	}
	patch, err := overlay(o.profile, o.model, o.dataDir)
	if err != nil {
		return dshResult{}, err
	}
	stdout, stderr, runErr := harness.RunCLI(ctx, o.bin, dshArgs(o, session, patch), o.cwd, harness.PathEnv(env...), prompt)
	res, failure := parseDsh(stdout)
	if failure != "" {
		return res, errors.New("DeepSeek Harness：" + failure)
	}
	if runErr != nil {
		return res, errors.New("DeepSeek Harness 运行失败：" + runErr.Error() + " " + harness.Tail(string(stderr), 400))
	}
	if strings.TrimSpace(res.Text) == "" {
		return res, errors.New("DeepSeek Harness 没有返回内容 " + harness.Tail(string(stderr), 300))
	}
	return res, nil
}

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
		s.mode = "harness"
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

// apiModels lists the models the key may ask.
func apiModels(ctx context.Context, s settings) ([]cat.Option, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 DeepSeek：" + err.Error())
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil, errors.New("DeepSeek 没有给出模型列表")
	}
	opts := make([]cat.Option, len(out.Data))
	for i, m := range out.Data {
		opts[i] = cat.Option{Value: m.ID, Title: m.Name, Note: m.ID}
	}
	return opts, nil
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
		dsh     dshOptions
		guard   harness.Guard
		locks   harness.Locks
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			s = read(h.Config)
			if s.key == "" && s.mode != "harness" {
				return errors.New("缺少 API Key")
			}
			switch s.mode {
			case "harness":
				bin, err := findDsh(harness.Str(h.Config, "bin"))
				if err != nil {
					return err
				}
				cwd, err := harness.EnsureDir(harness.Str(h.Config, "cwd"), h.DataDir)
				if err != nil {
					return err
				}
				dsh = dshOptions{bin: bin, cwd: cwd, permission: harness.Str(h.Config, "harness_permission"), key: s.key,
					profile: harness.Str(h.Config, "harness_profile"), dataDir: h.DataDir}
				guard = harness.NewGuard(h.Config, false)
			case "api":
				client = &openaichat.Client{Base: s.base, Key: s.key, Model: s.model}
				history = openaichat.History{Dir: filepath.Join(h.DataDir, "sessions")}
				guard = harness.NewGuard(h.Config, true)
			case "claude-code":
				bin, err := harness.FindBin("claude", harness.Str(h.Config, "bin"))
				if err != nil {
					return errors.New(err.Error() + "。这种方式需要先安装 Claude Code：https://claude.com/claude-code")
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
		// The models an action may switch to, written the way its mode wants them.
		Options: func(ctx context.Context, h *cat.Host, _, field string) ([]cat.Option, error) {
			if field != "model" {
				return nil, nil
			}
			if s.mode != "harness" {
				return apiModels(ctx, s)
			}
			// the Harness answers with what the profile in use declares, and
			// with DeepSeek's own models when there is a key for them
			profile := dsh.profile
			if profile == "" {
				profile = runProfile
			}
			entries, _ := modelEntries(profile)
			var opts []cat.Option
			for _, m := range declaredModels(entries) {
				opts = append(opts, cat.Option{Value: m.provider + "/" + m.id, Title: m.name, Note: m.provider + "/" + m.id})
			}
			if s.key != "" {
				official, err := apiModels(ctx, s)
				if err != nil && len(opts) == 0 {
					return nil, err
				}
				for _, o := range official {
					opts = append(opts, cat.Option{Value: "deepseek-official/" + o.Value, Title: o.Title, Note: "deepseek-official/" + o.Value})
				}
			}
			return opts, nil
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
				// this action's own model, when it names one
				model := harness.Str(p, "model")
				if s.mode == "api" {
					sys := strings.TrimSpace(s.system + "\n\n" + extra)
					c := client
					if model != "" {
						c = &openaichat.Client{Base: s.base, Key: s.key, Model: model}
					}
					text, sid, usage, err := openaichat.Converse(ctx, c, history, key, sys, prompt, p["new_session"] == true)
					if err != nil {
						return nil, err
					}
					return map[string]any{"text": text, "session_id": sid, "usage": usage}, nil
				}
				resume := ""
				if key != "" && p["new_session"] != true {
					resume, _ = h.StateGet("session:" + key)
				}
				if s.mode == "harness" {
					// DeepSeek Harness has no system prompt flag; instructions open the conversation.
					first := prompt
					if sys := strings.TrimSpace(s.system + "\n\n" + extra); sys != "" {
						first = sys + "\n\n" + prompt
					}
					var res dshResult
					var err error
					o := dsh
					o.model = model
					if resume != "" {
						if res, err = runDsh(ctx, o, prompt, resume); err != nil && ctx.Err() == nil {
							h.Log("warn", "续接会话 %s 失败（%v），改为新开会话", resume, err)
							res, err = runDsh(ctx, o, first, "")
						}
					} else {
						res, err = runDsh(ctx, o, first, "")
					}
					if res.SessionID != "" && key != "" && err == nil {
						_ = h.StateSet("session:"+key, res.SessionID)
					}
					return res, err
				}
				o := claude
				if model != "" {
					with := s
					with.model = model
					o.Model, o.Env = model, claudeEnv(with)
				}
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
	dshBin, dshErr := findDsh(harness.Str(cfg, "bin"))
	// Which profile's model to use: only offered when there is a choice.
	var profiles, profileTitles []string
	if dshErr == nil {
		for _, p := range listProfiles() {
			title := p.name + "（" + p.provider + " · " + p.model + "）"
			if p.model == "" {
				title = p.name + "（DeepSeek 官方模型，需要 API Key）"
			}
			profiles, profileTitles = append(profiles, p.name), append(profileTitles, title)
		}
	}
	if len(profiles) > 1 && harness.Str(cfg, "harness_profile") == "" {
		cfg["harness_profile"] = profiles[0]
		for _, name := range profiles {
			if name == "desktop" {
				cfg["harness_profile"] = name
			}
		}
	}
	problem := ""
	for {
		mode := harness.Str(cfg, "mode")
		if mode == "" {
			mode = "harness"
			if dshErr != nil {
				mode = "api"
			}
		}
		desc := "本机的 DeepSeek Harness：能用工具。"
		if dshErr != nil {
			desc += "（本机没有找到它）"
		} else {
			desc += "（已在本机找到）"
		}
		desc += "\n\nAPI Key 在 DeepSeek 开放平台创建；用 DeepSeek Harness 时，如果它自己已配置密钥可以留空。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		guardSchema := harness.GuardSchema()
		props := map[string]any{
			"mode":           map[string]any{"type": "string", "title": "方式", "enum": []string{"harness", "api", "claude-code"}, "x-enum-titles": []string{"本机的 DeepSeek Harness", "只调用 API（纯对话，不带工具）", "用 DeepSeek 模型驱动本机的 Claude Code"}},
			"api_key":        map[string]any{"type": "string", "title": "API Key", "format": "password"},
			"cwd":            map[string]any{"type": "string", "title": "工作目录（Harness / Claude Code 方式）", "description": "留空使用 Cat 为它准备的空目录。"},
			"allowed_actors": guardSchema["allowed_actors"],
			"allow_anyone":   guardSchema["allow_anyone"],
		}
		if len(profiles) > 1 {
			props["harness_profile"] = map[string]any{"type": "string", "title": "跟哪个档案用同一个模型（Harness 方式）", "enum": profiles, "x-enum-titles": profileTitles,
				"description": "DeepSeek Harness 的桌面端、网页端各有一份设置，这里按 " + runProfile + " 档案运行，默认不会用到你在桌面端选的模型。"}
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 DeepSeek", Description: desc, URL: "https://platform.deepseek.com/api_keys",
			Values: map[string]any{"mode": mode, "harness_profile": cfg["harness_profile"], "cwd": cfg["cwd"], "allowed_actors": cfg["allowed_actors"]},
			Schema: map[string]any{
				"type": "object", "x-order": []string{"mode", "harness_profile", "api_key", "cwd", "allowed_actors", "allow_anyone"},
				"properties": props,
			},
		})
		if err != nil {
			return nil, err
		}
		for k, v := range out {
			cfg[k] = v
		}
		set := read(cfg)
		switch set.mode {
		case "harness":
			if dshErr != nil {
				problem = dshErr.Error()
				continue
			}
		case "claude-code":
			if _, err := harness.FindBin("claude", harness.Str(cfg, "bin")); err != nil {
				problem = err.Error() + "。请先安装 Claude Code，或改用其他方式"
				continue
			}
		}
		if set.key == "" && set.mode != "harness" {
			problem = "这种方式需要填写 API Key"
			continue
		}
		if set.key != "" && set.key != "••••••••" {
			s.Progress("正在验证 API Key…", -1)
			if err := verifyKey(ctx, set); err != nil {
				problem = err.Error()
				continue
			}
		}
		cwd, err := harness.EnsureDir(harness.Str(cfg, "cwd"), s.DataDir)
		if err != nil {
			problem = err.Error()
			continue
		}
		// The only proof that the Harness can answer is an answer.
		if set.mode == "harness" && set.key != "••••••••" {
			s.Progress("正在让 DeepSeek Harness 试答一句…", -1)
			tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_, err := runDsh(tctx, dshOptions{bin: dshBin, cwd: cwd, permission: harness.Str(cfg, "harness_permission"), key: set.key,
				profile: harness.Str(cfg, "harness_profile"), dataDir: s.DataDir}, "只回复两个字：收到", "")
			cancel()
			if err != nil {
				problem = err.Error()
				continue
			}
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 DeepSeek（" + set.mode + "）",
			Description: "在规则里添加一个动作，目标选这个实例，动作选「对话」。"})
		return cfg, nil
	}
}
