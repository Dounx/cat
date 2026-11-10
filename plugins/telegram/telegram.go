package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultBase = "https://api.telegram.org"
	maxText     = 4096 // Telegram's per-message limit, in UTF-16 units; runes are a safe proxy
	pollSeconds = 25
)

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

type message struct {
	MessageID      int64            `json:"message_id"`
	ThreadID       int64            `json:"message_thread_id"`
	From           *user            `json:"from"`
	Chat           chat             `json:"chat"`
	Date           int64            `json:"date"`
	Text           string           `json:"text"`
	Caption        string           `json:"caption"`
	ReplyToMessage *message         `json:"reply_to_message"`
	Photo          []map[string]any `json:"photo"`
	Document       map[string]any   `json:"document"`
	Voice          map[string]any   `json:"voice"`
	Video          map[string]any   `json:"video"`
	Sticker        map[string]any   `json:"sticker"`
}

type update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *message `json:"message"`
	EditedMessage *message `json:"edited_message"`
	ChannelPost   *message `json:"channel_post"`
}

type apiError struct {
	Code        int
	Description string
}

func (e *apiError) Error() string { return fmt.Sprintf("Telegram %d: %s", e.Code, e.Description) }

type bot struct {
	token, base string
	client      *http.Client
	me          user
	// retry is how long to wait after a failed poll; shortened in tests.
	retry time.Duration
}

func newBot(cfg map[string]any) *bot {
	base, _ := cfg["api_base"].(string)
	if base == "" {
		base = defaultBase
	}
	token, _ := cfg["token"].(string)
	return &bot{
		token: strings.TrimSpace(token), base: strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: (pollSeconds + 15) * time.Second}, retry: 3 * time.Second,
	}
}

func (b *bot) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+"/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		// Never let the token (part of the URL) leak into logs or the UI.
		return errors.New(strings.ReplaceAll(err.Error(), b.token, "<token>"))
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("Telegram 返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if !env.OK {
		return &apiError{Code: env.ErrorCode, Description: env.Description}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (b *bot) getMe(ctx context.Context) error {
	if b.token == "" {
		return errors.New("缺少 Bot Token")
	}
	err := b.call(ctx, "getMe", map[string]any{}, &b.me)
	var apiErr *apiError
	if errors.As(err, &apiErr) && (apiErr.Code == 401 || apiErr.Code == 404) {
		return errors.New("Bot Token 无效")
	}
	return err
}

// New builds the plugin.
func New() cat.Plugin {
	var b *bot
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			b = newBot(h.Config)
			if err := b.getMe(ctx); err != nil {
				return err
			}
			h.Log("info", "已连接 @%s", b.me.Username)
			go b.poll(ctx, h)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「回复」需要由一条聊天消息触发；要主动发消息请用「发送到指定会话」")
				}
				replyTo := int64(0)
				if quote, ok := p["quote"].(bool); !ok || quote {
					replyTo = toInt(ev.Payload["message_id"])
				}
				return b.send(ctx, ev.Conversation.ID, text(p), replyTo, toInt(ev.Payload["thread_id"]), p["markdown"] == true)
			},
			"send": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				chatID := fmt.Sprint(p["chat_id"])
				if p["chat_id"] == nil || chatID == "" {
					return nil, errors.New("缺少会话 ID")
				}
				return b.send(ctx, chatID, text(p), 0, 0, p["markdown"] == true)
			},
			"typing": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				if actx.Event == nil || actx.Event.Conversation == nil {
					return nil, errors.New("「正在输入」需要由一条聊天消息触发")
				}
				err := b.call(ctx, "sendChatAction", map[string]any{"chat_id": actx.Event.Conversation.ID, "action": "typing"}, nil)
				return map[string]any{}, err
			},
		},
		Setup: setup,
	}
}

func text(p map[string]any) string {
	if p["text"] == nil {
		return ""
	}
	if s, ok := p["text"].(string); ok {
		return s
	}
	b, _ := json.Marshal(p["text"])
	return string(b)
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

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

func (b *bot) send(ctx context.Context, chatID, body string, replyTo, threadID int64, markdown bool) (any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("消息内容为空")
	}
	var ids []string
	for i, part := range split(body, maxText) {
		params := map[string]any{"chat_id": chatID, "text": part}
		if threadID != 0 {
			params["message_thread_id"] = threadID
		}
		if replyTo != 0 && i == 0 {
			params["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
		}
		var sent message
		var err error
		if markdown {
			params["parse_mode"] = "Markdown"
			err = b.call(ctx, "sendMessage", params, &sent)
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Code == 400 { // unbalanced markup: send as plain text
				delete(params, "parse_mode")
				err = b.call(ctx, "sendMessage", params, &sent)
			}
		} else {
			err = b.call(ctx, "sendMessage", params, &sent)
		}
		if err != nil {
			return map[string]any{"message_ids": ids}, err
		}
		ids = append(ids, strconv.FormatInt(sent.MessageID, 10))
	}
	return map[string]any{"message_id": ids[len(ids)-1], "message_ids": ids, "chat_id": chatID}, nil
}

func (b *bot) poll(ctx context.Context, h *cat.Host) {
	offset := int64(0)
	if v, ok := h.StateGet("offset"); ok {
		offset, _ = strconv.ParseInt(v, 10, 64)
	}
	healthy := true
	for ctx.Err() == nil {
		var updates []update
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": pollSeconds,
			"allowed_updates": []string{"message", "edited_message", "channel_post"},
		}, &updates)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if healthy {
				h.SetStatus("error", "与 Telegram 的连接中断，正在重试："+err.Error())
				healthy = false
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(b.retry):
			}
			continue
		}
		if !healthy {
			h.SetStatus("running", "")
			healthy = true
		}
		for _, u := range updates {
			if ev, ok := b.convert(u); ok {
				if _, err := h.Emit(ev); err != nil {
					h.Log("error", "emit: %v", err)
				}
			}
			offset = u.UpdateID + 1
		}
		if len(updates) > 0 {
			_ = h.StateSet("offset", strconv.FormatInt(offset, 10))
		}
	}
}

