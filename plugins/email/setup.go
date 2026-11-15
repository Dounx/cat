package main

import (
	"context"
	"strings"

	cat "github.com/dounx/cat/sdk/go"
)

// A provider is where a mail service takes IMAP and SMTP, and what it asks
// of its users before it does.
type provider struct{ imap, smtp, note string }

const needsCode = "要先在邮箱的设置里开启 IMAP/SMTP 服务，并生成授权码；下面填授权码，不是登录密码。"

var providers = map[string]provider{
	"gmail.com":   {"imap.gmail.com:993", "smtp.gmail.com:465", "要先开启两步验证，再在 Google 账号里生成「应用专用密码」，下面填它。"},
	"qq.com":      {"imap.qq.com:993", "smtp.qq.com:465", needsCode},
	"foxmail.com": {"imap.qq.com:993", "smtp.qq.com:465", needsCode},
	"163.com":     {"imap.163.com:993", "smtp.163.com:465", needsCode},
	"126.com":     {"imap.126.com:993", "smtp.126.com:465", needsCode},
	"yeah.net":    {"imap.yeah.net:993", "smtp.yeah.net:465", needsCode},
	"sina.com":    {"imap.sina.com:993", "smtp.sina.com:465", needsCode},
	"aliyun.com":  {"imap.aliyun.com:993", "smtp.aliyun.com:465", ""},
	"icloud.com":  {"imap.mail.me.com:993", "smtp.mail.me.com:587", "要在 Apple 账户里生成「App 专用密码」，下面填它。"},
	"me.com":      {"imap.mail.me.com:993", "smtp.mail.me.com:587", "要在 Apple 账户里生成「App 专用密码」，下面填它。"},
	"outlook.com": {"outlook.office365.com:993", "smtp-mail.outlook.com:587", "微软的个人邮箱大多已经不让用密码登录 IMAP/SMTP，连不上时这个邮箱暂时接不了。"},
	"hotmail.com": {"outlook.office365.com:993", "smtp-mail.outlook.com:587", "微软的个人邮箱大多已经不让用密码登录 IMAP/SMTP，连不上时这个邮箱暂时接不了。"},
}

// guess gives the servers of an address: the provider's when it is known,
// else the names most services of their own use.
func guess(address string) provider {
	_, domain, _ := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if p, ok := providers[domain]; ok {
		return p
	}
	if domain == "" {
		return provider{}
	}
	return provider{imap: "imap." + domain + ":993", smtp: "smtp." + domain + ":465"}
}

// verify signs in both ways, without reading or sending anything.
func verify(ctx context.Context, cfg map[string]any) error {
	a, err := newAccount(cfg)
	if err != nil {
		return err
	}
	c, _, err := a.open()
	if err != nil {
		return err
	}
	_ = c.Logout().Wait()
	c.Close()
	s, err := a.dial(ctx)
	if err != nil {
		return err
	}
	return s.Quit()
}

// setup asks for the address and its password, works out the servers, and
// checks that both let the account in before the instance is saved. The
// servers are asked for only when the guess does not work.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	problem, servers := "", false
	for {
		desc := "填邮箱地址和密码。收信用 IMAP，发信用 SMTP，常见邮箱的服务器会自动填好。\nGmail、QQ 邮箱、163、iCloud 等不让直接用登录密码：要在邮箱设置里开启 IMAP/SMTP，生成授权码（应用专用密码）填在这里。"
		if p := guess(str(cfg, "address")); p.note != "" {
			desc += "\n\n这个邮箱：" + p.note
		}
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		props := map[string]any{
			"address":  map[string]any{"type": "string", "title": "邮箱地址"},
			"password": map[string]any{"type": "string", "title": "密码或授权码", "format": "password"},
		}
		order := []string{"address", "password"}
		if servers {
			props["imap"] = map[string]any{"type": "string", "title": "IMAP 服务器（主机:端口）"}
			props["smtp"] = map[string]any{"type": "string", "title": "SMTP 服务器（主机:端口）"}
			props["insecure"] = map[string]any{"type": "boolean", "title": "不加密连接（只用于本机或内网的测试服务器）"}
			order = append(order, "imap", "smtp", "insecure")
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接邮箱", Description: desc,
			Values: map[string]any{"address": cfg["address"], "imap": cfg["imap"], "smtp": cfg["smtp"], "insecure": cfg["insecure"]},
			Schema: map[string]any{"type": "object", "required": []string{"address", "password"}, "x-order": order, "properties": props},
		})
		if err != nil {
			return nil, err
		}
		cfg["address"], cfg["password"] = out["address"], out["password"]
		if servers {
			cfg["imap"], cfg["smtp"], cfg["insecure"] = out["imap"], out["smtp"], out["insecure"]
		}
		if p := guess(str(cfg, "address")); !servers || str(cfg, "imap") == "" || str(cfg, "smtp") == "" {
			cfg["imap"], cfg["smtp"] = p.imap, p.smtp
		}
		s.Progress("正在登录邮箱…", -1)
		if err := verify(ctx, cfg); err != nil {
			problem, servers = err.Error()+"\n服务器地址不对的话，可以在下面改。", true
			continue
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 " + str(cfg, "address"),
			Description: "从现在起收到的新邮件会出现在「记录」页，之前的邮件不会。默认每 30 秒查一次。"})
		return cfg, nil
	}
}
