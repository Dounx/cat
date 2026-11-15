package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/logger"

	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultBase = "https://api.dingtalk.com"
	// userPrefix marks a conversation id that is a one-to-one chat: what
	// follows is the user's id in the organisation, which is what a message
	// to them is addressed by. A group goes by its conversation id.
	userPrefix = "user:"
	maxText    = 4000
)

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

type bot struct {
	id, secret, robot, base string
	http                    *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newBot(cfg map[string]any) (*bot, error) {
	b := &bot{id: str(cfg, "client_id"), secret: str(cfg, "client_secret"), robot: str(cfg, "robot_code"),
		base: strings.TrimRight(str(cfg, "api_base"), "/"), http: &http.Client{Timeout: 20 * time.Second}}
	if b.id == "" || b.secret == "" {
		return nil, errors.New("缺少 Client ID 或 Client Secret")
	}
	if b.robot == "" {
		b.robot = b.id
	}
	if b.base == "" {
		b.base = defaultBase
	}
	return b, nil
}

// call posts JSON to the open platform and reads the answer into out. An
// answer that is not a success comes back as the platform's own message.
func (b *bot) call(ctx context.Context, path string, token string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("连不上钉钉开放平台：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var failed struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &failed)
		if failed.Message == "" {
			failed.Message = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("钉钉（%d %s）：%s", resp.StatusCode, failed.Code, failed.Message)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// accessToken gives the app's token, asked for again shortly before it runs
// out. Asking is also how the credentials are checked.
func (b *bot) accessToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	var out struct {
		Token  string `json:"accessToken"`
		Expire int    `json:"expireIn"`
	}
	err := b.call(ctx, "/v1.0/oauth2/accessToken", "", map[string]string{"appKey": b.id, "appSecret": b.secret}, &out)
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("钉钉没有给出访问凭证，请检查 Client ID 和 Client Secret")
	}
	b.token, b.expires = out.Token, time.Now().Add(time.Duration(out.Expire)*time.Second-5*time.Minute)
	return b.token, nil
}

// convert turns a message the bot received into an event.
func convert(m *chatbot.BotCallbackDataModel) cat.Event {
	private := m.ConversationType == "1"
	conv := &cat.Conversation{ID: m.ConversationId, Kind: "group", Name: m.ConversationTitle}
	who := m.SenderStaffId
	if who == "" { // someone outside the organisation has no id in it
		who = m.SenderId
	}
	if private {
		conv = &cat.Conversation{ID: userPrefix + who, Kind: "private"}
	}
	body := strings.TrimSpace(m.Text.Content)
	payload := map[string]any{
		"text": body, "message_id": m.MsgId, "message_type": m.Msgtype,
		// in a group the bot is only told of the messages that mention it
		"mentioned": private || m.IsInAtList,
	}
	if m.Msgtype != "text" && m.Content != nil {
		payload["content"] = m.Content
	}
	ev := cat.Event{
		Type: "message.created", Conversation: conv, Payload: payload, Raw: m,
		Actor: &cat.Actor{ID: who, Name: m.SenderNick},
	}
	if m.CreateAt > 0 {
		t := time.UnixMilli(m.CreateAt)
		ev.Time = &t
	}
	return ev
}

// told passes on what the stream client says of its connection: it has no
// other way to tell that the connection dropped or came back.
type told struct{ h *cat.Host }

func (told) Debugf(string, ...interface{}) {}
func (t told) Infof(format string, args ...interface{}) {
	if strings.Contains(format, "connect success") {
		t.h.SetStatus("running", "")
	}
}
func (t told) Warningf(format string, args ...interface{}) { t.h.Log("warn", format, args...) }
func (t told) Errorf(format string, args ...interface{}) {
	t.h.Log("warn", format, args...)
	if strings.Contains(format, "reconnect error") || strings.Contains(format, "read message error") {
		t.h.SetStatus("error", "与钉钉的连接断了，正在重连")
	}
}
func (t told) Fatalf(format string, args ...interface{}) { t.h.Log("error", format, args...) }

// split cuts s into pieces of at most n runes, preferring line breaks.
func split(s string, n int) []string {
	var out []string
	r := []rune(s)
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	return append(out, string(r))
}

// title is what a Markdown message is announced by: its first line, bare.
func title(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	line = strings.TrimSpace(strings.TrimLeft(line, "#>*-` "))
	if r := []rune(line); len(r) > 30 {
		line = string(r[:30]) + "…"
	}
	if line == "" {
		return "消息"
	}
	return line
}

// webhook reads the address a message can be answered at off the event the
// message came as, while it is still good.
func webhook(ev *cat.Event) string {
	raw, _ := ev.Raw.(map[string]any)
	at, _ := raw["sessionWebhook"].(string)
	if until, _ := raw["sessionWebhookExpiredTime"].(float64); until > 0 && time.Now().UnixMilli() > int64(until) {
		return ""
	}
	// only ever DingTalk's own: the address is taken from data
	if u, err := url.Parse(at); err != nil || u.Scheme != "https" || !(u.Hostname() == "dingtalk.com" || strings.HasSuffix(u.Hostname(), ".dingtalk.com")) {
		return ""
	}
	return at
}

// answer posts to the address a received message came with. It needs no
// permission, which sending through the open platform does.
func (b *bot) answer(ctx context.Context, at, body string, markdown bool) error {
	msg := map[string]any{"msgtype": "text", "text": map[string]string{"content": body}}
	if markdown {
		msg = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": title(body), "text": body}}
	}
	raw, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, "POST", at, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"errcode"`
		Msg  string `json:"errmsg"`
	}
	said, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(said, &out)
	if resp.StatusCode != 200 || out.Code != 0 {
		return fmt.Errorf("钉钉（%d）：%s", out.Code, out.Msg)
	}
	return nil
}

// push sends through the open platform, to a group by its conversation id or
// to a person by their id in the organisation.
func (b *bot) push(ctx context.Context, to, body string, markdown bool) error {
	token, err := b.accessToken(ctx)
	if err != nil {
		return err
	}
	key, param := "sampleText", map[string]string{"content": body}
	if markdown {
		key, param = "sampleMarkdown", map[string]string{"title": title(body), "text": body}
	}
	raw, _ := json.Marshal(param)
	msg := map[string]any{"robotCode": b.robot, "msgKey": key, "msgParam": string(raw)}
	path := "/v1.0/robot/groupMessages/send"
	if user, ok := strings.CutPrefix(to, userPrefix); ok {
		path, msg["userIds"] = "/v1.0/robot/oToMessages/batchSend", []string{user}
	} else {
		msg["openConversationId"] = to
	}
	return b.call(ctx, path, token, msg, nil)
}

// send delivers a message in pieces that fit: by the address the triggering
// message came with when there is one, else through the open platform.
func (b *bot) send(ctx context.Context, to, at, body string, markdown bool) (any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("消息内容为空")
	}
	if to == "" && at == "" {
		return nil, errors.New("缺少会话 ID")
	}
	n := 0
	for _, part := range split(body, maxText) {
		var err error
		if at != "" {
			if err = b.answer(ctx, at, part, markdown); err != nil && to != "" {
				err = b.push(ctx, to, part, markdown) // the address ran out: the long way round
			}
		} else {
			err = b.push(ctx, to, part, markdown)
		}
		if err != nil {
			return map[string]any{"sent": n}, err
		}
		n++
	}
	return map[string]any{"sent": n, "chat_id": to}, nil
}

func text(p map[string]any) string {
	switch v := p["text"].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

// markdown reads the "markdown" parameter, on unless switched off.
func markdown(p map[string]any) bool {
	on, set := p["markdown"].(bool)
	return !set || on
}

// New builds the plugin.
func New() cat.Plugin {
	var b *bot
	var stream *client.StreamClient
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			made, err := newBot(h.Config)
			if err != nil {
				return err
			}
			if _, err := made.accessToken(ctx); err != nil {
				return err
			}
			b = made
			logger.SetLogger(told{h})
			opts := []client.ClientOption{client.WithAppCredential(client.NewAppCredentialConfig(b.id, b.secret))}
			if b.base != defaultBase {
				opts = append(opts, client.WithOpenApiHost(b.base))
			}
			stream = client.NewStreamClient(opts...)
			stream.RegisterChatBotCallbackRouter(func(_ context.Context, m *chatbot.BotCallbackDataModel) ([]byte, error) {
				if _, err := h.Emit(convert(m)); err != nil {
					h.Log("error", "emit: %v", err)
				}
				return []byte(""), nil
			})
			if err := stream.Start(ctx); err != nil {
				return fmt.Errorf("连不上钉钉的 Stream 连接：%w。请确认机器人的消息接收模式选的是 Stream 模式，并且应用已经发布", err)
			}
			return nil
		},
		Stop: func() {
			if stream != nil {
				stream.AutoReconnect = false
				stream.Close()
			}
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「回复」需要由一条聊天消息触发；要主动发消息请用「发送到指定会话」")
				}
				return b.send(ctx, ev.Conversation.ID, webhook(ev), text(p), markdown(p))
			},
			"send": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				return b.send(ctx, str(p, "chat_id"), "", text(p), markdown(p))
			},
			"typing": func(context.Context, *cat.Host, map[string]any, cat.ActionContext) (any, error) {
				return map[string]any{}, nil
			},
		},
		Setup: setup,
	}
}

// setup asks for the app's credentials and checks them before the instance
// is saved.
func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	problem := ""
	for {
		desc := "1. 在钉钉开放平台「应用开发 → 企业内部应用」创建应用，添加「机器人」能力。\n" +
			"2. 机器人的「消息接收模式」选「Stream 模式」，然后发布机器人，并创建应用版本发布。\n" +
			"3. 要让机器人主动发消息（不只是回复），在「权限管理」里开通「企业内机器人发送消息」。\n" +
			"4. 把「凭证与基础信息」里的 Client ID 和 Client Secret 填到下面。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接钉钉机器人", Description: desc, URL: "https://open-dev.dingtalk.com/fe/app",
			Values: map[string]any{"client_id": cfg["client_id"]},
			Schema: map[string]any{
				"type": "object", "required": []string{"client_id", "client_secret"}, "x-order": []string{"client_id", "client_secret"},
				"properties": map[string]any{
					"client_id":     map[string]any{"type": "string", "title": "Client ID（AppKey）"},
					"client_secret": map[string]any{"type": "string", "title": "Client Secret（AppSecret）", "format": "password"},
				},
			},
		})
		if err != nil {
			return nil, err
		}
		cfg["client_id"], cfg["client_secret"] = out["client_id"], out["client_secret"]
		s.Progress("正在验证…", -1)
		b, err := newBot(cfg)
		if err == nil {
			_, err = b.accessToken(ctx)
		}
		if err != nil {
			problem = err.Error()
			continue
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接钉钉",
			Description: "在钉钉里搜到这个机器人给它发一条消息，或把它加进群后 @ 它，就能在「记录」页看到。群里只有 @ 它的消息才会收到。"})
		return cfg, nil
	}
}