func (b *bot) convert(u update) (cat.Event, bool) {
	typ, m := "message.created", u.Message
	switch {
	case u.EditedMessage != nil:
		typ, m = "message.edited", u.EditedMessage
	case u.ChannelPost != nil:
		m = u.ChannelPost
	}
	if m == nil {
		return cat.Event{}, false
	}
	body := m.Text
	if body == "" {
		body = m.Caption
	}
	kind := map[string]string{"private": "private", "group": "group", "supergroup": "group", "channel": "channel"}[m.Chat.Type]
	name := m.Chat.Title
	if name == "" {
		name = m.Chat.Username
	}
	mention := "@" + b.me.Username
	mentioned := kind == "private" ||
		(b.me.Username != "" && strings.Contains(strings.ToLower(body), strings.ToLower(mention))) ||
		(m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && m.ReplyToMessage.From.ID == b.me.ID)
	payload := map[string]any{
		"text":       body,
		"message_id": strconv.FormatInt(m.MessageID, 10),
		"mentioned":  mentioned,
	}
	if m.ThreadID != 0 {
		payload["thread_id"] = strconv.FormatInt(m.ThreadID, 10)
	}
	if m.ReplyToMessage != nil {
		payload["reply_to_message_id"] = strconv.FormatInt(m.ReplyToMessage.MessageID, 10)
	}
	if strings.HasPrefix(body, "/") {
		cmd := strings.Fields(body)[0][1:]
		cmd, target, addressed := strings.Cut(cmd, "@")
		if !addressed || strings.EqualFold(target, b.me.Username) {
			payload["command"] = cmd
			payload["args"] = strings.TrimSpace(strings.TrimPrefix(body, strings.Fields(body)[0]))
		}
	}
	var attachments []map[string]any
	add := func(kind string, f map[string]any) {
		if f != nil {
			attachments = append(attachments, map[string]any{"kind": kind, "file_id": f["file_id"], "name": f["file_name"]})
		}
	}
	if len(m.Photo) > 0 {
		add("image", m.Photo[len(m.Photo)-1])
	}
	add("file", m.Document)
	add("audio", m.Voice)
	add("video", m.Video)
	add("sticker", m.Sticker)
	if attachments != nil {
		payload["attachments"] = attachments
	}
	ev := cat.Event{
		Type:         typ,
		Conversation: &cat.Conversation{ID: strconv.FormatInt(m.Chat.ID, 10), Kind: kind, Name: name},
		Payload:      payload,
		Raw:          u,
	}
	if m.Date > 0 {
		t := time.Unix(m.Date, 0)
		ev.Time = &t
	}
	if m.From != nil {
		ev.Actor = &cat.Actor{
			ID: strconv.FormatInt(m.From.ID, 10), IsBot: m.From.IsBot,
			Name: strings.TrimSpace(m.From.FirstName + " " + m.From.LastName),
		}
		payload["username"] = m.From.Username
	}
	return ev, true
}

func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	problem := ""
	for {
		desc := "1. 在 Telegram 里打开 @BotFather，发送 /newbot 并按提示创建。\n2. 把它给你的 Token 粘贴到下面。"
		if problem != "" {
			desc = "⚠ " + problem + "\n\n" + desc
		}
		out, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepForm, Title: "连接 Telegram Bot", Description: desc, URL: "https://t.me/BotFather",
			Schema: map[string]any{
				"type": "object", "required": []string{"token"},
				"properties": map[string]any{"token": map[string]any{"type": "string", "title": "Bot Token", "format": "password"}},
			},
		})
		if err != nil {
			return nil, err
		}
		cfg["token"] = out["token"]
		s.Progress("正在验证 Token…", -1)
		b := newBot(cfg)
		if err := b.getMe(ctx); err != nil {
			problem = err.Error()
			continue
		}
		s.Show(cat.Step{Kind: cat.StepInfo, Title: "已连接 @" + b.me.Username,
			Description: "给这个 Bot 发一条消息，就能在「事件」页看到。要在群里收到普通消息，需要在 BotFather 里用 /setprivacy 关闭隐私模式。"})
		return cfg, nil
	}
}
