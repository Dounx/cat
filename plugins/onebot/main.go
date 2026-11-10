// Command cat-plugin-onebot connects Cat to QQ through a OneBot 11
// implementation. It can run NapCat itself, or use one you already run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/jsonrpc"
)

func main() { cat.Run(New()) }

// pollInterval is how often a pending NapCat login is re-checked.
var pollInterval = 2 * time.Second

func timeAfter() <-chan time.Time { return time.After(pollInterval) }

func str(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return strings.TrimSpace(s)
	}
	if m[k] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[k]))
}

func managedURL(cfg map[string]any) string { return "ws://127.0.0.1:" + str(cfg, "ws_port") }

func New() cat.Plugin {
	var (
		c  *client
		nc *napcat
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			url := str(h.Config, "ws_url")
			if mode := str(h.Config, "mode"); mode == "managed" || mode == "docker" {
				url = managedURL(h.Config)
				var err error
				if mode == "docker" {
					nc, err = startDocker(ctx, h)
				} else {
					nc, err = startManaged(ctx, h)
				}
				if err != nil {
					return err
				}
				lctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				_, err = awaitLogin(lctx, nc, newClient(url, str(h.Config, "token")), nil, func(l string) { h.Log("debug", "%s", l) })
				cancel()
				if err != nil {
					nc.stop()
					return errors.New("NapCat 没有完成登录（登录状态可能已失效，请重新运行接入向导扫码）：" + err.Error())
				}
				go func() { // keep NapCat's console from filling up
					for l := range nc.lines {
						h.Log("debug", "%s", l)
					}
				}()
			}
			if url == "" {
				return errors.New("缺少 WebSocket 地址")
			}
			c = newClient(url, str(h.Config, "token"))
			onEvent := func(raw json.RawMessage) {
				if ev, ok := convert(raw); ok {
					if _, err := h.Emit(ev); err != nil {
						h.Log("error", "emit: %v", err)
					}
				}
			}
			done, err := c.connect(ctx, onEvent)
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
						if done, err = c.connect(ctx, onEvent); err == nil {
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
			nc.stop()
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

func startManaged(ctx context.Context, h *cat.Host) (*napcat, error) {
	dep, err := h.Dependency(ctx, "napcat")
	if err != nil {
		return nil, err
	}
	dir, err := installNapCat(dep, h.DataDir)
	if err != nil {
		return nil, err
	}
	return startNapCat(dir, str(h.Config, "uin"))
}

func startDocker(ctx context.Context, h *cat.Host) (*napcat, error) {
	if err := dockerReady(); err != nil {
		return nil, err
	}
	if !dockerImagePresent(dockerImage) { // e.g. the pinned version moved on
		h.Log("info", "正在拉取镜像 %s", dockerImage)
		if err := dockerPull(ctx, dockerImage, nil); err != nil {
			return nil, err
		}
	}
	port, err := strconv.Atoi(str(h.Config, "ws_port"))
	if err != nil {
		return nil, errors.New("缺少端口配置，请重新运行接入向导")
	}
	return startDockerNapCat(h.DataDir, port, str(h.Config, "uin"))
}

func cancelled(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.Is(err, cat.ErrCancelled) || (errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeCancelled)
}
