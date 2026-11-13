package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/harness"
	cat "github.com/dounx/cat/sdk/go"
)

// Signing in from the wizard is left to the GitHub CLI (gh). GitHub counts
// it among its own apps, so its sign-in also reaches the repositories of an
// organization that keeps third-party apps out, where one of ours would wait
// for an owner's approval. gh is only used here, to get a token: the
// instance then runs on that token like on one typed in.

// ghInstalled finds a gh this machine already has. Replaced in tests.
var ghInstalled = func() (string, bool) {
	bin, err := harness.FindBin("gh", "")
	return bin, err == nil
}

// ghPath finds gh: the one installed on this machine, or else the one Cat
// downloads for the plugin, after asking.
func ghPath(ctx context.Context, s *cat.Setup) (string, error) {
	if bin, ok := ghInstalled(); ok {
		return bin, nil
	}
	dir, err := s.Dependency(ctx, "gh")
	if err != nil {
		return "", err
	}
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(dir, "bin", name), nil
}

// ghHost is the host gh knows an API address by.
func ghHost(base string) string {
	return strings.TrimPrefix(strings.TrimPrefix(webBase(base), "https://"), "http://")
}

func ghCommand(ctx context.Context, bin, configDir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = harness.PathEnv("GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1")
	if configDir != "" {
		cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+configDir)
	}
	return cmd
}

// ghToken is the token of a login gh has: its own on this machine, or with
// configDir the one made there.
func ghToken(ctx context.Context, bin, configDir, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := ghCommand(ctx, bin, configDir, "auth", "token", "--hostname", host).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var (
	ghCode = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)
	ghLink = regexp.MustCompile(`https?://\S+`)
)

