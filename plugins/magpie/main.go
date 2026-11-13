// Command cat-plugin-magpie chats through a Magpie gateway: the models, the
// subscriptions and the keys behind them stay in Magpie, and an instance only
// says which model to ask. No tools: the model can only produce text.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/harness"
	"github.com/dounx/cat/plugins/internal/openaichat"
	cat "github.com/dounx/cat/sdk/go"
)

const (
	// where a Magpie on this machine serves its gateway
	defaultBase = "http://127.0.0.1:3425"
	// what Cat hands a wizard in place of a saved secret
	secretMask = "••••••••"
)

func main() { cat.Run(New()) }

// gateway is a Magpie to talk to. A Magpie on this machine asks for no key.
type gateway struct {
	base string
	key  string
}

func readGateway(cfg map[string]any) gateway {
	base := strings.TrimRight(harness.Str(cfg, "base_url"), "/")
	// the address is the gateway's own; take one pasted with the API's /v1 too
	base = strings.TrimSuffix(base, "/v1")
	if base == "" {
		base = defaultBase
	}
	key := harness.Str(cfg, "api_key")
	if key == secretMask {
		key = ""
	}
	return gateway{base: base, key: key}
}

func (g gateway) client(model string) *openaichat.Client {
	return &openaichat.Client{Base: g.base + "/v1", Key: g.key, Model: model, Keyless: true}
}

func (g gateway) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", g.base+path, nil)
	if err != nil {
		return fmt.Errorf("地址 %s 不对", g.base)
	}
	if g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return g.unreachable(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return errors.New("Magpie 不接受这个网关密钥。在运行 Magpie 的机器上用 magpie gateway-key add <名字> 建一个，填到这里")
	case resp.StatusCode != 200:
		return fmt.Errorf("%s 返回了 %d，它看起来不是 Magpie", g.base, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s 的回答看不懂，它看起来不是 Magpie", g.base)
	}
	return nil
}

func (g gateway) unreachable(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("连不上 %s 的 Magpie（%v）。请确认 Magpie 开着，或运行 magpie serve", g.base, err)
}

// probe says which version of Magpie answers at the address.
func (g gateway) probe(ctx context.Context) (string, error) {
	var out struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := g.get(ctx, "/", &out); err != nil {
		return "", err
	}
	if out.Name != "magpie" {
		return "", fmt.Errorf("%s 上的不是 Magpie", g.base)
	}
	return out.Version, nil
}

type model struct {
	ID    string `json:"id"`
	Name  string `json:"display_name"`
	Label string `json:"magpie_label"` // the name with what it is: "… · routing group"
}

