package main

import (
	"context"
	"errors"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"

	cat "github.com/dounx/cat/sdk/go"
)

var registerDomain, registerLarkDomain string

// scan has the open platform create the app: it gives a page to open, shown
// as a QR code, on which the user confirms in Feishu; the app's credentials
// come back once that is done. The app it makes is a bot that receives
// messages over the long connection, which is all this plugin needs.
func scan(ctx context.Context, s *cat.Setup) (appID, secret, domain string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res, err := registration.RegisterApp(ctx, &registration.Options{
		Source:     "cat",
		Domain:     registerDomain,
		LarkDomain: registerLarkDomain,
		AppPreset:  &registration.AppPreset{Name: "Cat", Desc: "Cat 事件中心的机器人：把这里的消息接到规则和 AI 上。"},
		OnQRCode: func(info *registration.QRCodeInfo) {
			s.Show(cat.Step{
				Kind: cat.StepQRCode, Title: "用飞书扫码",
				Description: "扫码后在手机上确认创建一个机器人应用，名字和头像可以当场改。确认后这里会自动继续。\n在电脑上已经登录飞书的话，也可以直接打开下面的链接。",
				Text:        info.URL, URL: info.URL,
			})
		},
	})
	if err != nil {
		var failed *registration.RegisterAppError
		if errors.As(err, &failed) {
			return "", "", "", errors.New(failed.Description)
		}
		return "", "", "", err
	}
	// the account that scanned says which edition the app lives in
	domain = "feishu"
	if res.UserInfo != nil && res.UserInfo.TenantBrand == "lark" {
		domain = "lark"
	}
	return res.ClientID, res.ClientSecret, domain, nil
}

// setup gets the app's credentials, by scanning a QR code or typed in by
// hand, and checks them before the instance is saved.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	problem := ""
	for {
		desc := "扫码最省事：用飞书扫一下，确认创建一个机器人，不用自己到开放平台建应用。\n已经有自建应用、或者用的是 Lark 时，选手动填写。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接飞书机器人", Description: desc,
			Values: map[string]any{"method": "qrcode"},
			Schema: map[string]any{
				"type": "object", "required": []string{"method"},
				"properties": map[string]any{"method": map[string]any{
					"type": "string", "title": "怎么接入",
					"enum": []string{"qrcode", "manual"}, "x-enum-titles": []string{"用飞书扫码创建机器人", "手动填写 App ID 和 App Secret"},
				}},
			},
		})
		if err != nil {
			return nil, err
		}
		if str(out, "method") == "manual" {
			out, err := s.Ask(ctx, cat.Step{
				Kind: cat.StepForm, Title: "填写应用凭证", URL: "https://open.feishu.cn/app",
				Description: "1. 在飞书开放平台创建企业自建应用，添加「机器人」能力。\n" +
					"2. 「权限管理」里开通：获取与发送单聊、群组消息（im:message）、以应用身份发消息（im:message:send_as_bot）；要收到群里没有 @ 机器人的消息，再开通「获取群组中所有消息」。\n" +
					"3. 「事件与回调」里订阅方式选「使用长连接接收事件」，添加事件「接收消息 im.message.receive_v1」。这一步要在 Cat 接入之后才能保存，可以先填好下面的再回去设置。\n" +
					"4. 创建版本并发布，然后把「凭证与基础信息」里的 App ID 和 App Secret 填到下面。",
				Values: map[string]any{"app_id": cfg["app_id"], "domain": "feishu"},
				Schema: map[string]any{
					"type": "object", "required": []string{"app_id", "app_secret"}, "x-order": []string{"app_id", "app_secret", "domain"},
					"properties": map[string]any{
						"app_id":     map[string]any{"type": "string", "title": "App ID"},
						"app_secret": map[string]any{"type": "string", "title": "App Secret", "format": "password"},
						"domain": map[string]any{"type": "string", "title": "版本", "enum": []string{"feishu", "lark"},
							"x-enum-titles": []string{"飞书（feishu.cn）", "Lark（larksuite.com）"}},
					},
				},
			})
			if err != nil {
				return nil, err
			}
			cfg["app_id"], cfg["app_secret"], cfg["domain"] = out["app_id"], out["app_secret"], out["domain"]
		} else {
			s.Progress("正在获取二维码…", -1)
			appID, secret, domain, err := scan(ctx, s)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				problem = "扫码接入没有成功：" + err.Error()
				continue
			}
			cfg["app_id"], cfg["app_secret"], cfg["domain"] = appID, secret, domain
		}
		s.Progress("正在验证…", -1)
		b, err := newBot(cfg, nil)
		if err == nil {
			err = b.identify(ctx)
		}
		if err != nil {
			problem = err.Error()
			continue
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接「" + b.name + "」",
			Description: "在飞书里给这个机器人发一条消息，就能在「记录」页看到。要在群里用，先把它加进群；群里默认只有 @ 它的消息才会收到。"})
		return cfg, nil
	}
}
