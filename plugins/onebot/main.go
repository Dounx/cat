// Command cat-plugin-onebot connects Cat to QQ through a OneBot 11
// service you run (any implementation with a forward WebSocket server).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func str(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return strings.TrimSpace(s)
	}
	if m[k] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[k]))
}

func New() cat.Plugin {
	var c *client
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			onEvent := func(raw json.RawMessage) {
				if ev, ok := convert(raw); ok {
					if _, err := h.Emit(ev); err != nil {
						h.Log("error", "emit: %v", err)
					}
				}
			}
			if mode := str(h.Config, "mode"); mode == "managed" || mode == "docker" {
				return errors.New("Cat 不再代为运行 NapCat。请自行运行一个 OneBot 11 服务，然后重新运行接入向导填写它的地址")
			}
			url := str(h.Config, "ws_url")
			if url == "" {
				return errors.New("缺少 WebSocket 地址")
			}
			c = newClient(url, str(h.Config, "token"))
			done, err := c.connect(ctx, onEvent) // closes when the connection ends
			if err != nil {
				return err
			}
			info, err := c.login(ctx)
			if err != nil {
				return err
			}
			h.Log("info", "已连接 QQ %s（%s）", info.Nickname, info.UserID)
			go func() { // reconnect for as long as the instance runs
				backoff := time.Second
				for {
					select {
					case <-ctx.Done():
						return
					case <-done:
					}
					h.SetStatus("error", "与 OneBot 服务的连接中断，正在重连")
					for {
						select {
						case <-ctx.Done():
							return
						case <-time.After(backoff):
						}
						if backoff < 30*time.Second {
							backoff *= 2
						}
						if d, err := c.connect(ctx, onEvent); err == nil {
							done = d
							_, _ = c.login(ctx)
							h.SetStatus("running", "")
							backoff = time.Second
							break
						}
					}
				}
			}()
			return nil
		},
		Stop: func() {
			if c != nil {
				c.close()
			}
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, _ *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「回复」需要由一条聊天消息触发；要主动发消息请用「发送到指定会话」")
				}
				replyTo := ""
				if quote, ok := p["quote"].(bool); !ok || quote {
					replyTo = str(ev.Payload, "message_id")
				}
				return c.send(ctx, ev.Conversation.ID, str(p, "text"), replyTo)
			},
			"send": func(ctx context.Context, _ *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				return c.send(ctx, str(p, "conversation_id"), str(p, "text"), "")
			},
			"typing": func(context.Context, *cat.Host, map[string]any, cat.ActionContext) (any, error) {
				return map[string]any{}, nil
			},
		},
		Setup: setup,
	}
}
