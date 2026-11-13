// Command cat-plugin-llm chats with any OpenAI-compatible endpoint. No tools:
// the model can only produce text.
package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/dounx/cat/plugins/internal/harness"
	"github.com/dounx/cat/plugins/internal/openaichat"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func New() cat.Plugin {
	var (
		client  *openaichat.Client
		history openaichat.History
		locks   harness.Locks
		system  string
	)
	return cat.Plugin{
		Start: func(_ context.Context, h *cat.Host) error {
			base := harness.Str(h.Config, "base_url")
			if base == "" {
				base = "https://api.openai.com/v1"
			}
			client = &openaichat.Client{Base: base, Key: harness.Str(h.Config, "api_key"), Model: harness.Str(h.Config, "model")}
			if client.Key == "" || client.Model == "" {
				return errors.New("需要填写 API Key 和模型")
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
					sys += "\n\n" + extra
				}
				text, sid, usage, err := openaichat.Converse(ctx, client, history, key, sys, prompt, p["new_session"] == true)
				if err != nil {
					return nil, err
				}
				return map[string]any{"text": text, "session_id": sid, "usage": usage}, nil
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
				choice, reply, usage, err := openaichat.Pick(ctx, client, sys, prompt, choices)
				if err != nil {
					return nil, err
				}
				return map[string]any{"choice": choice, "text": reply, "usage": usage}, nil
			},
		},
	}
}
