package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeAPI records each request's messages and answers "echo: <last user text>".
func fakeAPI(t *testing.T) (*httptest.Server, func() [][]map[string]string) {
	var mu sync.Mutex
	var seen [][]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k-1" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		var body struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Messages)
		mu.Unlock()
		last := body.Messages[len(body.Messages)-1]["content"]
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "echo: " + last + " (" + body.Model + ")"}}},
			"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]map[string]string(nil), seen...)
	}
}

func TestLLMConversation(t *testing.T) {
	srv, seen := fakeAPI(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "k-1", "model": "m1", "system_prompt": "be kind", "max_history": float64(4)}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	ev := &cat.Event{Source: "tg", Conversation: &cat.Conversation{ID: "c1"}, Actor: &cat.Actor{ID: "anyone"}}
	out, err := h.Invoke("chat", map[string]any{"prompt": "one"}, ev)
	if err != nil || out["text"] != "echo: one (m1)" || out["session_id"] == "" {
		t.Fatalf("first: %v %v", out, err)
	}
	for _, p := range []string{"two", "three", "four"} {
		if _, err := h.Invoke("chat", map[string]any{"prompt": p}, ev); err != nil {
			t.Fatal(err)
		}
	}
	reqs := seen()
	// Second request: system + previous exchange + the new prompt.
	if got := len(reqs[1]); got != 4 || reqs[1][0]["role"] != "system" || reqs[1][1]["content"] != "one" || reqs[1][3]["content"] != "two" {
		t.Fatalf("second request: %+v", reqs[1])
	}
	// History is capped at 4 messages, so the fourth request no longer has "one".
	if got := len(reqs[3]); got != 6 || reqs[3][1]["content"] != "two" {
		t.Fatalf("fourth request should carry a trimmed history: %+v", reqs[3])
	}
	// Without an event or key there is no memory.
	if _, err := h.Invoke("chat", map[string]any{"prompt": "solo"}, nil); err != nil {
		t.Fatal(err)
	}
	if last := seen()[4]; len(last) != 2 {
		t.Fatalf("one-off request: %+v", last)
	}
	// new_session forgets the conversation.
	if _, err := h.Invoke("chat", map[string]any{"prompt": "fresh", "new_session": true}, ev); err != nil {
		t.Fatal(err)
	}
	if last := seen()[5]; len(last) != 2 {
		t.Fatalf("after new_session: %+v", last)
	}
}

func TestLLMBadKey(t *testing.T) {
	srv, _ := fakeAPI(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "wrong", "model": "m"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "x"}, nil); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
		t.Fatalf("err = %v", err)
	}
}

func TestLLMJudge(t *testing.T) {
	// answers with whatever follows "say:" in the user's text
	var system string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		system = body.Messages[0]["content"]
		_, say, _ := strings.Cut(body.Messages[len(body.Messages)-1]["content"], "say:")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": say}}},
		})
	}))
	t.Cleanup(srv.Close)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "k", "model": "m"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	kinds := []any{"bug", "question", "not sure"}
	cases := []struct {
		say     string
		choices any
		want    string
	}{
		{"bug", kinds, "bug"},
		{" **Question**。", kinds, "question"},
		{"我认为是 bug，因为它崩了", kinds, "bug"},
		{"not sure", kinds, "not sure"},
		{"Yes.", nil, "yes"},
		{"no", "", "no"},
		{"乙", "甲，乙", "乙"},
		{"bug or question", kinds, ""},
		{"something else", kinds, ""},
	}
	for _, tc := range cases {
		p := map[string]any{"prompt": "say:" + tc.say}
		if tc.choices != nil {
			p["choices"] = tc.choices
		}
		out, err := h.Invoke("judge", p, nil)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%q should not count as an answer, got %v", tc.say, out)
			}
			continue
		}
		if err != nil || out["choice"] != tc.want {
			t.Errorf("%q: %v %v, want %s", tc.say, out, err, tc.want)
		}
	}
	if !strings.Contains(system, "- 甲") && !strings.Contains(system, "- bug") {
		t.Fatalf("the choices were not put to the model: %q", system)
	}
}
