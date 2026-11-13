package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dounx/cat/plugins/internal/faketest"
	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeCodex mimics `codex exec --json`: the JSONL shape was captured from
// codex-cli 0.167, including the warning it emits as an "error" item.
const fakeCodex = `
if [ "$1" = "--version" ]; then echo "codex-cli 0.0.0"; exit 0; fi
prompt=$(cat)
echo "ARGS:$*" >> "$FAKE_LOG"
echo "PROMPT:$(echo "$prompt" | tr '\n' '~')" >> "$FAKE_LOG"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
case "$*" in *"resume"*"gone"*) echo '{"type":"error","message":"session not found"}'; exit 1;; esac
case "$prompt" in *explode*) echo '{"type":"thread.started","thread_id":"t-x"}'; echo '{"type":"turn.failed","error":{"message":"quota exceeded"}}'; exit 1;; esac
n=$(grep -c '^ARGS:' "$FAKE_LOG")
echo "{\"type\":\"thread.started\",\"thread_id\":\"thread-$n\"}"
echo '{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Codex is ignoring 2 unrecognized configuration settings."}}'
echo '{"type":"turn.started"}'
echo "{\"type\":\"item.completed\",\"item\":{\"id\":\"item_1\",\"type\":\"agent_message\",\"text\":\"answer $n\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}'
printf 'answer %s' "$n" > "$out"
`

func msg(actor, conv string) *cat.Event {
	return &cat.Event{Source: "dc-1", Type: "message.created", Actor: &cat.Actor{ID: actor}, Conversation: &cat.Conversation{ID: conv}}
}

func TestCodexChat(t *testing.T) {
	bin := faketest.Script(t, "codex", fakeCodex)
	calls := faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": bin, "allowed_actors": "7", "system_prompt": "Answer in Chinese."}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("chat", map[string]any{"prompt": "hi"}, msg("8", "c")); err == nil {
		t.Fatal("unlisted sender should be refused")
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
	bin := faketest.Script(t, "codex", fakeCodex)
	faketest.Log(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"bin": bin, "allow_anyone": true}, State: map[string]string{"session:k": "gone"}})
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
// only runs when asked: CAT_REAL_CODEX=1 go test ./plugins/codex-desktop -run Real
func TestRealCodex(t *testing.T) {
	if os.Getenv("CAT_REAL_CODEX") == "" {
		t.Skip("set CAT_REAL_CODEX=1 to run against the real Codex CLI")
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"allow_anyone": true}})
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