// ghSignIn has gh sign in through the browser: gh prints a code to enter on
// GitHub, the wizard shows it, and gh waits until that has been done. The
// login is made in a directory of its own and thrown away once the token is
// out, so the login gh may already have on this machine is left alone, and
// the bot can be another account than the one that uses gh.
func ghSignIn(ctx context.Context, s *cat.Setup, bin, host string) (string, error) {
	// A wizard that is closed half way takes this process with it, and leaves
	// its directory behind: clear away what earlier ones have left.
	stale, _ := filepath.Glob(filepath.Join(os.TempDir(), "cat-gh-*"))
	for _, old := range stale {
		if st, err := os.Stat(old); err == nil && time.Since(st.ModTime()) > time.Hour {
			os.RemoveAll(old)
		}
	}
	dir, err := os.MkdirTemp("", "cat-gh-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cmd := ghCommand(ctx, bin, dir, "auth", "login", "--hostname", host, "--git-protocol", "https", "--web", "--clipboard", "--insecure-storage", "--skip-ssh-key")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	code, pasted, said := "", "", []string{}
	for lines := bufio.NewScanner(stderr); lines.Scan(); {
		line := strings.TrimSpace(lines.Text())
		if line == "" {
			continue
		}
		said = append(said, line)
		if c := ghCode.FindString(line); c != "" {
			code = c
			if strings.Contains(line, "copied to clipboard") {
				// GitHub's page takes no code from the link, but it takes one pasted
				pasted = "验证码已复制到剪贴板"
			}
		}
		if link := ghLink.FindString(line); link != "" && code != "" {
			s.Show(cat.Step{
				Kind: cat.StepQRCode, Title: "在 GitHub 上输入 " + code,
				Description: "打开链接（或用手机扫码），登录要给 Cat 用的那个账号，输入上面的验证码并授权。\n" +
					"授权页上写的是 GitHub CLI：登录是借它完成的。评论会以这个账号的名义发出。授权完成后这里会自动继续。",
				URL: link, Text: link, Notice: pasted,
			})
			code = ""
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if len(said) > 0 {
			return "", errors.New(said[len(said)-1])
		}
		return "", err
	}
	token := ghToken(ctx, bin, dir, host)
	if token == "" {
		return "", errors.New("gh 登录完成了，却没有给出令牌")
	}
	return token, nil
}

const tokenHelp = "1. 在 GitHub 的 Settings → Developer settings → Personal access tokens 里新建一个令牌：" +
	"细粒度令牌选上要用到的仓库，给 Issues 和 Pull requests 读写权限；经典令牌勾选 repo（只用公开仓库时 public_repo 即可）。\n" +
	"2. 把它粘贴到下面。\n\n" +
	"规则里要用「在任何仓库被 @ 都接收」的话，得是经典令牌，细粒度令牌读不了通知。"

// setup signs in to GitHub, in the browser or with a token typed in, and
// checks the token before the instance is saved. An instance is an account
// and no more: what to watch with it is said by the rules.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	base := str(cfg, "api_base")
	if base == "" {
		base = defaultBase
	}
	host := ghHost(base)
	problem, method := "", ""
	for {
		// A login gh already has on this machine can be used as it is.
		bin, existing := "", ""
		if found, ok := ghInstalled(); ok {
			if token := ghToken(ctx, found, "", host); token != "" {
				if probe := newBot(map[string]any{"api_base": cfg["api_base"], "token": token}); probe.getMe(ctx) == nil {
					bin, existing = found, probe.me.Login
				}
			}
		}
		methods, titles := []string{"browser", "token"}, []string{"在浏览器里登录 GitHub", "自己填访问令牌"}
		notes := []string{
			"打开 GitHub 输入一个验证码就行，组织的仓库不用等管理员批准。借 GitHub CLI（gh）完成，本机没有时会先问你要不要下载。",
			"想精确控制权限（比如只给某几个仓库）时用。",
		}
		if existing != "" {
			methods, titles = append([]string{"existing"}, methods...), append([]string{"用本机 gh 已有的登录（@" + existing + "）"}, titles...)
			notes = append([]string{"直接用，不用再登录。"}, notes...)
		}
		desc := "评论会以登录的这个账号发出。"
		if method == "" || (method == "existing" && existing == "") {
			method = methods[0]
		}
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 GitHub", Description: desc,
			Schema: map[string]any{"type": "object", "required": []string{"method"}, "properties": map[string]any{"method": map[string]any{
				"type": "string", "title": "怎么登录", "enum": methods, "x-enum-titles": titles, "x-enum-notes": notes,
			}}},
			Values: map[string]any{"method": method},
		})
		if err != nil {
			return nil, err
		}
		problem = ""
		if m := str(out, "method"); m != "" {
			method = m
		}
		switch method {
		case "token":
			if cfg["token"], err = askToken(ctx, s); err != nil {
				return nil, err
			}
		case "existing":
			cfg["token"] = ghToken(ctx, bin, "", host)
		default:
			s.Progress("正在准备 GitHub CLI…", -1)
			bin, err := ghPath(ctx, s)
			if err == nil {
				s.Progress("正在向 GitHub 要验证码…", -1)
				cfg["token"], err = ghSignIn(ctx, s, bin, host)
			}
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, cat.ErrCancelled) {
					return nil, err
				}
				problem = "登录没有成功：" + err.Error()
				continue
			}
		}
		s.Progress("正在验证…", -1)
		b := newBot(cfg)
		if err := b.getMe(ctx); err != nil {
			problem = err.Error()
			continue
		}

		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 @" + b.me.Login,
			Description: "接下来在规则里用它：触发节点选这个接入，再选要关注哪些仓库；回复、发评论、新建 Issue 是动作节点。评论会以 @" + b.me.Login + " 的名义发出，在评论里 @" + b.me.Login + " 就是提到了机器人。\n\n" +
				"消息是定时查来的，会晚几十秒。想实时收到，在这个接入的设置里填「Webhook 密钥」，按那里的说明到 GitHub 添加 Webhook。"})
		return cfg, nil
	}
}

// askToken asks for a token typed in by hand.
func askToken(ctx context.Context, s *cat.Setup) (any, error) {
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "填写访问令牌", Description: tokenHelp, URL: "https://github.com/settings/personal-access-tokens/new",
		Schema: map[string]any{
			"type": "object", "required": []string{"token"},
			"properties": map[string]any{"token": map[string]any{"type": "string", "title": "访问令牌（Token）", "format": "password"}},
		},
	})
	if err != nil {
		return nil, err
	}
	return out["token"], nil
}
