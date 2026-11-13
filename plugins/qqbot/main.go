// Command cat-plugin-qqbot connects Cat to QQ through an official bot of the
// QQ open platform (q.qq.com): group chats and one-to-one chats, received over
// the gateway WebSocket.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func str(m map[string]any, k string) string {
	if m[k] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[k]))
}

// replyWindow is how long after a message QQ still takes a reply to it.
func replyWindow(ev *cat.Event) time.Duration {
	if ev.Conversation.Kind == "group" {
		return 5 * time.Minute
	}
	return time.Hour
}

func New() cat.Plugin {
	var b *bot
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			b = newBot(h.Config)
			emit := func(ev cat.Event) {
				if _, err := h.Emit(ev); err != nil {
					h.Log("error", "emit: %v", err)
				}
			}
			conn, interval, err := b.dial(ctx, emit)
			if err != nil {
				return err
			}
			h.Log("info", "已连接 QQ 机器人 %s", b.name)
			go b.run(ctx, h, conn, interval, emit)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, _ *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「回复」需要由一条聊天消息触发；要主动发消息请用「发送到指定会话」")
				}
				return b.send(ctx, ev.Conversation.ID, str(p, "text"), str(ev.Payload, "message_id"), p["markdown"] == true)
			},
			"send": func(ctx context.Context, _ *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				conv := str(p, "conversation_id")
				if conv == "" {
					return nil, errors.New("缺少会话 ID")
				}
				// Proactive messages are rationed, replies are not: when this
				// goes to the conversation the trigger just came from, send it
				// as a reply to that message.
				replyTo := ""
				if ev := actx.Event; ev != nil && ev.Conversation != nil && ev.Conversation.ID == conv &&
					ev.Time != nil && time.Since(*ev.Time) < replyWindow(ev)-30*time.Second {
					replyTo = str(ev.Payload, "message_id")
				}
				return b.send(ctx, conv, str(p, "text"), replyTo, p["markdown"] == true)
			},
			"typing": func(ctx context.Context, _ *cat.Host, _ map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「正在输入」需要由一条聊天消息触发")
				}
				msgID := str(ev.Payload, "message_id")
				if ev.Conversation.Kind != "private" || msgID == "" {
					return map[string]any{}, nil
				}
				path, err := messagesPath(ev.Conversation.ID)
				if err != nil {
					return nil, err
				}
				_, err = b.post(ctx, path, map[string]any{"msg_type": 6, "input_notify": map[string]int{"input_type": 1, "input_second": 60}}, msgID)
				return map[string]any{}, err
			},
		},
		Setup: setup,
	}
}
