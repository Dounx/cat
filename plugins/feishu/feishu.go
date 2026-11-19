package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/channel"
	"github.com/larksuite/oapi-sdk-go/v3/channel/normalize"
	"github.com/larksuite/oapi-sdk-go/v3/channel/types"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	cat "github.com/dounx/cat/sdk/go"
)

// The open platform's address for each edition.
var bases = map[string]string{"feishu": lark.FeishuBaseUrl, "lark": lark.LarkBaseUrl}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// quiet keeps the SDK's own log out of the way: only what went wrong is
// passed on, to Cat's log.
type quiet struct{ h *cat.Host }

func (quiet) Debug(context.Context, ...interface{}) {}
func (quiet) Info(context.Context, ...interface{})  {}
func (q quiet) Warn(_ context.Context, args ...interface{}) {
	if q.h != nil {
		q.h.Log("warn", "%s", fmt.Sprint(args...))
	}
}
func (q quiet) Error(_ context.Context, args ...interface{}) {
	if q.h != nil {
		q.h.Log("error", "%s", fmt.Sprint(args...))
	}
}

type bot struct {
	appID, secret, base string
	client              *lark.Client
	// out sends: it splits long text, turns Markdown into a rich-text post
	// and tries again on a blip.
	out types.Channel
	// openID and name are the bot's own, to tell when it is mentioned and to
	// leave its own messages alone.
	openID, name string

	mu              sync.Mutex
	chats           map[string]string // chat id → name, "" when it cannot be read
	typingReactions map[string]string // trigger message_id → reaction_id
}

func newBot(cfg map[string]any, h *cat.Host) (*bot, error) {
	b := &bot{
		appID:           str(cfg, "app_id"),
		secret:          str(cfg, "app_secret"),
		chats:           map[string]string{},
		typingReactions: map[string]string{},
	}
	if b.appID == "" || b.secret == "" {
		return nil, errors.New("缺少 App ID 或 App Secret")
	}
	domain := str(cfg, "domain")
	if domain == "" {
		domain = "feishu"
	}
	b.base = bases[domain]
	if b.base == "" { // an address of one's own: a proxy, or a test
		b.base = strings.TrimRight(domain, "/")
	}
	b.client = lark.NewClient(b.appID, b.secret, lark.WithOpenBaseUrl(b.base), lark.WithLogger(quiet{h}),
		lark.WithLogLevel(larkcore.LogLevelWarn), lark.WithReqTimeout(30*time.Second))
	b.out = channel.NewChannel(b.client, nil)
	return b, nil
}

// identify asks who the bot is, which also checks the credentials.
func (b *bot) identify(ctx context.Context) error {
	resp, err := b.client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return fmt.Errorf("连不上飞书开放平台，或 App ID / App Secret 不对：%w", err)
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &out); err != nil {
		return fmt.Errorf("飞书返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if out.Code != 0 {
		return fmt.Errorf("飞书（%d）：%s。如果应用还没有开启「机器人」能力，先到开放平台给它加上并发布", out.Code, out.Msg)
	}
	b.openID, b.name = out.Bot.OpenID, out.Bot.AppName
	return nil
}

