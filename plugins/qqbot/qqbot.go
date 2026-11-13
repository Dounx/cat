package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	cat "github.com/dounx/cat/sdk/go"
)

const (
	tokenBase   = "https://bots.qq.com"
	apiBase     = "https://api.sgroup.qq.com"
	sandboxBase = "https://sandbox.api.sgroup.qq.com"

	// intents asks for group and one-to-one (C2C) events, the only ones a bot
	// gets in QQ itself (as opposed to QQ 频道).
	intents = 1 << 25
	maxText = 2000

	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
)

// loginRetry is how long to wait before logging in again after a refusal.
var loginRetry = 6 * time.Second

var errRefused = errors.New("QQ 网关拒绝了登录。请确认机器人没有被下架或封禁，还没上线的机器人要打开「沙箱环境」")

// apiError is a refusal from the open platform.
type apiError struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("QQ 开放平台（%d）：%s", e.Code, e.Message)
}

// bot talks to one QQ official bot: HTTP for sending, a gateway WebSocket for
// receiving.
type bot struct {
	appID, secret   string
	tokenURL, base  string
	http            *http.Client
	handshakeWithin time.Duration

	mu        sync.Mutex
	token     string
	expires   time.Time
	sessionID string // gateway session, kept to resume after a dropped connection
	seq       int64  // last dispatch sequence number seen
	msgSeq    int    // numbers our replies; QQ drops a reply that repeats one for the same msg_id
	name      string
}

func newBot(cfg map[string]any) *bot {
	b := &bot{
		appID: str(cfg, "app_id"), secret: str(cfg, "secret"),
		tokenURL: tokenBase, base: apiBase,
		http: &http.Client{Timeout: 30 * time.Second}, handshakeWithin: 30 * time.Second,
	}
	if cfg["sandbox"] == true {
		b.base = sandboxBase
	}
	if custom := strings.TrimRight(str(cfg, "api_base"), "/"); custom != "" {
		b.tokenURL, b.base = custom, custom
	}
	return b
}

// accessToken returns the app's access token, fetching a new one shortly
// before the current one runs out.
func (b *bot) accessToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	if b.appID == "" || b.secret == "" {
		return "", errors.New("缺少 AppID 或 AppSecret")
	}
	body, _ := json.Marshal(map[string]string{"appId": b.appID, "clientSecret": b.secret})
	req, err := http.NewRequestWithContext(ctx, "POST", b.tokenURL+"/app/getAppAccessToken", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接 QQ 开放平台：%w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"` // seconds, sent as a string
		Code        int    `json:"code"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("QQ 开放平台返回了无法解析的响应（HTTP %d）", resp.StatusCode)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("AppID 或 AppSecret 不正确（%d：%s）", out.Code, out.Message)
	}
	seconds, _ := strconv.Atoi(fmt.Sprint(out.ExpiresIn))
	// QQ only hands out a fresh token in the last minute of the old one, so
	// keep using this one until then.
	b.token, b.expires = out.AccessToken, time.Now().Add(time.Duration(seconds-50)*time.Second)
	return b.token, nil
}

func (b *bot) forgetToken() {
	b.mu.Lock()
	b.token = ""
	b.mu.Unlock()
}

// api calls the open platform and decodes the answer into out.
func (b *bot) api(ctx context.Context, method, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := b.accessToken(ctx)
		if err != nil {
			return err
		}
		var rd io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, b.base+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "QQBot "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := b.http.Do(req)
		if err != nil {
			return fmt.Errorf("无法连接 QQ 开放平台：%w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			apiErr := &apiError{Status: resp.StatusCode}
			if json.Unmarshal(data, apiErr) != nil || apiErr.Message == "" {
				apiErr.Message = "HTTP " + strconv.Itoa(resp.StatusCode)
			}
			if resp.StatusCode == 401 && attempt == 0 { // the token ran out early: get another
				b.forgetToken()
				continue
			}
			return apiErr
		}
		if out != nil && len(data) > 0 {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

// check makes sure the credentials work without logging in to the gateway,
// and learns the bot's name when QQ will tell.
func (b *bot) check(ctx context.Context) error {
	if err := b.api(ctx, "GET", "/gateway", nil, nil); err != nil {
		return err
	}
	var me struct {
		Username string `json:"username"`
	}
	if b.api(ctx, "GET", "/users/@me", nil, &me) == nil {
		b.mu.Lock()
		b.name = me.Username
		b.mu.Unlock()
	}
	return nil
}

// ---- gateway ----

type frame struct {
	Op int             `json:"op"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
	ID string          `json:"id"`
	D  json.RawMessage `json:"d"`
}

