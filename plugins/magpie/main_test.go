package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

type seenChat struct {
	Auth     string
	Model    string              `json:"model"`
	Messages []map[string]string `json:"messages"`
}

// fakeMagpie is a gateway with two models. With key set it turns away every
// other key, as a Magpie reached from another machine does; without, it takes
// requests that carry none, as one on this machine does. It answers a chat
// with "echo: <last user text>".
func fakeMagpie(t *testing.T, key string) (*httptest.Server, func() []seenChat) {
	var mu sync.Mutex
	var seen []seenChat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if key != "" && auth != "Bearer "+key {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"no such key"}}`))
			return
		}
		switch r.URL.Path {
		case "/":
			json.NewEncoder(w).Encode(map[string]any{"name": "magpie", "version": "0.1.7"})
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "auto/fast", "display_name": "Fast", "magpie_label": "Fast · routing group"},
				{"id": "deepseek/deepseek-chat", "display_name": "DeepSeek Chat"},
			}})
		case "/v1/chat/completions":
			body := seenChat{Auth: auth}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			seen = append(seen, body)
			mu.Unlock()
			last := body.Messages[len(body.Messages)-1]["content"]
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "echo: " + last}}},
				"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 2},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenChat {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenChat(nil), seen...)
	}
}

func TestChatNeedsNoKeyOnThisMachine(t *testing.T) {
	srv, seen := fakeMagpie(t, "")
	// an address pasted with the API's /v1 is the same gateway
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL + "/v1/", "model": "auto/fast", "system_prompt": "be kind"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	ev := &cat.Event{Source: "tg", Conversation: &cat.Conversation{ID: "c1"}, Actor: &cat.Actor{ID: "anyone"}}
	out, err := h.Invoke("chat", map[string]any{"prompt": "one"}, ev)
	if err != nil || out["text"] != "echo: one" || out["session_id"] == "" || out["model"] != "auto/fast" {
		t.Fatalf("first: %v %v", out, err)
	}
	// The conversation carries on, and one action may ask another model.
	out, err = h.Invoke("chat", map[string]any{"prompt": "two", "model": "deepseek/deepseek-chat"}, ev)
	if err != nil || out["model"] != "deepseek/deepseek-chat" {
		t.Fatalf("second: %v %v", out, err)
	}
	reqs := seen()
	if reqs[0].Auth != "" || reqs[0].Model != "auto/fast" {
		t.Fatalf("first request: %+v", reqs[0])
	}
	if m := reqs[1].Messages; reqs[1].Model != "deepseek/deepseek-chat" || len(m) != 4 || m[0]["content"] != "be kind" || m[1]["content"] != "one" || m[3]["content"] != "two" {
		t.Fatalf("second request: %+v", reqs[1])
	}
}

func TestGatewayKeyIsSent(t *testing.T) {
	srv, seen := fakeMagpie(t, "sk-magpie-1")
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "sk-magpie-1", "model": "auto/fast"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Invoke("judge", map[string]any{"prompt": "yes"}, nil); err != nil || out["choice"] != "yes" {
		t.Fatalf("judge: %v %v", out, err)
	}
	if got := seen()[0].Auth; got != "Bearer sk-magpie-1" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestMagpieNotRunning(t *testing.T) {
	srv, _ := fakeMagpie(t, "")
	url := srv.URL
	srv.Close()
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": url, "model": "auto/fast"}})
	// Cat may start before Magpie does.
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "x"}, nil); err == nil || !strings.Contains(err.Error(), "连不上") || !strings.Contains(err.Error(), url) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupFindsMagpieAndOffersItsModels(t *testing.T) {
	srv, _ := fakeMagpie(t, "")
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"model": "deepseek/deepseek-chat", "system_prompt": "be brief"}
	cfg, err := h.RunSetup(map[string]any{"base_url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "deepseek/deepseek-chat" || cfg["base_url"] != srv.URL || cfg["system_prompt"] != "be brief" {
		t.Fatalf("config: %+v", cfg)
	}
	// The only question was which model, with the routing group first and picked.
	var form cat.Step
	for len(h.Steps) > 0 {
		if s := <-h.Steps; s.Kind == cat.StepForm {
			if form.Kind != "" {
				t.Fatalf("asked twice: %q then %q", form.Title, s.Title)
			}
			form = s
		}
	}
	b, _ := json.Marshal(form.Schema["properties"])
	var props struct {
		Model struct {
			Enum   []string `json:"enum"`
			Titles []string `json:"x-enum-titles"`
		} `json:"model"`
	}
	_ = json.Unmarshal(b, &props)
	if len(props.Model.Enum) != 2 || props.Model.Enum[0] != "auto/fast" || props.Model.Titles[0] != "Fast · routing group（auto/fast）" || props.Model.Titles[1] != "DeepSeek Chat（deepseek/deepseek-chat）" {
		t.Fatalf("model field: %+v", props.Model)
	}
	if form.Values["model"] != "auto/fast" || !strings.Contains(form.Description, "0.1.7") {
		t.Fatalf("form: %+v", form)
	}
}

func TestSetupAsksWhereMagpieIs(t *testing.T) {
	srv, _ := fakeMagpie(t, "sk-magpie-1")
	notMagpie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"something else"}`))
	}))
	t.Cleanup(notMagpie.Close)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"base_url": srv.URL}                           // a Magpie, but no key
	h.Answers <- map[string]any{"base_url": srv.URL, "api_key": "sk-wrong"}    // a wrong key
	h.Answers <- map[string]any{"base_url": srv.URL, "api_key": "sk-magpie-1"} // right
	h.Answers <- map[string]any{"model": "auto/fast"}                          // the model form
	cfg, err := h.RunSetup(map[string]any{"base_url": notMagpie.URL, "model": "gone/model"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["api_key"] != "sk-magpie-1" || cfg["model"] != "auto/fast" || cfg["base_url"] != srv.URL {
		t.Fatalf("config: %+v", cfg)
	}
	var problems []string
	for len(h.Steps) > 0 {
		if s := <-h.Steps; s.Kind == cat.StepForm && strings.HasPrefix(s.Description, "⚠") {
			problems = append(problems, s.Description)
		}
	}
	if len(problems) != 3 || !strings.Contains(problems[0], "不是 Magpie") || !strings.Contains(problems[1], "网关密钥") || !strings.Contains(problems[2], "网关密钥") {
		t.Fatalf("problems: %q", problems)
	}
}

// The wizard and one short exchange against the Magpie on this machine. It
// spends a few tokens there, so it only runs when asked:
// CAT_REAL_MAGPIE=1 go test ./plugins/magpie -run Real -v
// CAT_REAL_MAGPIE_MODEL names the model; the first one Magpie lists otherwise.
func TestRealMagpie(t *testing.T) {
	if os.Getenv("CAT_REAL_MAGPIE") == "" {
		t.Skip("set CAT_REAL_MAGPIE=1 to run against the local Magpie")
	}
	w := cattest.New(t, New(), cattest.Options{})
	w.Answers <- map[string]any{"model": os.Getenv("CAT_REAL_MAGPIE_MODEL")}
	cfg, err := w.RunSetup(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("setup: %v", cfg)
	h := cattest.New(t, New(), cattest.Options{Config: cfg})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	out, err := h.Invoke("chat", map[string]any{"prompt": "Reply with the single word: pong"}, nil)
	if err != nil || out["text"] == "" {
		t.Fatalf("chat: %v %v", out, err)
	}
	t.Logf("chat: %v", out)
	out, err = h.Invoke("judge", map[string]any{"prompt": "Is 2 greater than 1?"}, nil)
	if err != nil || out["choice"] != "yes" {
		t.Fatalf("judge: %v %v", out, err)
	}
}
