package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

// Scan-to-bind, as Tencent's own openclaw-qqbot CLI does it: ask q.qq.com for
// a bind task, show its page as a QR code, and poll until the user has
// scanned it with QQ and picked (or created) a bot. The bot's secret comes
// back encrypted with a key only we hold. These endpoints are not part of
// the documented bot API.
var (
	bindBase      = "https://q.qq.com"
	bindPollEvery = 2 * time.Second
)

const (
	bindPending = 1
	bindDone    = 2
	bindExpired = 3
)

func bindCall(ctx context.Context, path string, in map[string]string, out any) error {
	body, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", bindBase+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 q.qq.com：%w", err)
	}
	defer resp.Body.Close()
	env := struct {
		RetCode int    `json:"retcode"`
		Msg     string `json:"msg"`
		Data    any    `json:"data"`
	}{Data: out}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("q.qq.com 返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if env.RetCode != 0 {
		return fmt.Errorf("q.qq.com（%d）：%s", env.RetCode, env.Msg)
	}
	return nil
}

// openSecret decrypts what poll_bind_result returns: base64 of a 12-byte IV,
// the AES-256-GCM ciphertext and its tag.
func openSecret(sealed string, key []byte) (string, error) {
	data, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("密文太短")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	return string(plain), err
}

// scan shows QR codes until one is scanned, and returns the bot's credentials.
func scan(ctx context.Context, s *cat.Setup) (appID, secret string, err error) {
	for {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return "", "", err
		}
		var task struct {
			TaskID string `json:"task_id"`
		}
		if err := bindCall(ctx, "/lite/create_bind_task", map[string]string{"key": base64.StdEncoding.EncodeToString(key)}, &task); err != nil {
			return "", "", err
		}
		if task.TaskID == "" {
			return "", "", errors.New("q.qq.com 没有给出二维码")
		}
		s.Show(cat.Step{
			Kind: cat.StepQRCode, Title: "用手机 QQ 扫码",
			Description: "扫码后在手机上选一个已有的机器人，或者新建一个，然后确认。二维码过期会自动换新的。\n这是 QQ 给 OpenClaw 准备的绑定入口，手机上的页面会提到 OpenClaw，照常确认即可。",
			Text:        "https://q.qq.com/qqbot/openclaw/connect.html?task_id=" + url.QueryEscape(task.TaskID) + "&_wv=2",
		})
		for pending := true; pending; {
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(bindPollEvery):
			}
			var res struct {
				Status int    `json:"status"`
				AppID  any    `json:"bot_appid"`
				Secret string `json:"bot_encrypt_secret"`
			}
			if err := bindCall(ctx, "/lite/poll_bind_result", map[string]string{"task_id": task.TaskID}, &res); err != nil {
				if ctx.Err() != nil {
					return "", "", ctx.Err()
				}
				continue // a blip: keep waiting on the same code
			}
			switch res.Status {
			case bindDone:
				secret, err := openSecret(res.Secret, key)
				if err != nil {
					return "", "", errors.New("解不开 QQ 返回的 AppSecret")
				}
				return fmt.Sprint(res.AppID), secret, nil
			case bindExpired:
				pending = false
			}
		}
	}
}

// setup gets the bot's credentials, by scanning a QR code or typed in by
// hand, and checks them against the open platform before the instance is
// saved.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	problem := ""
	for {
		desc := "扫码最省事：用手机 QQ 扫一下，选一个机器人或当场新建一个，不用自己复制 AppID 和 AppSecret。\n已经在 QQ 开放平台（q.qq.com）有机器人、或者要用沙箱环境时，选手动填写。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 QQ 官方机器人", Description: desc,
			Values: map[string]any{"method": "qrcode"},
			Schema: map[string]any{
				"type": "object", "required": []string{"method"},
				"properties": map[string]any{"method": map[string]any{
					"type": "string", "title": "怎么接入",
					"enum": []string{"qrcode", "manual"}, "x-enum-titles": []string{"用手机 QQ 扫码", "手动填写 AppID 和 AppSecret"},
				}},
			},
		})
		if err != nil {
			return nil, err
		}
		if str(out, "method") == "manual" {
			return manual(ctx, s, cfg)
		}
		s.Progress("正在获取二维码…", -1)
		appID, secret, err := scan(ctx, s)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			problem = "扫码接入没有成功：" + err.Error()
			continue
		}
		cfg["app_id"], cfg["secret"] = appID, secret
		s.Progress("正在连接 QQ…", -1)
		if err := verify(ctx, s, cfg); err != nil {
			problem = err.Error()
			continue
		}
		return cfg, nil
	}
}

// manual asks for the credentials until a pair works.
func manual(ctx context.Context, s *cat.Setup, cfg map[string]any) (map[string]any, error) {
	problem := ""
	for {
		desc := "1. 在 QQ 开放平台（q.qq.com）创建一个机器人。\n" +
			"2. 打开「开发 → 开发设置」，复制 AppID 和 AppSecret，并把这台机器的出口 IP 加进「IP 白名单」。\n" +
			"3. 机器人还没上线时，在「沙箱配置」里加上用来调试的群和 QQ 号，并打开下面的「沙箱环境」。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "填写机器人的凭据", Description: desc, URL: "https://q.qq.com/qqbot/",
			Values: map[string]any{"app_id": str(cfg, "app_id"), "sandbox": cfg["sandbox"] == true},
			Schema: map[string]any{
				"type": "object", "required": []string{"app_id", "secret"}, "x-order": []string{"app_id", "secret", "sandbox"},
				"properties": map[string]any{
					"app_id":  map[string]any{"type": "string", "title": "AppID"},
					"secret":  map[string]any{"type": "string", "title": "AppSecret", "format": "password"},
					"sandbox": map[string]any{"type": "boolean", "title": "沙箱环境"},
				},
			},
		})
		if err != nil {
			return nil, err
		}
		cfg["app_id"], cfg["secret"], cfg["sandbox"] = str(out, "app_id"), str(out, "secret"), out["sandbox"] == true
		s.Progress("正在连接 QQ…", -1)
		if err := verify(ctx, s, cfg); err != nil {
			problem = err.Error()
			continue
		}
		return cfg, nil
	}
}

// verify checks cfg's credentials and says who they belong to. It stays off
// the gateway: the instance logs in there right after the wizard, and QQ
// refuses two logins in quick succession.
func verify(ctx context.Context, s *cat.Setup, cfg map[string]any) error {
	b := newBot(cfg)
	if err := b.check(ctx); err != nil {
		return err
	}
	title := "已连接"
	if b.name != "" {
		title += " " + b.name
	}
	s.Show(cat.Step{Kind: cat.StepInfo, Title: title,
		Description: "在群里 @ 这个机器人，或者和它单聊，就能在「记录」页看到。QQ 的机器人只能在收到消息后回复，主动发消息的次数很少。"})
	return nil
}