func write(ctx context.Context, conn *websocket.Conn, op int, d any) error {
	data, _ := json.Marshal(map[string]any{"op": op, "d": d})
	return conn.Write(ctx, websocket.MessageText, data)
}

// dial connects to the gateway and logs in, resuming the previous session
// when there is one so that events sent while disconnected arrive after all;
// those are handed to emit. It returns the connection once QQ has accepted
// it, and how often it wants a heartbeat.
func (b *bot) dial(ctx context.Context, emit func(cat.Event)) (*websocket.Conn, time.Duration, error) {
	// QQ turns away a login that follows another too closely (a restart, say),
	// so a refusal is worth a couple of patient retries before giving up.
	for attempt := 0; ; attempt++ {
		conn, interval, err := b.dialOnce(ctx, emit)
		if !errors.Is(err, errRefused) || attempt == 2 {
			return conn, interval, err
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(loginRetry):
		}
	}
}

func (b *bot) dialOnce(ctx context.Context, emit func(cat.Event)) (*websocket.Conn, time.Duration, error) {
	var gw struct {
		URL string `json:"url"`
	}
	if err := b.api(ctx, "GET", "/gateway", nil, &gw); err != nil {
		return nil, 0, err
	}
	token, err := b.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	hctx, cancel := context.WithTimeout(ctx, b.handshakeWithin)
	defer cancel()
	conn, _, err := websocket.Dial(hctx, gw.URL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接 QQ 网关：%w", err)
	}
	conn.SetReadLimit(16 << 20)
	b.mu.Lock()
	session, seq := b.sessionID, b.seq
	b.mu.Unlock()
	var interval time.Duration
	for {
		_, data, err := conn.Read(hctx)
		if err != nil {
			conn.CloseNow()
			return nil, 0, fmt.Errorf("QQ 网关断开了连接：%w", err)
		}
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Op {
		case opHello:
			var hello struct {
				Interval int `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(f.D, &hello)
			interval = time.Duration(hello.Interval) * time.Millisecond
			if session != "" {
				err = write(hctx, conn, opResume, map[string]any{"token": "QQBot " + token, "session_id": session, "seq": seq})
			} else {
				err = write(hctx, conn, opIdentify, map[string]any{
					"token": "QQBot " + token, "intents": intents, "shard": []int{0, 1},
					"properties": map[string]string{"$os": "cat", "$browser": "cat", "$device": "cat"},
				})
			}
			if err != nil {
				conn.CloseNow()
				return nil, 0, err
			}
		case opInvalidSession, opReconnect:
			conn.CloseNow()
			if session != "" { // the old session is gone: log in afresh
				b.mu.Lock()
				b.sessionID, b.seq = "", 0
				b.mu.Unlock()
				return b.dialOnce(ctx, emit)
			}
			return nil, 0, errRefused
		case opDispatch:
			if b.dispatch(f, emit) {
				if interval <= 0 {
					interval = 40 * time.Second
				}
				return conn, interval, nil
			}
		}
	}
}

// dispatch handles one event from the gateway and reports whether it was the
// one that says the login is complete.
func (b *bot) dispatch(f frame, emit func(cat.Event)) (ready bool) {
	b.mu.Lock()
	if f.S > 0 {
		b.seq = f.S
	}
	b.mu.Unlock()
	switch f.T {
	case "READY":
		var d struct {
			SessionID string `json:"session_id"`
			User      struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		_ = json.Unmarshal(f.D, &d)
		b.mu.Lock()
		b.sessionID, b.name = d.SessionID, d.User.Username
		b.mu.Unlock()
		return true
	case "RESUMED":
		return true
	}
	if ev, ok := convert(f); ok && emit != nil {
		emit(ev)
	}
	return false
}

// serve keeps a logged-in connection alive and passes on its events until it
// ends.
func (b *bot) serve(ctx context.Context, conn *websocket.Conn, interval time.Duration, emit func(cat.Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				b.mu.Lock()
				seq := b.seq
				b.mu.Unlock()
				if write(ctx, conn, opHeartbeat, seq) != nil {
					conn.CloseNow()
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Op {
		case opDispatch:
			b.dispatch(f, emit)
		case opReconnect:
			return errors.New("QQ 网关要求重新连接")
		case opInvalidSession:
			b.mu.Lock()
			b.sessionID, b.seq = "", 0
			b.mu.Unlock()
			return errors.New("QQ 网关的会话已失效")
		}
	}
}

// run holds the gateway connection for as long as the instance runs,
// reconnecting when it drops.
func (b *bot) run(ctx context.Context, h *cat.Host, conn *websocket.Conn, interval time.Duration, emit func(cat.Event)) {
	backoff := time.Second
	for {
		err := b.serve(ctx, conn, interval, emit)
		if ctx.Err() != nil {
			return
		}
		h.SetStatus("error", "与 QQ 的连接中断，正在重连："+err.Error())
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			if conn, interval, err = b.dial(ctx, emit); err == nil {
				h.SetStatus("running", "")
				backoff = time.Second
				break
			}
			h.SetStatus("error", "与 QQ 的连接中断，正在重连："+err.Error())
		}
	}
}

// ---- events ----

type attachment struct {
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
	URL         string `json:"url"`
}

type qqMessage struct {
	ID          string       `json:"id"`
	Content     string       `json:"content"`
	Timestamp   any          `json:"timestamp"` // RFC 3339 on messages, Unix seconds on notices
	GroupOpenID string       `json:"group_openid"`
	Attachments []attachment `json:"attachments"`
	Author      struct {
		UserOpenID   string `json:"user_openid"`
		MemberOpenID string `json:"member_openid"`
	} `json:"author"`
	// Notices (the bot was added to a group, befriended, …) carry these.
	OpenID         string `json:"openid"`
	OpMemberOpenID string `json:"op_member_openid"`
}

// convert maps a gateway event to Cat's envelope. ok is false for events not
// worth surfacing.
func convert(f frame) (cat.Event, bool) {
	var m qqMessage
	if json.Unmarshal(f.D, &m) != nil {
		return cat.Event{}, false
	}
	var raw any
	_ = json.Unmarshal(f.D, &raw)
	out := cat.Event{Raw: map[string]any{"t": f.T, "id": f.ID, "d": raw}}
	switch ts := m.Timestamp.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			out.Time = &t
		}
	case float64:
		t := time.Unix(int64(ts), 0)
		out.Time = &t
	}
	switch f.T {
	case "C2C_MESSAGE_CREATE":
		out.Conversation = &cat.Conversation{ID: "user:" + m.Author.UserOpenID, Kind: "private"}
		out.Actor = &cat.Actor{ID: m.Author.UserOpenID}
	case "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE":
		out.Conversation = &cat.Conversation{ID: "group:" + m.GroupOpenID, Kind: "group"}
		out.Actor = &cat.Actor{ID: m.Author.MemberOpenID}
	case "FRIEND_ADD", "FRIEND_DEL", "C2C_MSG_REJECT", "C2C_MSG_RECEIVE",
		"GROUP_ADD_ROBOT", "GROUP_DEL_ROBOT", "GROUP_MSG_REJECT", "GROUP_MSG_RECEIVE":
		out.Type = "notice"
		out.Payload = map[string]any{"notice_type": strings.ToLower(f.T)}
		if m.GroupOpenID != "" {
			out.Conversation = &cat.Conversation{ID: "group:" + m.GroupOpenID, Kind: "group"}
			if m.OpMemberOpenID != "" {
				out.Actor = &cat.Actor{ID: m.OpMemberOpenID}
			}
		} else if m.OpenID != "" {
			out.Conversation = &cat.Conversation{ID: "user:" + m.OpenID, Kind: "private"}
			out.Actor = &cat.Actor{ID: m.OpenID}
		}
		return out, true
	default:
		return cat.Event{}, false
	}
	text := strings.TrimSpace(m.Content)
	out.Type = "message.created"
	out.Payload = map[string]any{
		"text": text, "message_id": m.ID,
		// A bot only sees every group message with a special permission;
		// otherwise all it gets are the ones that @ it.
		"mentioned": f.T != "GROUP_MESSAGE_CREATE",
	}
	if strings.HasPrefix(text, "/") && len(text) > 1 {
		first := strings.Fields(text)[0]
		out.Payload["command"] = first[1:]
		out.Payload["args"] = strings.TrimSpace(strings.TrimPrefix(text, first))
	}
	var attachments []map[string]any
	for _, a := range m.Attachments {
		kind := "file"
		for _, k := range []string{"image", "video"} {
			if strings.HasPrefix(a.ContentType, k) {
				kind = k
			}
		}
		if strings.HasPrefix(a.ContentType, "audio") || a.ContentType == "voice" {
			kind = "audio"
		}
		attachments = append(attachments, map[string]any{"kind": kind, "url": a.URL, "name": a.Filename})
	}
	if attachments != nil {
		out.Payload["attachments"] = attachments
	}
	return out, true
}

// ---- sending ----

// messagesPath gives the endpoint that sends to a conversation id.
func messagesPath(conv string) (string, error) {
	kind, openid, ok := strings.Cut(conv, ":")
	if ok && openid != "" && !strings.ContainsAny(openid, "/?# ") {
		switch kind {
		case "group":
			return "/v2/groups/" + openid + "/messages", nil
		case "user":
			return "/v2/users/" + openid + "/messages", nil
		}
	}
	return "", fmt.Errorf("会话格式应为 group:群的 openid 或 user:用户的 openid，收到 %q", conv)
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

// post sends one message body to a conversation. replyTo is the id of the
// message it answers; without one QQ counts it as a proactive message.
func (b *bot) post(ctx context.Context, path string, body map[string]any, replyTo string) (string, error) {
	if replyTo != "" {
		b.mu.Lock()
		b.msgSeq++
		seq := b.msgSeq
		b.mu.Unlock()
		body["msg_id"], body["msg_seq"] = replyTo, seq
	}
	var sent struct {
		ID string `json:"id"`
	}
	err := b.api(ctx, "POST", path, body, &sent)
	return sent.ID, err
}

func (b *bot) send(ctx context.Context, conv, text, replyTo string, markdown bool) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("消息内容为空")
	}
	path, err := messagesPath(conv)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, part := range split(text, maxText) {
		var id string
		err := errors.New("")
		if markdown {
			id, err = b.post(ctx, path, map[string]any{"msg_type": 2, "markdown": map[string]string{"content": part}}, replyTo)
		}
		var refused *apiError
		if !markdown || errors.As(err, &refused) { // no Markdown permission, or markup QQ rejects: send as plain text
			id, err = b.post(ctx, path, map[string]any{"msg_type": 0, "content": part}, replyTo)
		}
		if err != nil {
			return map[string]any{"message_ids": ids}, err
		}
		ids = append(ids, id)
	}
	return map[string]any{"message_id": ids[len(ids)-1], "message_ids": ids, "conversation_id": conv}, nil
}
