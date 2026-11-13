// Command cat-plugin-github connects Cat to GitHub: the issues and pull
// requests of the watched repositories are conversations, what is written in
// them are messages, and a reply is a comment.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/dounx/cat/plugins/internal/poller"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func str(m map[string]any, k string) string {
	if m[k] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[k]))
}

func New() cat.Plugin {
	var (
		mu sync.Mutex // Start and Triggers, one at a time
		b  *bot
		p  *poller.Poller[target]
	)
	return cat.Plugin{
		// An instance is an account. What to watch with it is said by the
		// rules, in their trigger nodes; with no rule yet there is nothing to
		// look at, which is as it should be.
		Start: func(ctx context.Context, h *cat.Host) error {
			mu.Lock()
			defer mu.Unlock()
			nb := newBot(h.Config)
			if err := nb.getMe(ctx); err != nil {
				return err
			}
			nb.watch(ctx, h, h.Triggers(), true)
			b = nb
			h.Log("info", "已连接 @%s", b.me.Login)
			p = poller.New(b.targets, b.check, poller.Words{Failed: "检查失败", AllFailed: "全部检查失败"})
			go p.Run(ctx, h)
			return nil
		},
		Triggers: func(ctx context.Context, h *cat.Host, list []cat.Trigger) map[string]string {
			mu.Lock()
			defer mu.Unlock()
			if b == nil { // before Start, which takes the list itself
				_, _, _, errs := wanted(list)
				return errs
			}
			errs := b.watch(ctx, h, list, false)
			p.Wake()
			return errs
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, _ *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「回复」需要由一条 GitHub 消息触发；要主动发评论请用「发送到指定会话」")
				}
				return b.comment(ctx, ev.Conversation.ID, str(p, "text"))
			},
			"send": func(ctx context.Context, _ *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				conv := str(p, "conversation_id")
				if conv == "" {
					return nil, errors.New("缺少会话：写成 owner/repo#编号")
				}
				return b.comment(ctx, conv, str(p, "text"))
			},
			"create_issue": func(ctx context.Context, _ *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				return b.createIssue(ctx, str(p, "repo"), str(p, "title"), str(p, "text"), str(p, "labels"))
			},
			"typing": func(ctx context.Context, _ *cat.Host, _ map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「表示已看到」需要由一条 GitHub 消息触发")
				}
				return map[string]any{}, b.react(ctx, ev, "eyes")
			},
		},
		Options: func(ctx context.Context, _ *cat.Host, action, field string) ([]cat.Option, error) {
			if b == nil || !(action == "@trigger" && field == "repos" || action == "create_issue" && field == "repo") {
				return nil, nil
			}
			// the watched ones first, then whatever else the account can reach
			out, seen := []cat.Option{}, map[string]bool{}
			for _, t := range b.targets() {
				if r, ok := t.(*repo); ok {
					out, seen[strings.ToLower(r.name)] = append(out, cat.Option{Value: r.name, Note: "已关注"}), true
				}
			}
			mine, err := b.myRepos(ctx)
			for _, o := range mine {
				if !seen[strings.ToLower(o.Value)] {
					out = append(out, o)
				}
			}
			if len(out) > 0 {
				err = nil
			}
			return out, err
		},
		Ingress: func(ctx context.Context, h *cat.Host, req cat.IngressRequest) (cat.IngressResponse, error) {
			if b == nil {
				return cat.IngressResponse{Status: 503}, nil
			}
			return b.ingress(ctx, h, req)
		},
		Setup: setup,
	}
}
