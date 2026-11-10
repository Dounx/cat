package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	cat "github.com/dounx/cat/sdk/go"
)

const maxText = 3000

// client speaks OneBot 11 over a forward WebSocket: events arrive on the
// socket, and API calls are sent on it and matched to replies by "echo".
type client struct {
	url, token string

	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan apiReply
	nextID  int
	selfID  string
}

type apiReply struct {
	Status  string          `json:"status"`
	RetCode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	Echo    string          `json:"echo"`
}

func newClient(url, token string) *client {
	return &client{url: url, token: token, pending: map[string]chan apiReply{}}
}

// connect dials the server. onEvent receives each event; the returned channel
// closes when the connection ends.
func (c *client) connect(ctx context.Context, onEvent func(raw json.RawMessage)) (<-chan struct{}, error) {
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, resp, err := websocket.Dial(dctx, c.url, &websocket.DialOptions{HTTPHeader: header})
	cancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			return nil, errors.New("访问令牌不正确")
		}
		return nil, fmt.Errorf("无法连接 OneBot 服务 %s：%w", c.url, err)
	}
	conn.SetReadLimit(16 << 20)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				c.mu.Lock()
				if c.conn == conn {
					c.conn = nil
				}
				for id, ch := range c.pending {
					close(ch)
					delete(c.pending, id)
				}
				c.mu.Unlock()
				return
			}
			var probe struct {
				Echo     *string `json:"echo"`
				PostType string  `json:"post_type"`
			}
			if json.Unmarshal(data, &probe) != nil {
				continue
			}
			if probe.Echo != nil && probe.PostType == "" {
				var r apiReply
				_ = json.Unmarshal(data, &r)
				c.mu.Lock()
				ch := c.pending[r.Echo]
				delete(c.pending, r.Echo)
				c.mu.Unlock()
				if ch != nil {
					ch <- r
				}
				continue
			}
			if onEvent != nil {
				onEvent(data)
			}
		}
	}()
	return done, nil
}

func (c *client) close() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

// call invokes a OneBot API and decodes its data into out.
func (c *client) call(ctx context.Context, action string, params, out any) error {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return errors.New("未连接到 OneBot 服务")
	}
	c.nextID++
	echo := strconv.Itoa(c.nextID)
	ch := make(chan apiReply, 1)
	c.pending[echo] = ch
	c.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"action": action, "params": params, "echo": echo})
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case r, ok := <-ch:
		if !ok {
			return errors.New("连接已断开")
		}
		if r.Status != "ok" && r.RetCode != 0 {
			msg := r.Wording
			if msg == "" {
				msg = r.Message
			}
			return fmt.Errorf("OneBot %s 失败（%d）：%s", action, r.RetCode, msg)
		}
		if out != nil && len(r.Data) > 0 {
			return json.Unmarshal(r.Data, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, echo)
		c.mu.Unlock()
		return errors.New("OneBot 服务没有响应 " + action)
	}
}

type loginInfo struct {
	UserID   json.Number `json:"user_id"`
	Nickname string      `json:"nickname"`
}

func (c *client) login(ctx context.Context) (loginInfo, error) {
	var info loginInfo
	err := c.call(ctx, "get_login_info", map[string]any{}, &info)
	if err == nil {
		c.mu.Lock()
		c.selfID = info.UserID.String()
		c.mu.Unlock()
	}
	return info, err
}

// ---- events ----

type segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

type obEvent struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	NoticeType  string          `json:"notice_type"`
	SubType     string          `json:"sub_type"`
	Time        int64           `json:"time"`
	SelfID      json.Number     `json:"self_id"`
	UserID      json.Number     `json:"user_id"`
	GroupID     json.Number     `json:"group_id"`
	MessageID   json.Number     `json:"message_id"`
	RawMessage  string          `json:"raw_message"`
	Message     json.RawMessage `json:"message"`
	Sender      struct {
		Nickname string `json:"nickname"`
		Card     string `json:"card"`
	} `json:"sender"`
}

func convID(ev obEvent) (id, kind string) {
	if ev.GroupID.String() != "" && ev.GroupID.String() != "0" {
		return "group:" + ev.GroupID.String(), "group"
	}
	return "private:" + ev.UserID.String(), "private"
}

