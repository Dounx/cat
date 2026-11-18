package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/dounx/cat/core/plugin"
)

func TestNotifyInvoke(t *testing.T) {
	var gotTitle, gotBody string
	var gotSound bool
	n := &notify{
		deliver: func(_ context.Context, title, body string, sound bool) error {
			gotTitle, gotBody, gotSound = title, body, sound
			return nil
		},
	}

	if _, err := n.Invoke(context.Background(), "other", map[string]any{}, plugin.InvokeContext{}); err == nil {
		t.Fatal("expected error for unknown action")
	}
	if _, err := n.Invoke(context.Background(), "send", map[string]any{"text": "  "}, plugin.InvokeContext{}); err == nil {
		t.Fatal("expected error for empty text")
	}

	longText := strings.Repeat("字", 600)
	out, err := n.Invoke(context.Background(), "send", map[string]any{
		"text":  longText,
		"sound": true,
	}, plugin.InvokeContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTitle != defaultNotifyTitle || !gotSound {
		t.Fatalf("got title=%q sound=%v", gotTitle, gotSound)
	}
	if len([]rune(gotBody)) != 501 || !strings.HasSuffix(gotBody, "…") {
		t.Fatalf("unexpected truncation: len=%d suffix=%q", len([]rune(gotBody)), gotBody[len(gotBody)-6:])
	}
	if out["title"] != defaultNotifyTitle || out["text"] != gotBody {
		t.Fatalf("unexpected output: %+v", out)
	}
}
