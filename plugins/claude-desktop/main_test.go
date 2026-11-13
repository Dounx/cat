package main

import (
	"strings"
	"testing"

	"github.com/dounx/cat/plugins/internal/faketest"
	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

func msg(actor, conv string) *cat.Event {
	return &cat.Event{Source: "tg-1", Type: "message.created", Actor: &cat.Actor{ID: actor, Name: "n"}, Conversation: &cat.Conversation{ID: conv}}
}

func TestChatSessions(t *testing.T) {
	bin := faketest.Script(t, "claude", faketest.Claude)
	calls := faketest.Log(t)
	work := t.TempDir()
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"bin": bin, "cwd": work, "system_prompt": "be brief", "allowed_tools": "Read,Bash(git status)",
	}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}

	out, err := h.Invoke("chat", map[string]any{"prompt": "--help me please"}, msg("42", "c1"))
	if err != nil || out["text"] != "reply 1" || out["session_id"] != "sess-1" || out["cost_usd"] != 0.01 {
		t.Fatalf("first: %v %v", out, err)
	}
	// Same conversation resumes; another conversation starts fresh.
	if out, _ := h.Invoke("chat", map[string]any{"prompt": "again"}, msg("43", "c1")); out["session_id"] != "sess-2" {
		t.Fatalf("second: %v", out)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "other"}, msg("42", "c2")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "reset", "new_session": true}, msg("42", "c1")); err != nil {
		t.Fatal(err)
	}
	var args, prompts, cwds []string
	for _, l := range calls() {
		switch {
		case strings.HasPrefix(l, "ARGS:"):
			args = append(args, l)
		case strings.HasPrefix(l, "PROMPT:"):
			prompts = append(prompts, l)
		case strings.HasPrefix(l, "CWD:"):
			cwds = append(cwds, l)
		}
	}
	if len(args) != 4 {
		t.Fatalf("want 4 runs, got %v", args)
	}
	first := args[0]
	for _, want := range []string{"-p --output-format json", "--allowedTools Read,Bash(git status)", "--permission-mode dontAsk", "--append-system-prompt be brief"} {
		if !strings.Contains(first, want) {
			t.Errorf("args missing %q: %s", want, first)
		}
	}
	if strings.Contains(first, "--resume") || strings.Contains(first, "help me") {
		t.Errorf("first run must not resume, and the prompt must not be an argument: %s", first)
	}
	if prompts[0] != "PROMPT:--help me please" {
		t.Errorf("prompt should arrive on stdin: %s", prompts[0])
	}
	if !strings.Contains(args[1], "--resume sess-1") {
		t.Errorf("second run should resume sess-1: %s", args[1])
	}
	if strings.Contains(args[2], "--resume") || strings.Contains(args[3], "--resume") {
		t.Errorf("new conversation / new_session must not resume: %s | %s", args[2], args[3])
	}
	if !strings.HasSuffix(cwds[0], strings.TrimPrefix(work, "/private")) && !strings.Contains(cwds[0], work) {
		t.Errorf("cwd: %s want %s", cwds[0], work)
	}
	if h.State("session:tg-1:c1") != "sess-4" || h.State("session:tg-1:c2") != "sess-3" {
		t.Errorf("stored sessions: c1=%s c2=%s", h.State("session:tg-1:c1"), h.State("session:tg-1:c2"))
	}
}

func TestErrorsAndLostSession(t *testing.T) {
	bin := faketest.Script(t, "claude", faketest.Claude)
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{
		Config: map[string]any{"bin": bin},
		State:  map[string]string{"session:k": "gone"},
	})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// The stored session no longer exists: the plugin starts a fresh one.
	out, err := h.Invoke("chat", map[string]any{"prompt": "hi", "session_key": "k"}, nil)
	if err != nil || out["text"] != "reply 2" {
		t.Fatalf("lost session: %v %v (%v)", out, err, calls())
	}
	// An error result from the CLI is reported with its message.
	_, err = h.Invoke("chat", map[string]any{"prompt": "explode"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Failed to authenticate") {
		t.Fatalf("error result: %v", err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "  "}, nil); err == nil {
		t.Fatal("empty prompt should fail")
	}
}

func TestMissingCLI(t *testing.T) {
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": "/nonexistent/claude"}})
	if err := h.Start(); err == nil || !strings.Contains(err.Error(), "安装 Claude Desktop") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetup(t *testing.T) {
	bin := faketest.Script(t, "claude", faketest.Claude)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"permission_mode": "dontAsk", "allowed_tools": "Read"}
	cfg, err := h.RunSetup(map[string]any{"bin": bin})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["allowed_tools"] != "Read" || cfg["bin"] != bin {
		t.Fatalf("config: %+v", cfg)
	}
}
