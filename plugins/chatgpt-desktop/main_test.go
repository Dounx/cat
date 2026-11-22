package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dounx/cat/plugins/internal/faketest"
	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

func msg(actor, conv string) *cat.Event {
	return &cat.Event{Source: "dc-1", Type: "message.created", Actor: &cat.Actor{ID: actor}, Conversation: &cat.Conversation{ID: conv}}
}

func TestCodexChat(t *testing.T) {
	bin := faketest.Bin(t, "codex")
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": bin, "system_prompt": "Answer in Chinese."}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	out, err := h.Invoke("chat", map[string]any{"prompt": "first"}, msg("7", "c"))
	if err != nil || out["text"] != "answer 1" || out["session_id"] != "thread-1" {
		t.Fatalf("first: %v %v", out, err)
	}
	out, err = h.Invoke("chat", map[string]any{"prompt": "second"}, msg("7", "c"))
	if err != nil || out["text"] != "answer 2" {
		t.Fatalf("second: %v %v", out, err)
	}
	lines := calls()
	if !strings.HasPrefix(lines[0], `ARGS:exec --json --skip-git-repo-check -c sandbox_mode="read-only" -o `) || !strings.HasSuffix(lines[0], " -") {
		t.Errorf("first args: %s", lines[0])
	}
	if lines[1] != "PROMPT:Answer in Chinese.~~first~" && lines[1] != "PROMPT:Answer in Chinese.~~first" {
		t.Errorf("first prompt should carry the instructions: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "ARGS:exec resume --json") || !strings.HasSuffix(lines[2], " thread-1 -") {
		t.Errorf("second args should resume thread-1: %s", lines[2])
	}
	if !strings.HasPrefix(lines[3], "PROMPT:second") {
		t.Errorf("resumed prompt should not repeat the instructions: %q", lines[3])
	}
}

func TestCodexFailures(t *testing.T) {
	bin := faketest.Bin(t, "codex")
	faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": bin}, State: map[string]string{"session:k": "gone"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// A session that can not be resumed falls back to a new one.
	out, err := h.Invoke("chat", map[string]any{"prompt": "hi", "session_key": "k"}, nil)
	if err != nil || out["session_id"] != "thread-2" {
		t.Fatalf("fallback: %v %v", out, err)
	}
	_, err = h.Invoke("chat", map[string]any{"prompt": "explode"}, nil)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("turn.failed: %v", err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "   "}, nil); err == nil {
		t.Fatal("empty prompt should fail")
	}
}

func TestMissingCLI(t *testing.T) {
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": "/nonexistent/codex"}})
	if err := h.Start(); err == nil || !strings.Contains(err.Error(), "安装 ChatGPT Desktop") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetup(t *testing.T) {
	bin := faketest.Bin(t, "codex")
	work := t.TempDir()
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"cwd": work, "sandbox": "workspace-write"}
	cfg, err := h.RunSetup(map[string]any{"bin": bin})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["sandbox"] != "workspace-write" || cfg["cwd"] != work || cfg["bin"] != bin {
		t.Fatalf("config: %+v", cfg)
	}
	foundInfo := false
	for len(h.Steps) > 0 {
		if s := <-h.Steps; s.Kind == cat.StepInfo && strings.Contains(s.Title, "codex-cli 0.0.0") {
			foundInfo = true
		}
	}
	if !foundInfo {
		t.Fatal("expected info step reporting detected codex version")
	}

	// Missing CLI asks to confirm retrying; declining cancels the wizard.
	h2 := cattest.New(t, New(), cattest.Options{})
	h2.Answers <- map[string]any{"confirmed": false}
	if _, err := h2.RunSetup(map[string]any{"bin": "/nonexistent/codex"}); err == nil {
		t.Fatal("declining missing CLI should cancel setup")
	}
	sawConfirm := false
	for len(h2.Steps) > 0 {
		if s := <-h2.Steps; s.Kind == cat.StepConfirm && strings.Contains(s.Title, "没有找到 ChatGPT Desktop") {
			sawConfirm = true
		}
	}
	if !sawConfirm {
		t.Fatal("expected confirm step when codex is missing")
	}
}

func TestParseRealStream(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"01a1c329-c5bc-7312-bee2-40f9085a2b3e"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Codex is ignoring 2 unrecognized configuration settings."}}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"pong"}}
{"type":"turn.completed","usage":{"input_tokens":23658,"cached_input_tokens":8064,"output_tokens":5}}`
	res, failure := parse([]byte(stream))
	if failure != "" || res.Text != "pong" || res.SessionID != "01a1c329-c5bc-7312-bee2-40f9085a2b3e" || res.Usage["output_tokens"] != float64(5) {
		t.Fatalf("parse: %+v %q", res, failure)
	}
}

// TestRealCodex talks to the installed Codex CLI. It spends real quota, so it
// only runs when asked: CAT_REAL_CODEX=1 go test ./plugins/chatgpt-desktop -run Real
func TestRealCodex(t *testing.T) {
	if os.Getenv("CAT_REAL_CODEX") == "" {
		t.Skip("set CAT_REAL_CODEX=1 to run against the real Codex CLI")
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	out, err := h.Invoke("chat", map[string]any{"prompt": "Remember the word 'mango'. Reply with exactly: ok", "session_key": "real"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first: %v", out)
	out, err = h.Invoke("chat", map[string]any{"prompt": "What word did I ask you to remember? Reply with just that word.", "session_key": "real"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second: %v", out)
	if !strings.Contains(strings.ToLower(out["text"].(string)), "mango") {
		t.Fatalf("the resumed session lost its context: %v", out["text"])
	}
}
