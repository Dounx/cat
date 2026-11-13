package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			w.Write([]byte(`{"data":[{"id":"deepseek-v4-pro","name":"DeepSeek-V4-Pro"}]}`))
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
	if lines[0] != "ARGS:--profile headless --json --approval-policy never --permission-mode workspace-write -" {
		t.Errorf("first args: %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], "PROMPT:Be brief.~~first") || lines[2] != "KEY:sk-good" {
		t.Errorf("first prompt/key: %q %q", lines[1], lines[2])
	}
	if lines[3] != "ARGS:--profile headless --json --approval-policy never --permission-mode workspace-write --session-id session-1 -" {
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
	if err == nil || !strings.Contains(err.Error(), "没有可用的密钥（llm-deepseek: no API key）") {
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
	if opts, err := h.Options("chat", "model"); err != nil || len(opts) != 1 || opts[0].Value != "deepseek-v4-pro" {
		t.Fatalf("options: %+v %v", opts, err)
	}
	out, _ = h.Invoke("chat", map[string]any{"prompt": "more", "model": "deepseek-v4-pro"}, ev)
	if out["text"] != "deepseek-v4-pro saw 5 messages" {
		t.Fatalf("the action's model should answer: %v", out)
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
	// An action may name its own model: both the flag and the endpoint's model follow it.
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi", "model": "deepseek-flash", "new_session": true}, member); err != nil {
		t.Fatal(err)
	}
	own := strings.Join(calls()[4:], "\n")
	if !strings.Contains(own, "--model deepseek-flash") || !strings.Contains(own, "ENV:https://api.deepseek.com/anthropic|sk-good|deepseek-flash") {
		t.Fatalf("the action's model should drive Claude Code:\n%s", own)
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

const desktopPatch = `# user layer
- id: locale
  config:
    preference: zh
- id: agent-default-model # picked in the app
  config:
    provider: magpie
    model: "auto/deepseek-v4.1-flash"
- id: llm-pi-ai
  config:
    providers:
      magpie:
        apiKeyEnv: MAGPIE_GATEWAY_KEY
        models:
          - id: auto/glm-5.3
            name: "GLM-5.3"
            input: [text]
- id: llm-deepseek
  disabled: !!js 'true'
- id: im-qqbot
  config:
    token: secret
`

func writeProfile(t *testing.T, home, name, patch string) {
	t.Helper()
	dir := filepath.Join(home, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cordis.patch.yml"), []byte(patch), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessBorrowsProfileModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeProfile(t, home, "desktop", desktopPatch)
	writeProfile(t, home, "headless", "- id: llm-pi-ai\n  config: {}\n")

	got := listProfiles()
	if len(got) != 2 || got[0] != (dshProfile{"desktop", "magpie", "auto/deepseek-v4.1-flash"}) || got[1] != (dshProfile{name: "headless"}) {
		t.Fatalf("profiles: %+v", got)
	}

	bin := faketest.Script(t, "dsh", fakeDsh)
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": bin, "api_key": "sk-good", "allow_anyone": true, "harness_profile": "desktop"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(calls()[0])
	if len(args) < 4 || args[0] != "ARGS:--profile" || args[1] != "headless" || args[2] != "--patch" {
		t.Fatalf("args: %v", args)
	}
	patch, err := os.ReadFile(args[3])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- id: agent-default-model", "provider: magpie", "- id: llm-pi-ai", "apiKeyEnv: MAGPIE_GATEWAY_KEY", "disabled: !!js 'true'"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}
	for _, not := range []string{"im-qqbot", "secret", "locale"} {
		if strings.Contains(string(patch), not) {
			t.Errorf("patch must not carry %q:\n%s", not, patch)
		}
	}

	// The node's model menu: what the borrowed profile declares, then DeepSeek's own.
	opts, err := h.Options("chat", "model")
	if err != nil || len(opts) != 1 || opts[0].Value != "magpie/auto/glm-5.3" || opts[0].Title != "GLM-5.3" {
		t.Fatalf("options: %+v %v", opts, err)
	}

	// A profile that picked no model is refused rather than silently ignored.
	writeProfile(t, home, "web", "- id: locale\n  config: {}\n")
	if _, err := overlay("web", "", t.TempDir()); err == nil {
		t.Fatal("profile without a model should be an error")
	}

	// An action may name its own model: it replaces the profile's choice and
	// keeps the provider setup.
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi", "model": "magpie/auto/glm-5.3", "new_session": true}, nil); err != nil {
		t.Fatal(err)
	}
	own, _ := os.ReadFile(strings.Fields(calls()[3])[3])
	if !strings.Contains(string(own), `provider: "magpie"`) || !strings.Contains(string(own), `model: "auto/glm-5.3"`) ||
		strings.Contains(string(own), "deepseek-v4.1-flash") || !strings.Contains(string(own), "apiKeyEnv: MAGPIE_GATEWAY_KEY") {
		t.Fatalf("patch for the action's model:\n%s", own)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi", "model": "glm"}, nil); err == nil || !strings.Contains(err.Error(), "提供方/模型") {
		t.Fatalf("a model without its provider should be refused: %v", err)
	}
}

func TestSetupTriesTheHarness(t *testing.T) {
	srv := fakeDeepSeek(t)
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	t.Setenv("DEEPSEEK_API_KEY", "")
	writeProfile(t, home, "desktop", desktopPatch)
	bin := faketest.Script(t, "dsh", fakeDsh)
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"mode": "harness"} // the Harness cannot answer: asked again
	h.Answers <- map[string]any{"mode": "harness", "api_key": "sk-good"}
	cfg, err := h.RunSetup(map[string]any{"api_base": srv.URL, "bin": bin})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["harness_profile"] != "desktop" || cfg["api_key"] != "sk-good" {
		t.Fatalf("config: %+v", cfg)
	}
	if lines := calls(); len(lines) != 6 || !strings.Contains(lines[0], "--patch") {
		t.Fatalf("setup should have tried the Harness twice, with the desktop model:\n%s", strings.Join(lines, "\n"))
	}
}
