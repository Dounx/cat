package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeDingtalk is a little of the open platform: it gives out a token, opens
// a stream connection over which it pushes the queued bot messages, and
// records what is sent through it.
type fakeDingtalk struct {
	*httptest.Server
	mu     sync.Mutex
	queued []map[string]any
	sent   []map[string]any
}

func newFake(t *testing.T) *fakeDingtalk {
	f := &fakeDingtalk{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			f.mu.Lock()
			queued := f.queued
			f.queued = nil
			f.mu.Unlock()
			for i, m := range queued {
				data, _ := json.Marshal(m)
				frame, _ := json.Marshal(map[string]any{
					"specVersion": "1.0", "type": "CALLBACK", "data": string(data),
					"headers": map[string]string{"topic": "/v1.0/im/bot/messages/get", "messageId": "f" + string(rune('0'+i)), "contentType": "application/json"},
				})
				if conn.Write(r.Context(), websocket.MessageText, frame) != nil {
					return
				}
			}
			for { // the acknowledgements, until the bot hangs up
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
			}
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			if body["appSecret"] != "good" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"code": "invalidClientIdOrSecret", "message": "无效的clientId或者clientSecret"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"accessToken": "tok", "expireIn": 7200})
		case "/v1.0/gateway/connections/open":
			json.NewEncoder(w).Encode(map[string]any{"endpoint": "ws" + strings.TrimPrefix(f.URL, "http") + "/stream", "ticket": "tk"})
		case "/v1.0/robot/groupMessages/send", "/v1.0/robot/oToMessages/batchSend":
			if r.Header.Get("x-acs-dingtalk-access-token") != "tok" {
				w.WriteHeader(401)
				return
			}
			body["path"] = r.URL.Path
			f.mu.Lock()
			f.sent = append(f.sent, body)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"processQueryKey": "k"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func TestReceiveReplyAndSend(t *testing.T) {
	f := newFake(t)
	f.queued = []map[string]any{
		{"conversationId": "cidGROUP", "conversationType": "2", "conversationTitle": "产品群", "msgId": "m1", "msgtype": "text",
			"senderStaffId": "u_ann", "senderId": "$:x", "senderNick": "Ann", "isInAtList": true, "createAt": 1700000000000,
			"text": map[string]any{"content": " 部署一下 "}, "sessionWebhook": "https://oapi.dingtalk.com/robot/sendBySession?session=s1",
			"sessionWebhookExpiredTime": time.Now().Add(time.Hour).UnixMilli()},
		{"conversationId": "cidP2P", "conversationType": "1", "msgId": "m2", "msgtype": "text", "senderStaffId": "u_bob", "senderNick": "Bob",
			"text": map[string]any{"content": "你好"}},
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"client_id": "ding1", "client_secret": "good", "api_base": f.URL}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// the two come over the same connection, each handled on its own
	got := map[string]cat.Event{}
	for range 2 {
		ev := h.NextEvent()
		got[ev.Payload["message_id"].(string)] = ev
	}
	group, private := got["m1"], got["m2"]
	if group.Type != "message.created" || group.Conversation.ID != "cidGROUP" || group.Conversation.Kind != "group" || group.Conversation.Name != "产品群" {
		t.Fatalf("group conversation: %+v", group.Conversation)
	}
	if group.Actor.ID != "u_ann" || group.Actor.Name != "Ann" || group.Payload["text"] != "部署一下" || group.Payload["mentioned"] != true ||
		group.Time == nil || group.Time.UnixMilli() != 1700000000000 {
		t.Fatalf("group message: %+v %+v", group.Actor, group.Payload)
	}
	if private.Conversation.ID != "user:u_bob" || private.Conversation.Kind != "private" || private.Payload["mentioned"] != true {
		t.Fatalf("private message: %+v %+v", private.Conversation, private.Payload)
	}

	// The address a message came with is DingTalk's or it is not used; this
	// one came without, so the reply goes through the open platform.
	if at := webhook(&group); at != "https://oapi.dingtalk.com/robot/sendBySession?session=s1" {
		t.Errorf("webhook of the group message: %q", at)
	}
	for _, bad := range []map[string]any{
		{"sessionWebhook": "https://evil.example/robot?x=oapi.dingtalk.com"},
		{"sessionWebhook": "http://oapi.dingtalk.com/robot"},
		{"sessionWebhook": "https://oapi.dingtalk.com/robot", "sessionWebhookExpiredTime": float64(time.Now().Add(-time.Minute).UnixMilli())},
	} {
		if at := webhook(&cat.Event{Raw: bad}); at != "" {
			t.Errorf("webhook %v was taken: %q", bad, at)
		}
	}
	if out, err := h.Invoke("reply", map[string]any{"text": "# 好的\n马上"}, &private); err != nil || out["sent"] != float64(1) {
		t.Fatalf("reply: %v %v", out, err)
	}
	if _, err := h.Invoke("send", map[string]any{"chat_id": "cidGROUP", "text": "纯文本通知", "markdown": false}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := h.Invoke("send", map[string]any{"chat_id": "cidGROUP", "text": " "}, nil); err == nil {
		t.Error("an empty message was sent")
	}
	if _, err := h.Invoke("reply", map[string]any{"text": "x"}, nil); err == nil {
		t.Error("a reply to nothing was sent")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 2 {
		t.Fatalf("sent %d: %+v", len(f.sent), f.sent)
	}
	if s := f.sent[0]; s["path"] != "/v1.0/robot/oToMessages/batchSend" || s["robotCode"] != "ding1" || s["msgKey"] != "sampleMarkdown" ||
		s["userIds"].([]any)[0] != "u_bob" || !strings.Contains(s["msgParam"].(string), `"title":"好的"`) {
		t.Errorf("reply was sent as %+v", s)
	}
	if s := f.sent[1]; s["path"] != "/v1.0/robot/groupMessages/send" || s["openConversationId"] != "cidGROUP" || s["msgKey"] != "sampleText" ||
		!strings.Contains(s["msgParam"].(string), "纯文本通知") {
		t.Errorf("message was sent as %+v", s)
	}
}

func TestBadCredentials(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"client_id": "ding1", "client_secret": "wrong", "api_base": f.URL}})
	if err := h.Start(); err == nil || !strings.Contains(err.Error(), "无效的clientId") {
		t.Fatalf("start with a wrong secret: %v", err)
	}
	if _, err := newBot(map[string]any{"client_id": "x"}); err == nil {
		t.Error("a bot without a secret was made")
	}
	b, _ := newBot(map[string]any{"client_id": "ding1", "client_secret": "good", "api_base": f.URL})
	if _, err := b.accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
}
