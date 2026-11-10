package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dounx/cat/plugins/internal/faketest"
	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

func fakeDeepSeek(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-good" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/models":
			w.Write([]byte(`{"data":[]}`))
		case "/chat/completions":
			var body struct {
				Model    string              `json:"model"`
				Messages []map[string]string `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{
				"role": "assistant", "content": body.Model + " saw " + string(rune('0'+len(body.Messages))) + " messages",
			}}}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPIMode(t *testing.T) {
	srv := fakeDeepSeek(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"api_key": "sk-good", "api_base": srv.URL}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	ev := &cat.Event{Source: "qq", Conversation: &cat.Conversation{ID: "g1"}, Actor: &cat.Actor{ID: "u"}}
	out, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, ev)
	if err != nil || out["text"] != "deepseek-flash saw 1 messages" {
		t.Fatalf("first: %v %v", out, err)
	}
	out, _ = h.Invoke("chat", map[string]any{"prompt": "again"}, ev)
	if out["text"] != "deepseek-flash saw 3 messages" {
		t.Fatalf("second turn should include history: %v", out)
	}
}

func TestClaudeCodeMode(t *testing.T) {
	bin := faketest.Script(t, "claude", faketest.Claude)
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"api_key": "sk-good", "mode": "claude-code", "bin": bin, "model": "deepseek-v4-pro", "allowed_actors": "u1",
	}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	stranger := &cat.Event{Source: "qq", Conversation: &cat.Conversation{ID: "g1"}, Actor: &cat.Actor{ID: "u2"}}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, stranger); err == nil {
		t.Fatal("tool-capable mode must refuse unlisted senders")
	}
	member := &cat.Event{Source: "qq", Conversation: &cat.Conversation{ID: "g1"}, Actor: &cat.Actor{ID: "u1"}}
	out, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, member)
	if err != nil || out["text"] != "reply 1" {
		t.Fatalf("chat: %v %v", out, err)
	}
	joined := strings.Join(calls(), "\n")
	if !strings.Contains(joined, "ENV:https://api.deepseek.com/anthropic|sk-good|deepseek-v4-pro") {
		t.Fatalf("Claude Code was not pointed at DeepSeek:\n%s", joined)
	}
	if !strings.Contains(joined, "--model deepseek-v4-pro") {
		t.Fatalf("model flag missing:\n%s", joined)
	}
}

func TestSetupValidatesKey(t *testing.T) {
	srv := fakeDeepSeek(t)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"api_key": "sk-bad", "mode": "api"}
	h.Answers <- map[string]any{"api_key": "sk-good", "mode": "api"}
	cfg, err := h.RunSetup(map[string]any{"api_base": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["api_key"] != "sk-good" {
		t.Fatalf("config: %+v", cfg)
	}
}
