// Command cat-plugin-llm chats with any OpenAI-compatible endpoint. No tools:
// the model can only produce text.
package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/dounx/cat/plugins/internal/harness"
	"github.com/dounx/cat/plugins/internal/openaichat"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func New() cat.Plugin {
	var (
		client  *openaichat.Client
		history openaichat.History
		guard   harness.Guard
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
			guard = harness.NewGuard(h.Config, true)
			system = harness.Str(h.Config, "system_prompt")
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
		},
	}
}
