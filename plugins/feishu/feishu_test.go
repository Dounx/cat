package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cat "github.com/dounx/cat/sdk/go"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// fakeFeishu is a little of the open platform: it gives out a token, says
// who the bot is, names one chat and records the messages sent.
type fakeFeishu struct {
	*httptest.Server
	mu   sync.Mutex
	sent []map[string]any
}

func newFake(t *testing.T) *fakeFeishu {
	f := &fakeFeishu{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		reply := func(v map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/tenant_access_token/internal"):
			if body["app_secret"] != "good" {
				reply(map[string]any{"code": 10014, "msg": "app secret invalid"})
				return
			}
			reply(map[string]any{"code": 0, "tenant_access_token": "t-1", "expire": 7200})
		case path == "/open-apis/bot/v3/info":
			reply(map[string]any{"code": 0, "bot": map[string]any{"open_id": "ou_bot", "app_name": "Cat"}})
		case path == "/open-apis/im/v1/chats/oc_group":
			reply(map[string]any{"code": 0, "data": map[string]any{"name": "产品群"}})
		case strings.HasPrefix(path, "/open-apis/im/v1/messages/") && strings.Contains(path, "/resources/"):
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
		case path == "/open-apis/im/v1/images":
			reply(map[string]any{"code": 0, "data": map[string]any{"image_key": "img_k1"}})
		case path == "/open-apis/im/v1/files":
			reply(map[string]any{"code": 0, "data": map[string]any{"file_key": "file_k1"}})
		case strings.HasPrefix(path, "/open-apis/im/v1/messages"):
			f.mu.Lock()
			body["path"] = path
			f.sent = append(f.sent, body)
			n := len(f.sent)
			f.mu.Unlock()
			reply(map[string]any{"code": 0, "data": map[string]any{"message_id": "om_sent" + string(rune('0'+n)), "chat_id": "oc_group"}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func received(t *testing.T, raw string) *larkim.P2MessageReceiveV1 {
	t.Helper()
	in := &larkim.P2MessageReceiveV1{}
	if err := json.Unmarshal([]byte(raw), in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestFeishu(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	bad, _ := newBot(map[string]any{"app_id": "cli_1", "app_secret": "wrong", "domain": f.URL}, nil)
	if err := bad.identify(ctx); err == nil {
		t.Fatal("a wrong secret passed")
	}
	if _, err := newBot(map[string]any{"app_id": "cli_1"}, nil); err == nil {
		t.Fatal("a bot without a secret was made")
	}
	b, err := newBot(map[string]any{"app_id": "cli_1", "app_secret": "good", "domain": f.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.identify(ctx); err != nil || b.openID != "ou_bot" || b.name != "Cat" {
		t.Fatalf("identify: %v %+v", err, b)
	}

	// a message in a group that mentions the bot and someone else
	ev, ok := b.convert(ctx, nil, received(t, `{"schema":"2.0","header":{"event_id":"e1","create_time":"1700000000000"},"event":{
		"sender":{"sender_id":{"open_id":"ou_alice"},"sender_type":"user"},
		"message":{"message_id":"om_1","parent_id":"om_0","chat_id":"oc_group","chat_type":"group","message_type":"text",
			"content":"{\"text\":\"@_user_1 帮 @_user_2 看看这个\"}",
			"mentions":[{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"Cat"},{"key":"@_user_2","id":{"open_id":"ou_bob"},"name":"Bob"}]}}}`))
	if !ok {
		t.Fatal("a message was dropped")
	}
	if ev.Type != "message.created" || ev.Payload["text"] != "帮 @Bob 看看这个" || ev.Payload["mentioned"] != true ||
		ev.Payload["message_id"] != "om_1" || ev.Payload["reply_to_message_id"] != "om_0" {
		t.Errorf("payload: %+v", ev.Payload)
	}
	if c := ev.Conversation; c.ID != "oc_group" || c.Kind != "group" || c.Name != "产品群" {
		t.Errorf("conversation: %+v", c)
	}
	if ev.Actor.ID != "ou_alice" || ev.Actor.IsBot || ev.Time == nil || ev.Time.UnixMilli() != 1700000000000 {
		t.Errorf("actor %+v time %v", ev.Actor, ev.Time)
	}
	// a private chat counts as a mention; a group message without one does not
	ev, _ = b.convert(ctx, nil, received(t, `{"event":{"sender":{"sender_id":{"open_id":"ou_alice"}},
		"message":{"message_id":"om_2","chat_id":"oc_p2p","chat_type":"p2p","message_type":"text","content":"{\"text\":\"你好\"}"}}}`))
	if ev.Payload["mentioned"] != true || ev.Conversation.Kind != "private" || ev.Payload["text"] != "你好" {
		t.Errorf("private: %+v %+v", ev.Payload, ev.Conversation)
	}
	ev, _ = b.convert(ctx, nil, received(t, `{"event":{"sender":{"sender_id":{"open_id":"ou_alice"}},
		"message":{"message_id":"om_3","chat_id":"oc_group","chat_type":"group","message_type":"text","content":"{\"text\":\"路过\"}"}}}`))
	if ev.Payload["mentioned"] != false {
		t.Errorf("a passing remark counted as a mention: %+v", ev.Payload)
	}
	// the bot's own messages are left alone
	if _, ok := b.convert(ctx, nil, received(t, `{"event":{"sender":{"sender_id":{"open_id":"ou_bot"}},
		"message":{"message_id":"om_4","chat_id":"oc_group","chat_type":"group","message_type":"text","content":"{\"text\":\"我说的\"}"}}}`)); ok {
		t.Error("the bot heard itself")
	}

	// a reply quotes the message; a plain send goes to the chat
	out, err := b.send(ctx, "oc_group", "收到", "om_1", false)
	if err != nil || out.(map[string]any)["message_id"] != "om_sent1" {
		t.Fatalf("reply: %v %v", out, err)
	}
	if _, err := b.send(ctx, "oc_group", "**加粗**的通知", "", true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := b.send(ctx, "oc_group", "  ", "", false); err == nil {
		t.Error("an empty message was sent")
	}
	{
		f.mu.Lock()
		if len(f.sent) != 2 {
			f.mu.Unlock()
			t.Fatalf("sent %d messages: %+v", len(f.sent), f.sent)
		}
		if s := f.sent[0]; s["path"] != "/open-apis/im/v1/messages/om_1/reply" || s["msg_type"] != "text" || !strings.Contains(s["content"].(string), "收到") {
			t.Errorf("reply was sent as %+v", s)
		}
		if s := f.sent[1]; s["path"] != "/open-apis/im/v1/messages" || s["receive_id"] != "oc_group" || s["msg_type"] != "post" || !strings.Contains(s["content"].(string), "加粗") {
			t.Errorf("markdown was sent as %+v", s)
		}
		f.mu.Unlock()
	}

	// incoming image resource downloads into blobsDir and sendWithFiles uploads image & file
	blobs := t.TempDir()
	h := &cat.Host{BlobsDir: blobs}
	imgEv, ok := b.convert(ctx, h, received(t, `{"event":{"sender":{"sender_id":{"open_id":"ou_alice"}},
		"message":{"message_id":"om_img","chat_id":"oc_p2p","chat_type":"p2p","message_type":"image","content":"{\"image_key\":\"img_in_1\"}"}}}`))
	if !ok {
		t.Fatal("image message dropped")
	}
	atts, _ := imgEv.Payload["attachments"].([]map[string]any)
	if len(atts) != 1 || atts[0]["kind"] != "image" || !strings.HasPrefix(atts[0]["path"].(string), blobs) {
		t.Fatalf("downloaded image att = %+v", imgEv.Payload["attachments"])
	}
	doc := filepath.Join(t.TempDir(), "doc.txt")
	_ = os.WriteFile(doc, []byte("hello"), 0o600)
	if _, err := b.sendWithFiles(ctx, "oc_group", "", []string{atts[0]["path"].(string), doc}, "", false); err != nil {
		t.Fatalf("sendWithFiles: %v", err)
	}
	if len(f.sent) != 4 || f.sent[2]["msg_type"] != "image" || f.sent[3]["msg_type"] != "file" {
		t.Fatalf("uploaded messages = %+v", f.sent[2:])
	}
}