// name is what the model is called, with what it is: "… · routing group".
func (m model) name() string {
	if m.Label != "" {
		return m.Label
	}
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

func (m model) title() string {
	if name := m.name(); name != m.ID {
		return name + "（" + m.ID + "）"
	}
	return m.ID
}

// models lists what the key may ask, routing groups first, as Magpie orders them.
func (g gateway) models(ctx context.Context) ([]model, error) {
	var out struct {
		Data []model `json:"data"`
	}
	if err := g.get(ctx, "/v1/models", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func New() cat.Plugin {
	var (
		gw      gateway
		model   string
		history openaichat.History
		locks   harness.Locks
		system  string
	)
	// the client for one call: the instance's model unless the action names another
	client := func(p map[string]any) *openaichat.Client {
		if m := harness.Str(p, "model"); m != "" {
			return gw.client(m)
		}
		return gw.client(model)
	}
	// A request that never reached the gateway says so in words that tell
	// what to do about it; what the gateway itself answered is left as it is.
	explain := func(ctx context.Context, err error) error {
		var ue *url.Error
		if errors.As(err, &ue) && ctx.Err() == nil {
			return gw.unreachable(err)
		}
		return err
	}
	return cat.Plugin{
		// The models to choose from, for the instance and for each action.
		Options: func(ctx context.Context, _ *cat.Host, _, field string) ([]cat.Option, error) {
			if field != "model" {
				return nil, nil
			}
			models, err := gw.models(ctx)
			if err != nil {
				return nil, explain(ctx, err)
			}
			opts := make([]cat.Option, len(models))
			for i, m := range models {
				opts[i] = cat.Option{Value: m.ID, Title: m.name(), Note: m.ID}
			}
			return opts, nil
		},
		// Magpie need not be running yet: it is only asked when an action runs.
		Start: func(_ context.Context, h *cat.Host) error {
			gw = readGateway(h.Config)
			if model = harness.Str(h.Config, "model"); model == "" {
				return errors.New("需要选一个模型")
			}
			max, _ := h.Config["max_history"].(float64)
			history = openaichat.History{Dir: filepath.Join(h.DataDir, "sessions"), Max: int(max)}
			system = harness.Str(h.Config, "system_prompt")
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"chat": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				prompt := harness.Str(p, "prompt")
				if prompt == "" {
					return nil, errors.New("提示词为空")
				}
				key := harness.SessionKey(p, actx)
				defer locks.Lock(key)()
				sys := system
				if extra := harness.Str(p, "system_prompt"); extra != "" {
					sys = strings.TrimSpace(sys + "\n\n" + extra)
				}
				c := client(p)
				text, sid, usage, err := openaichat.Converse(ctx, c, history, key, sys, prompt, p["new_session"] == true)
				if err != nil {
					return nil, explain(ctx, err)
				}
				return map[string]any{"text": text, "session_id": sid, "model": c.Model, "usage": usage}, nil
			},
			// One question, one answer out of a fixed set, no memory. A rule's
			// condition node asks this and takes the way out named by "choice".
			"judge": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				prompt := harness.Str(p, "prompt")
				if prompt == "" {
					return nil, errors.New("提示词为空")
				}
				choices := harness.Choices(p)
				if len(choices) == 0 {
					choices = []string{"yes", "no"}
				}
				sys := system
				if extra := harness.Str(p, "system_prompt"); extra != "" {
					sys = strings.TrimSpace(sys + "\n\n" + extra)
				}
				c := client(p)
				choice, reply, usage, err := openaichat.Pick(ctx, c, sys, prompt, choices)
				if err != nil {
					return nil, explain(ctx, err)
				}
				return map[string]any{"choice": choice, "text": reply, "model": c.Model, "usage": usage}, nil
			},
		},
		Setup: setup,
	}
}

// setup looks for a Magpie, on this machine unless the instance says where,
// and has the user pick one of its models. The address and key are only asked
// for when no Magpie answers there.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	var (
		gw      gateway
		version string
		models  []model
	)
	for {
		gw = readGateway(cfg)
		s.Progress("正在查找 Magpie…", -1)
		var err error
		if version, err = gw.probe(ctx); err == nil {
			if models, err = gw.models(ctx); err == nil && len(models) == 0 {
				err = errors.New("这个 Magpie 里还没有能用的模型。先在 Magpie 里添加一个 provider 或登录一个订阅")
			}
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out, askErr := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 Magpie", URL: "https://usemagpie.ai",
			Description: "⚠ " + err.Error() + "\n\nMagpie 开在这台机器上时，打开它再点下一步就行，地址不用改。",
			Values:      map[string]any{"base_url": gw.base},
			Schema: map[string]any{
				"type": "object", "x-order": []string{"base_url", "api_key"},
				"properties": map[string]any{
					"base_url": map[string]any{"type": "string", "title": "Magpie 地址"},
					"api_key":  map[string]any{"type": "string", "title": "网关密钥（可选）", "format": "password", "description": "连接本机的 Magpie 不需要。连接别的机器上的 Magpie 时，填在那边用 magpie gateway-key add 建的密钥。"},
				},
			},
		})
		if askErr != nil {
			return nil, askErr
		}
		for k, v := range out {
			cfg[k] = v
		}
	}
	cfg["base_url"] = gw.base

	ids, titles := make([]string, len(models)), make([]string, len(models))
	picked := models[0].ID
	for i, m := range models {
		ids[i], titles[i] = m.ID, m.title()
		if m.ID == harness.Str(cfg, "model") {
			picked = m.ID
		}
	}
	desc := fmt.Sprintf("已找到 %s 的 Magpie", gw.base)
	if version != "" {
		desc += " " + version
	}
	desc += fmt.Sprintf("，有 %d 个模型。排在前面的是路由组：几个模型当一个用，由 Magpie 决定这次问谁。", len(models))
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "选一个模型", Description: desc,
		Values: map[string]any{"model": picked, "system_prompt": cfg["system_prompt"]},
		Schema: map[string]any{
			"type": "object", "required": []string{"model"}, "x-order": []string{"model", "system_prompt"},
			"properties": map[string]any{
				"model":         map[string]any{"type": "string", "title": "模型", "enum": ids, "x-enum-titles": titles},
				"system_prompt": map[string]any{"type": "string", "title": "系统提示（可选）", "format": "textarea"},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	for k, v := range out {
		cfg[k] = v
	}
	if harness.Str(cfg, "model") == "" {
		cfg["model"] = picked
	}
	s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 Magpie（" + harness.Str(cfg, "model") + "）",
		Description: "在规则里添加一个动作，目标选这个实例，动作选「对话」；条件节点切到「模型判断」时也可以选它。"})
	return cfg, nil
}
