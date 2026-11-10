package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"runtime"
	"strconv"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	mode := str(cfg, "mode")
	if mode == "" {
		mode = "external"
		if runtime.GOOS == "windows" {
			mode = "managed"
		}
	}
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "怎样连接 QQ",
		Description: "managed：由 Cat 下载、配置并启动 NapCat，你只需要扫码登录（目前支持 Windows，需要已安装 QQ）。\nexternal：连接你已经在运行的 OneBot 11 服务（NapCat、LLOneBot 等）。",
		Values:      map[string]any{"mode": mode},
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"mode": map[string]any{"type": "string", "title": "方式", "enum": []string{"managed", "external"}},
		}},
	})
	if err != nil {
		return nil, err
	}
	if str(out, "mode") == "managed" {
		managed, err := setupManaged(ctx, s, cfg)
		if err == nil {
			return managed, nil
		}
		if cancelled(err) || ctx.Err() != nil {
			return nil, err
		}
		// Automatic setup is not possible here: say why, then fall back to
		// connecting to a service the user runs themselves.
		ok, aerr := s.Ask(ctx, cat.Step{
			Kind: cat.StepConfirm, Title: "无法自动配置 NapCat", URL: "https://napneko.github.io/guide/boot/Shell",
			Description: err.Error() + "\n\n你可以按 NapCat 文档自行安装并启动它，在它的 WebUI 里新建一个「WebSocket 服务端」，然后点确认，填写它的地址和令牌。",
		})
		if aerr != nil {
			return nil, aerr
		}
		if c, _ := ok["confirmed"].(bool); !c {
			return nil, cat.ErrCancelled
		}
	}
	return setupExternal(ctx, s, cfg)
}

func setupExternal(ctx context.Context, s *cat.Setup, cfg map[string]any) (map[string]any, error) {
	url := str(cfg, "ws_url")
	if url == "" {
		url = "ws://127.0.0.1:3001"
	}
	problem := ""
	for {
		desc := "填写 OneBot 11 正向 WebSocket 服务的地址和访问令牌。\n在 NapCat 的 WebUI →「网络配置」里新建「WebSocket 服务端」即可得到。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接已有的 OneBot 服务", Description: desc, URL: "https://napneko.github.io/config/basic",
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
		return map[string]any{"mode": "external", "ws_url": url, "token": out["token"]}, nil
	}
}

func setupManaged(ctx context.Context, s *cat.Setup, cfg map[string]any) (map[string]any, error) {
	for !qqInstalled() {
		ok, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepConfirm, Title: "需要先安装 QQ", URL: "https://im.qq.com/pcqq/index.shtml",
			Description: "NapCat 需要本机已安装 QQ。请安装后点确认重新检测。",
		})
		if err != nil {
			return nil, err
		}
		if c, _ := ok["confirmed"].(bool); !c {
			return nil, cat.ErrCancelled
		}
	}
	// The host shows where NapCat comes from and asks before downloading.
	dep, err := s.Dependency(ctx, "napcat")
	if err != nil {
		return nil, err
	}
	s.Progress("正在准备 NapCat…", -1)
	dir, err := installNapCat(dep, s.DataDir)
	if err != nil {
		return nil, err
	}
	wsPort, err := freePort()
	if err != nil {
		return nil, err
	}
	webuiPort, err := freePort()
	if err != nil {
		return nil, err
	}
	token := str(cfg, "token")
	if token == "" || strings.HasPrefix(token, "•") {
		token = randomToken()
	}
	if err := writeNapCatConfig(dir, wsPort, token, webuiPort, randomToken()); err != nil {
		return nil, err
	}
	s.Progress("正在启动 NapCat…", -1)
	n, err := startNapCat(dir, "")
	if err != nil {
		return nil, err
	}
	defer n.stop()
	lctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c := newClient("ws://127.0.0.1:"+strconv.Itoa(wsPort), token)
	info, err := awaitLogin(lctx, n, c, func(url string) {
		s.Show(cat.Step{Kind: cat.StepQRCode, Title: "用手机 QQ 扫码登录", Text: url,
			Description: "打开手机 QQ →「扫一扫」，扫描后在手机上确认登录。二维码过期会自动刷新。"})
	}, nil)
	if err != nil {
		return nil, err
	}
	s.Show(cat.Step{Kind: cat.StepInfo, Title: "已登录 " + info.Nickname + "（" + info.UserID.String() + "）",
		Description: "之后启动会自动用这个账号快速登录。登录状态失效时，重新运行接入向导扫码即可。"})
	return map[string]any{"mode": "managed", "ws_port": wsPort, "token": token, "uin": info.UserID.String()}, nil
}
