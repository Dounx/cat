package main

import (
	"context"

	cat "github.com/dounx/cat/sdk/go"
)

// setup asks where the OneBot service is and checks that it answers before
// the instance is saved.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	url := str(s.Existing, "ws_url")
	if url == "" {
		url = "ws://127.0.0.1:3001"
	}
	problem := ""
	for {
		desc := "填写你运行的 OneBot 11 服务的正向 WebSocket 地址和访问令牌。\n在你用的 OneBot 实现里开启「正向 WebSocket 服务」（WebSocket 服务端）即可得到。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 OneBot 服务", Description: desc,
			Values: map[string]any{"ws_url": url},
			Schema: map[string]any{"type": "object", "required": []string{"ws_url"}, "x-order": []string{"ws_url", "token"}, "properties": map[string]any{
				"ws_url": map[string]any{"type": "string", "title": "WebSocket 地址"},
				"token":  map[string]any{"type": "string", "title": "访问令牌", "format": "password"},
			}},
		})
		if err != nil {
			return nil, err
		}
		url = str(out, "ws_url")
		s.Progress("正在连接…", -1)
		c := newClient(url, str(out, "token"))
		done, err := c.connect(ctx, nil)
		if err != nil {
			problem = err.Error()
			continue
		}
		info, err := c.login(ctx)
		c.close()
		<-done
		if err != nil {
			problem = err.Error()
			continue
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 QQ " + info.Nickname + "（" + info.UserID.String() + "）"})
		return map[string]any{"ws_url": url, "token": out["token"]}, nil
	}
}
