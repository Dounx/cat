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

// fakeDsh mimics `dsh headless --json`; the event shapes were captured from
// the DeepSeek Harness CLI.
const fakeDsh = `
prompt=$(cat)
echo "ARGS:$*" >> "$FAKE_LOG"
echo "PROMPT:$(echo "$prompt" | tr '\n' '~')" >> "$FAKE_LOG"
echo "KEY:$DEEPSEEK_API_KEY" >> "$FAKE_LOG"
if [ -z "$DEEPSEEK_API_KEY" ]; then
  echo '{"type":"session","sessionId":"session-x","cwd":"/tmp"}'
  echo '{"type":"final","sessionId":"session-x","reason":{"kind":"error","error":{"message":"llm-deepseek: no API key","code":"MISSING_CREDENTIAL"}},"text":""}'
  exit 0
fi
n=$(grep -c '^ARGS:' "$FAKE_LOG")
echo "{\"type\":\"session\",\"sessionId\":\"session-$n\",\"cwd\":\"/tmp\"}"
echo '{"type":"status","phase":"turn_start","turn":1}'
echo "{\"type\":\"final\",\"sessionId\":\"session-$n\",\"reason\":{\"kind\":\"completed\"},\"text\":\"done $n\"}"
`

func TestHarnessMode(t *testing.T) {
	bin := faketest.Script(t, "dsh", fakeDsh)
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"bin": bin, "api_key": "sk-good", "allowed_actors": "u1", "harness_permission": "workspace-write", "system_prompt": "Be brief.",
	}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	stranger := &cat.Event{Source: "qq", Conversation: &cat.Conversation{ID: "g1"}, Actor: &cat.Actor{ID: "u2"}}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, stranger); err == nil {
		t.Fatal("unlisted sender should be refused")
	}
	member := &cat.Event{Source: "qq", Conversation: &cat.Conversation{ID: "g1"}, Actor: &cat.Actor{ID: "u1"}}
	out, err := h.Invoke("chat", map[string]any{"prompt": "first"}, member)
	if err != nil || out["text"] != "done 1" || out["session_id"] != "session-1" {
		t.Fatalf("first: %v %v", out, err)
	}
	if out, err = h.Invoke("chat", map[string]any{"prompt": "second"}, member); err != nil || out["text"] != "done 2" {
		t.Fatalf("second: %v %v", out, err)
	}
	lines := calls()
	if lines[0] != "ARGS:headless --json --approval-policy never --permission-mode workspace-write -" {
		t.Errorf("first args: %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], "PROMPT:Be brief.~~first") || lines[2] != "KEY:sk-good" {
		t.Errorf("first prompt/key: %q %q", lines[1], lines[2])
	}
	if lines[3] != "ARGS:headless --json --approval-policy never --permission-mode workspace-write --session-id session-1 -" {
		t.Errorf("second args should resume: %s", lines[3])
	}
	if !strings.HasPrefix(lines[4], "PROMPT:second") {
		t.Errorf("resumed prompt should not repeat the instructions: %q", lines[4])
	}
}

func TestHarnessMissingCredential(t *testing.T) {
	bin := faketest.Script(t, "dsh", fakeDsh)
	faketest.Log(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": bin, "allow_anyone": true}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	_, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, nil)
	if err == nil || !strings.Contains(err.Error(), "没有可用的 API Key") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIMode(t *testing.T) {
	srv := fakeDeepSeek(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"mode": "api", "api_key": "sk-good", "api_base": srv.URL}})
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
	h.Answers <- map[string]any{"mode": "api"} // no key
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
