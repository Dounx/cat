package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeTelegram is a tiny Bot API: it hands out queued updates once and
// records what the bot sends.
type fakeTelegram struct {
	*httptest.Server
	mu      sync.Mutex
	updates []map[string]any
	sent    []map[string]any
	offsets []float64
}

func newFake(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params)
		reply := func(v any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
		if !strings.HasPrefix(r.URL.Path, "/botgood-token/") {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/botgood-token/") {
		case "getMe":
			reply(map[string]any{"id": 999, "is_bot": true, "username": "cat_bot"})
		case "getUpdates":
			f.offsets = append(f.offsets, params["offset"].(float64))
			out := f.updates
			f.updates = nil
			if out == nil {
				f.mu.Unlock()
				time.Sleep(50 * time.Millisecond) // stand in for the long poll
				f.mu.Lock()
				out = []map[string]any{}
			}
			reply(out)
		case "sendMessage":
			if params["parse_mode"] == "Markdown" && strings.Contains(params["text"].(string), "*broken") {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "can't parse entities"})
				return
			}
			f.sent = append(f.sent, params)
			reply(map[string]any{"message_id": 500 + len(f.sent), "chat": map[string]any{"id": -100}})
		case "sendChatAction":
			f.sent = append(f.sent, params)
			reply(true)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegram) sentCopy() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func TestReceiveAndReply(t *testing.T) {
	f := newFake(t)
	f.updates = []map[string]any{
		{"update_id": 41, "message": map[string]any{
			"message_id": 7, "date": 1700000000, "text": "hey @Cat_Bot deploy please",
			"from": map[string]any{"id": 123, "first_name": "Ann", "last_name": "Lee", "username": "ann"},
			"chat": map[string]any{"id": -100, "type": "supergroup", "title": "Ops"},
		}},
		{"update_id": 42, "message": map[string]any{
			"message_id": 8, "text": "/status@cat_bot now",
			"from": map[string]any{"id": 123, "first_name": "Ann"},
			"chat": map[string]any{"id": 123, "type": "private"},
		}},
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"token": "good-token", "api_base": f.URL}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}

	ev := h.NextEvent()
	if ev.Type != "message.created" || ev.Conversation.ID != "-100" || ev.Conversation.Kind != "group" || ev.Conversation.Name != "Ops" {
		t.Fatalf("conversation: %+v %+v", ev, ev.Conversation)
	}
	if ev.Actor.ID != "123" || ev.Actor.Name != "Ann Lee" {
		t.Fatalf("actor: %+v", ev.Actor)
	}
	if ev.Payload["text"] != "hey @Cat_Bot deploy please" || ev.Payload["message_id"] != "7" || ev.Payload["mentioned"] != true {
		t.Fatalf("payload: %+v", ev.Payload)
	}
	second := h.NextEvent()
	if second.Payload["command"] != "status" || second.Payload["args"] != "now" || second.Conversation.Kind != "private" {
		t.Fatalf("command: %+v", second.Payload)
	}

	out, err := h.Invoke("reply", map[string]any{"text": "on it"}, &ev)
	if err != nil {
		t.Fatal(err)
	}
	if out["message_id"] != "501" {
		t.Fatalf("reply output: %+v", out)
	}
	sent := f.sentCopy()
	if sent[0]["chat_id"] != "-100" || sent[0]["text"] != "on it" || sent[0]["reply_parameters"].(map[string]any)["message_id"] != float64(7) {
		t.Fatalf("sendMessage params: %+v", sent[0])
	}

	// Markdown that Telegram rejects is resent as plain text.
	if _, err := h.Invoke("send", map[string]any{"chat_id": "55", "text": "*broken", "markdown": true}, nil); err != nil {
		t.Fatal(err)
	}
	if last := f.sentCopy()[1]; last["parse_mode"] != nil || last["chat_id"] != "55" {
		t.Fatalf("fallback: %+v", last)
	}

	// Long messages are split.
	if _, err := h.Invoke("send", map[string]any{"chat_id": "55", "text": strings.Repeat("a", 5000)}, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sentCopy()); n != 4 {
		t.Fatalf("want 4 sends after split, got %d", n)
	}

	if _, err := h.Invoke("reply", map[string]any{"text": "x"}, nil); err == nil {
		t.Fatal("reply without a triggering message should fail")
	}

	// The offset moved past the last update and was persisted.
	deadline := time.Now().Add(3 * time.Second)
	for h.State("offset") != "43" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.State("offset") != "43" {
		t.Fatalf("offset state = %q", h.State("offset"))
	}
}

func TestResumesFromStoredOffset(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"token": "good-token", "api_base": f.URL}, State: map[string]string{"offset": "77"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.offsets)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offsets) == 0 || f.offsets[0] != 77 {
		t.Fatalf("offsets: %v", f.offsets)
	}
}

func TestBadTokenFailsStart(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"token": "wrong", "api_base": f.URL}})
	err := h.Start()
	if err == nil || !strings.Contains(err.Error(), "Token 无效") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupRetriesUntilTokenValid(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"token": "wrong"}
	h.Answers <- map[string]any{"token": "good-token"}
	cfg, err := h.RunSetup(map[string]any{"api_base": f.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "good-token" || cfg["api_base"] != f.URL {
		t.Fatalf("config: %+v", cfg)
	}
	var titles []string
	for len(h.Steps) > 0 {
		s := <-h.Steps
		titles = append(titles, s.Kind+":"+s.Title)
		if s.Kind == cat.StepForm && len(titles) > 1 && !strings.Contains(s.Description, "Token 无效") {
			t.Errorf("second form should explain the failure: %q", s.Description)
		}
	}
	if titles[len(titles)-1] != "info:已连接 @cat_bot" {
		t.Fatalf("steps: %v", titles)
	}
}