// chatName gives what a chat is called, asked once. Without the permission to
// read it the name stays empty, and the chat goes by its id.
func (b *bot) chatName(ctx context.Context, chatID string) string {
	b.mu.Lock()
	name, known := b.chats[chatID]
	b.mu.Unlock()
	if known {
		return name
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := b.client.Im.V1.Chat.Get(ctx, larkim.NewGetChatReqBuilder().ChatId(chatID).Build())
	if err == nil && resp.Success() && resp.Data != nil && resp.Data.Name != nil {
		name = *resp.Data.Name
	}
	b.mu.Lock()
	b.chats[chatID] = name
	b.mu.Unlock()
	return name
}

// convert turns a received message into an event. ok is false for the bot's
// own messages.
func (b *bot) convert(ctx context.Context, h *cat.Host, in *larkim.P2MessageReceiveV1) (cat.Event, bool) {
	m := normalize.ParseMessage(in)
	if m == nil || (b.openID != "" && m.UserID == b.openID) {
		return cat.Event{}, false
	}
	// A mention stands in the text as a key (@_user_1): the bot's own is
	// taken out, the others are put back by name.
	body, mentioned := m.Content, m.ChatType == "p2p"
	var names []string
	for _, at := range m.Mentions {
		if at.OpenID != "" && at.OpenID == b.openID {
			mentioned = true
			body = strings.ReplaceAll(body, at.Key, "")
			continue
		}
		body = strings.ReplaceAll(body, at.Key, "@"+at.Name)
		names = append(names, at.Name)
	}
	body = strings.TrimSpace(body)
	payload := map[string]any{
		"text": body, "message_id": m.MessageID, "mentioned": mentioned, "message_type": m.RawContentType,
	}
	if m.MentionAll {
		payload["mention_all"] = true
	}
	if len(names) > 0 {
		payload["mentions"] = names
	}
	if msg := in.Event.Message; msg.ParentId != nil && *msg.ParentId != "" {
		payload["reply_to_message_id"] = *msg.ParentId
	}
	var attachments []map[string]any
	for _, r := range m.Resources {
		if h != nil && m.MessageID != "" && r.FileKey != "" {
			resType := "file"
			if r.Type == "image" {
				resType = "image"
			}
			dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			resp, err := b.client.Im.V1.MessageResource.Get(dctx, larkim.NewGetMessageResourceReqBuilder().
				MessageId(m.MessageID).FileKey(r.FileKey).Type(resType).Build())
			cancel()
			if err == nil && resp.Success() && resp.File != nil {
				name := r.FileName
				if name == "" {
					name = resp.FileName
				}
				if att, serr := h.SaveBlob(name, "", resp.File, 10<<20); serr == nil {
					item := att.Map()
					item["file_key"] = r.FileKey
					attachments = append(attachments, item)
					continue
				}
			}
		}
		attachments = append(attachments, map[string]any{"kind": r.Type, "file_key": r.FileKey, "name": r.FileName})
	}
	if attachments != nil {
		payload["attachments"] = attachments
	}
	conv := &cat.Conversation{ID: m.ChatID, Kind: "group"}
	if m.ChatType == "p2p" {
		conv.Kind = "private"
	} else {
		conv.Name = b.chatName(ctx, m.ChatID)
	}
	ev := cat.Event{
		Type: "message.created", Conversation: conv, Payload: payload, Raw: in.Event,
		Actor: &cat.Actor{ID: m.UserID},
	}
	if sender := in.Event.Sender; sender != nil && sender.SenderType != nil && *sender.SenderType != "user" {
		ev.Actor.IsBot = true
	}
	if m.CreateTimeMs > 0 {
		t := time.UnixMilli(m.CreateTimeMs)
		ev.Time = &t
	}
	return ev, true
}

// listen keeps the long connection up and emits what comes over it.
func (b *bot) listen(ctx context.Context, h *cat.Host) {
	handler := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(func(ctx context.Context, in *larkim.P2MessageReceiveV1) error {
		if ev, ok := b.convert(ctx, h, in); ok {
			if _, err := h.Emit(ev); err != nil {
				h.Log("error", "emit: %v", err)
			}
		}
		return nil
	})
	ws := larkws.NewClient(b.appID, b.secret, larkws.WithEventHandler(handler), larkws.WithDomain(b.base),
		larkws.WithLogger(quiet{h}), larkws.WithLogLevel(larkcore.LogLevelWarn))
	ws.SetOnReady(func() { h.SetStatus("running", "") })
	ws.SetOnReconnected(func() { h.SetStatus("running", "") })
	ws.SetOnReconnecting(func() { h.SetStatus("error", "与飞书的长连接断了，正在重连") })
	ws.SetOnError(func(err error) { h.SetStatus("error", "与飞书的长连接出错，正在重试："+err.Error()) })
	for ctx.Err() == nil {
		if err := ws.Start(ctx); err != nil && ctx.Err() == nil {
			h.SetStatus("error", "连不上飞书的长连接："+err.Error()+"。请确认应用在开放平台的「事件与回调」里选了「使用长连接接收事件」")
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
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

func (b *bot) sendWithFiles(ctx context.Context, to, body string, files []string, replyTo string, markdown bool) (any, error) {
	if strings.TrimSpace(body) == "" && len(files) == 0 {
		return nil, errors.New("消息内容为空")
	}
	var ids []string
	lastID := ""
	if strings.TrimSpace(body) != "" {
		res, err := b.send(ctx, to, body, replyTo, markdown)
		if err != nil {
			return res, err
		}
		if m, ok := res.(map[string]any); ok {
			lastID, _ = m["message_id"].(string)
			if list, ok := m["message_ids"].([]string); ok {
				ids = append(ids, list...)
			}
		}
		replyTo = ""
	}
	for i, fp := range files {
		info, err := os.Stat(fp)
		if err != nil || info.IsDir() {
			return map[string]any{"message_ids": ids}, fmt.Errorf("找不到附件文件：%s", fp)
		}
		in := &types.SendInput{ReceiveID: to}
		if i == 0 {
			in.ReplyMessageID = replyTo
		}
		switch strings.ToLower(filepath.Ext(fp)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
			in.ImagePath = fp
		default:
			in.FilePath = fp
		}
		res, err := b.out.Send(ctx, in)
		if err != nil {
			return map[string]any{"message_ids": ids}, fmt.Errorf("飞书发送附件失败：%w", err)
		}
		lastID = res.MessageID
		if len(res.ChunkIDs) > 0 {
			ids = append(ids, res.ChunkIDs...)
		} else if res.MessageID != "" {
			ids = append(ids, res.MessageID)
		}
	}
	return map[string]any{"message_id": lastID, "message_ids": ids, "chat_id": to}, nil
}

// send posts a message to a chat or a user, as a reply when replyTo is set.
func (b *bot) send(ctx context.Context, to, body, replyTo string, markdown bool) (any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("消息内容为空")
	}
	in := &types.SendInput{ReceiveID: to, ReplyMessageID: replyTo}
	if markdown {
		in.Markdown = body
	} else {
		in.Text = body
	}
	res, err := b.out.Send(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("飞书没有发出去：%w", err)
	}
	ids := res.ChunkIDs
	if len(ids) == 0 {
		ids = []string{res.MessageID}
	}
	return map[string]any{"message_id": res.MessageID, "message_ids": ids, "chat_id": to}, nil
}

func (b *bot) edit(ctx context.Context, to, messageID, body string, md bool) (any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("消息内容为空")
	}
	msgType := "text"
	var content string
	if md {
		postJSON, err := normalize.SimpleMarkdownToPost("", body, nil)
		if err == nil {
			msgType = "post"
			content = postJSON
		}
	}
	if content == "" {
		raw, _ := json.Marshal(map[string]string{"text": body})
		content = string(raw)
	}
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().MsgType(msgType).Content(content).Build()).
		Build()
	resp, err := b.client.Im.V1.Message.Update(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("飞书更新消息失败：%w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书更新消息失败（%d）：%s", resp.Code, resp.Msg)
	}
	return map[string]any{"message_id": messageID, "message_ids": []string{messageID}, "chat_id": to}, nil
}

func (b *bot) startTyping(ctx context.Context, msgID string) error {
	if msgID == "" {
		return nil
	}
	b.mu.Lock()
	if _, exists := b.typingReactions[msgID]; exists {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(msgID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType("Typing").Build()).
			Build()).
		Build()
	resp, err := b.client.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("飞书添加正在输入表情失败（%d）：%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.ReactionId != nil && *resp.Data.ReactionId != "" {
		b.mu.Lock()
		b.typingReactions[msgID] = *resp.Data.ReactionId
		b.mu.Unlock()
	}
	return nil
}

func (b *bot) clearTyping(ctx context.Context, msgID string) {
	if msgID == "" {
		return
	}
	b.mu.Lock()
	reactionID, ok := b.typingReactions[msgID]
	if ok {
		delete(b.typingReactions, msgID)
	}
	b.mu.Unlock()
	if !ok || reactionID == "" {
		return
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(msgID).
		ReactionId(reactionID).
		Build()
	_, _ = b.client.Im.V1.MessageReaction.Delete(ctx, req)
}

// markdown reads the "markdown" parameter, on unless switched off.
func markdown(p map[string]any) bool {
	on, set := p["markdown"].(bool)
	return !set || on
}

// New builds the plugin.
func New() cat.Plugin {
	var b *bot
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			made, err := newBot(h.Config, h)
			if err != nil {
				return err
			}
			if err := made.identify(ctx); err != nil {
				return err
			}
			b = made
			h.Log("info", "已连接飞书机器人「%s」", b.name)
			go b.listen(ctx, h)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				ev := actx.Event
				if ev == nil || ev.Conversation == nil {
					return nil, errors.New("「回复」需要由一条聊天消息触发；要主动发消息请用「发送到指定会话」")
				}
				triggerMsgID, _ := ev.Payload["message_id"].(string)
				b.clearTyping(ctx, triggerMsgID)
				if editID := str(p, "edit_message_id"); editID != "" {
					return b.edit(ctx, ev.Conversation.ID, editID, text(p), markdown(p))
				}
				replyTo := ""
				if quote, set := p["quote"].(bool); !set || quote {
					replyTo = triggerMsgID
				}
				return b.sendWithFiles(ctx, ev.Conversation.ID, text(p), cat.Files(p), replyTo, markdown(p))
			},
			"send": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				to := str(p, "chat_id")
				if to == "" {
					return nil, errors.New("缺少会话 ID")
				}
				if editID := str(p, "edit_message_id"); editID != "" {
					return b.edit(ctx, to, editID, text(p), markdown(p))
				}
				return b.sendWithFiles(ctx, to, text(p), cat.Files(p), "", markdown(p))
			},
			"typing": func(ctx context.Context, h *cat.Host, _ map[string]any, actx cat.ActionContext) (any, error) {
				if actx.Event != nil {
					msgID, _ := actx.Event.Payload["message_id"].(string)
					if err := b.startTyping(ctx, msgID); err != nil {
						h.Log("debug", "feishu typing reaction: %v", err)
					}
				}
				return map[string]any{}, nil
			},
		},
		Setup: setup,
	}
}