// convert maps a OneBot event to Cat's envelope. ok is false for events that
// are not worth surfacing (heartbeats, the bot's own messages).
func convert(raw json.RawMessage) (cat.Event, bool) {
	var ev obEvent
	if json.Unmarshal(raw, &ev) != nil {
		return cat.Event{}, false
	}
	var rawAny any
	_ = json.Unmarshal(raw, &rawAny)
	out := cat.Event{Raw: rawAny}
	if ev.Time > 0 {
		t := time.Unix(ev.Time, 0)
		out.Time = &t
	}
	switch ev.PostType {
	case "message":
		if ev.UserID.String() == ev.SelfID.String() {
			return cat.Event{}, false
		}
		id, kind := convID(ev)
		text, mentioned, attachments := flatten(ev)
		name := ev.Sender.Card
		if name == "" {
			name = ev.Sender.Nickname
		}
		out.Type = "message.created"
		out.Conversation = &cat.Conversation{ID: id, Kind: kind}
		out.Actor = &cat.Actor{ID: ev.UserID.String(), Name: name}
		out.Payload = map[string]any{
			"text": text, "message_id": ev.MessageID.String(),
			"mentioned": mentioned || kind == "private",
		}
		if attachments != nil {
			out.Payload["attachments"] = attachments
		}
		return out, true
	case "notice":
		id, kind := convID(ev)
		out.Type = "notice"
		out.Conversation = &cat.Conversation{ID: id, Kind: kind}
		if ev.UserID.String() != "" {
			out.Actor = &cat.Actor{ID: ev.UserID.String()}
		}
		out.Payload = map[string]any{"notice_type": ev.NoticeType, "sub_type": ev.SubType}
		return out, true
	}
	return cat.Event{}, false
}

// flatten turns message segments into plain text, noting whether the bot was
// mentioned and collecting media.
func flatten(ev obEvent) (text string, mentioned bool, attachments []map[string]any) {
	var segs []segment
	if json.Unmarshal(ev.Message, &segs) != nil {
		return ev.RawMessage, strings.Contains(ev.RawMessage, "[CQ:at,qq="+ev.SelfID.String()), nil
	}
	var b strings.Builder
	for _, s := range segs {
		switch s.Type {
		case "text":
			b.WriteString(fmt.Sprint(s.Data["text"]))
		case "at":
			if qq := fmt.Sprint(s.Data["qq"]); qq == ev.SelfID.String() {
				mentioned = true
			} else {
				b.WriteString("@" + qq)
			}
		case "image", "record", "video", "file":
			kind := map[string]string{"image": "image", "record": "audio", "video": "video", "file": "file"}[s.Type]
			attachments = append(attachments, map[string]any{"kind": kind, "url": s.Data["url"], "name": s.Data["file"]})
		case "face":
			b.WriteString("[表情]")
		}
	}
	return strings.TrimSpace(b.String()), mentioned, attachments
}

// ---- sending ----

func parseConv(id string) (map[string]any, error) {
	kind, num, ok := strings.Cut(id, ":")
	n, err := strconv.ParseInt(num, 10, 64)
	if !ok || err != nil {
		return nil, fmt.Errorf("会话格式应为 group:群号 或 private:QQ号，收到 %q", id)
	}
	switch kind {
	case "group":
		return map[string]any{"message_type": "group", "group_id": n}, nil
	case "private":
		return map[string]any{"message_type": "private", "user_id": n}, nil
	}
	return nil, fmt.Errorf("未知的会话类型 %q", kind)
}

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

func (c *client) send(ctx context.Context, conv, text, replyTo string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("消息内容为空")
	}
	target, err := parseConv(conv)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i, part := range split(text, maxText) {
		msg := []segment{}
		if replyTo != "" && i == 0 {
			msg = append(msg, segment{Type: "reply", Data: map[string]any{"id": replyTo}})
		}
		msg = append(msg, segment{Type: "text", Data: map[string]any{"text": part}})
		params := map[string]any{"message": msg}
		for k, v := range target {
			params[k] = v
		}
		var sent struct {
			MessageID json.Number `json:"message_id"`
		}
		if err := c.call(ctx, "send_msg", params, &sent); err != nil {
			return map[string]any{"message_ids": ids}, err
		}
		ids = append(ids, sent.MessageID.String())
	}
	return map[string]any{"message_id": ids[len(ids)-1], "message_ids": ids, "conversation_id": conv}, nil
}
