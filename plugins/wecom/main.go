// Command cat-plugin-wecom receives WeCom (企业微信) app callbacks and sends
// messages through the app.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

const maxBytes = 2000 // WeCom limits text content to 2048 bytes

type app struct {
	codec           *codec
	corpID, agentID string
	secret, base    string
	http            *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	seen    map[string]time.Time // MsgId → first seen, to drop WeCom's retries
}

func str(m map[string]any, k string) string {
	if m[k] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[k]))
}

func q(req cat.IngressRequest, key string) string {
	if v := req.Query[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (a *app) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Before(a.expires) {
		return a.token, nil
	}
	u := a.base + "/cgi-bin/gettoken?corpid=" + url.QueryEscape(a.corpID) + "&corpsecret=" + url.QueryEscape(a.secret)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", errors.New("无法连接企业微信：" + strings.ReplaceAll(err.Error(), a.secret, "<secret>"))
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ErrCode != 0 {
		return "", fmt.Errorf("企业微信拒绝了凭据（%d）：%s", out.ErrCode, out.ErrMsg)
	}
	a.token = out.AccessToken
	a.expires = time.Now().Add(time.Duration(out.ExpiresIn-300) * time.Second)
	return a.token, nil
}

func splitBytes(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && (s[cut]&0xC0) == 0x80 { // do not cut inside a UTF-8 sequence
			cut--
		}
		if i := strings.LastIndexByte(s[:cut], '\n'); i > n/2 {
			cut = i + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

func (a *app) send(ctx context.Context, toUser, text string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("消息内容为空")
	}
	if toUser == "" {
		return nil, errors.New("缺少接收成员")
	}
	agent, err := strconv.Atoi(a.agentID)
	if err != nil {
		return nil, errors.New("AgentId 应为数字")
	}
	var ids []string
	for _, part := range splitBytes(text, maxBytes) {
		token, err := a.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		body, _ := json.Marshal(map[string]any{"touser": toUser, "msgtype": "text", "agentid": agent, "text": map[string]string{"content": part}})
		req, _ := http.NewRequestWithContext(ctx, "POST", a.base+"/cgi-bin/message/send?access_token="+url.QueryEscape(token), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.http.Do(req)
		if err != nil {
			return nil, errors.New("发送失败：无法连接企业微信")
		}
		var out struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
			MsgID   string `json:"msgid"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if out.ErrCode == 40014 || out.ErrCode == 42001 { // token expired early: forget it
			a.mu.Lock()
			a.token = ""
			a.mu.Unlock()
		}
		if out.ErrCode != 0 {
			return map[string]any{"msgids": ids}, fmt.Errorf("企业微信发送失败（%d）：%s", out.ErrCode, out.ErrMsg)
		}
		ids = append(ids, out.MsgID)
	}
	return map[string]any{"msgid": ids[len(ids)-1], "msgids": ids}, nil
}

type envelope struct {
	Encrypt string `xml:"Encrypt"`
}

type inbound struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime   int64  `xml:"CreateTime"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MsgID        string `xml:"MsgId"`
	AgentID      string `xml:"AgentID"`
	Event        string `xml:"Event"`
	EventKey     string `xml:"EventKey"`
	PicURL       string `xml:"PicUrl"`
	MediaID      string `xml:"MediaId"`
}

func (a *app) duplicate(id string) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, t := range a.seen {
		if now.Sub(t) > 5*time.Minute {
			delete(a.seen, k)
		}
	}
	if _, ok := a.seen[id]; ok {
		return true
	}
	a.seen[id] = now
	return false
}

func (a *app) ingress(h *cat.Host, req cat.IngressRequest) (cat.IngressResponse, error) {
	sig, ts, nonce := q(req, "msg_signature"), q(req, "timestamp"), q(req, "nonce")
	deny := cat.IngressResponse{Status: 403, Body: []byte("signature mismatch")}
	if req.Method == "GET" { // URL verification when the callback is saved in the admin console
		echo := q(req, "echostr")
		if !a.codec.verify(sig, ts, nonce, echo) {
			return deny, nil
		}
		plain, err := a.codec.decrypt(echo)
		if err != nil {
			return cat.IngressResponse{Status: 400, Body: []byte(err.Error())}, nil
		}
		return cat.IngressResponse{Status: 200, Body: plain}, nil
	}
	var env envelope
	if err := xml.Unmarshal(req.Body, &env); err != nil || env.Encrypt == "" {
		return cat.IngressResponse{Status: 400, Body: []byte("bad body")}, nil
	}
	if !a.codec.verify(sig, ts, nonce, env.Encrypt) {
		return deny, nil
	}
	plain, err := a.codec.decrypt(env.Encrypt)
	if err != nil {
		return cat.IngressResponse{Status: 400, Body: []byte(err.Error())}, nil
	}
	var m inbound
	if err := xml.Unmarshal(plain, &m); err != nil {
		return cat.IngressResponse{Status: 400, Body: []byte("bad message")}, nil
	}
	ok := cat.IngressResponse{Status: 200, Body: []byte("success")}
	if a.duplicate(m.MsgID) {
		return ok, nil
	}
	ev := cat.Event{
		Conversation: &cat.Conversation{ID: m.FromUserName, Kind: "private"},
		Actor:        &cat.Actor{ID: m.FromUserName, Name: m.FromUserName},
		Raw:          m,
	}
	if m.CreateTime > 0 {
		t := time.Unix(m.CreateTime, 0)
		ev.Time = &t
	}
	if m.MsgType == "event" {
		ev.Type = "wecom.event"
		ev.Payload = map[string]any{"event": m.Event, "event_key": m.EventKey}
	} else {
		ev.Type = "message.created"
		ev.Payload = map[string]any{"text": m.Content, "message_id": m.MsgID, "mentioned": true, "msg_type": m.MsgType}
		if m.MediaID != "" {
			kind := map[string]string{"image": "image", "voice": "audio", "video": "video"}[m.MsgType]
			if kind == "" {
				kind = "file"
			}
			ev.Payload["attachments"] = []map[string]any{{"kind": kind, "media_id": m.MediaID, "url": m.PicURL}}
		}
	}
	// WeCom retries unless answered within five seconds, so acknowledge now
	// and let the rules run on their own time.
	if _, err := h.Emit(ev); err != nil {
		return cat.IngressResponse{Status: 500}, err
	}
	return ok, nil
}

func New() cat.Plugin {
	var a *app
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			cfg := h.Config
			for _, k := range []string{"corp_id", "agent_id", "secret", "token", "aes_key"} {
				if str(cfg, k) == "" {
					return errors.New("配置不完整：缺少 " + k)
				}
			}
			codec, err := newCodec(str(cfg, "token"), str(cfg, "aes_key"), str(cfg, "corp_id"))
			if err != nil {
				return err
			}
			base := strings.TrimRight(str(cfg, "api_base"), "/")
			if base == "" {
				base = "https://qyapi.weixin.qq.com"
			}
			a = &app{codec: codec, corpID: str(cfg, "corp_id"), agentID: str(cfg, "agent_id"), secret: str(cfg, "secret"),
				base: base, http: &http.Client{Timeout: 20 * time.Second}, seen: map[string]time.Time{}}
			_, err = a.accessToken(ctx) // fail fast on wrong credentials
			return err
		},
		Ingress: func(_ context.Context, h *cat.Host, req cat.IngressRequest) (cat.IngressResponse, error) {
			return a.ingress(h, req)
		},
		Actions: map[string]cat.ActionFunc{
			"reply": func(ctx context.Context, _ *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				if actx.Event == nil || actx.Event.Actor == nil {
					return nil, errors.New("「回复」需要由一条消息触发；要主动发消息请用「发送给指定成员」")
				}
				return a.send(ctx, actx.Event.Actor.ID, str(p, "text"))
			},
			"send": func(ctx context.Context, _ *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				return a.send(ctx, str(p, "to_user"), str(p, "text"))
			},
			"typing": func(context.Context, *cat.Host, map[string]any, cat.ActionContext) (any, error) {
				return map[string]any{}, nil
			},
		},
	}
}
