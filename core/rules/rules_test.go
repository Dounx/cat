package rules

import (
	"reflect"
	"testing"

	"github.com/dounx/cat/core/event"
)

func ev() *event.Event {
	return &event.Event{
		ID: "e1", Source: "tg-1", Plugin: "telegram", Type: "message.created",
		Actor:        &event.Actor{ID: "42", Name: "alice"},
		Conversation: &event.Conversation{ID: "c9", Kind: "group"},
		Payload:      map[string]any{"text": "hello cat", "count": 2},
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"empty matches all", Rule{}, true},
		{"source hit", Rule{Sources: []string{"tg-1"}}, true},
		{"source miss", Rule{Sources: []string{"dc-1"}}, false},
		{"type glob", Rule{Types: []string{"message.*"}}, true},
		{"type miss", Rule{Types: []string{"member.joined"}}, false},
		{"condition true", Rule{Condition: `payload.text contains "cat" and actor.name == "alice"`}, true},
		{"condition false", Rule{Condition: `conversation.kind == "private"`}, false},
		{"regex", Rule{Condition: `payload.text matches "^hello"`}, true},
		{"missing field is nil", Rule{Condition: `payload.nope == nil`}, true},
	}
	for _, c := range cases {
		compiled, err := Compile(c.rule)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := compiled.Match(ev())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRender(t *testing.T) {
	env := Env(ev(), map[string]any{"a": map[string]any{"text": "out", "list": []any{1, 2}}})
	got, err := Render(map[string]any{
		"plain":  "x",
		"interp": "hi {{ actor.name }}: {{ payload.text }}!",
		"typed":  "{{ payload.count }}",
		"obj":    "{{ steps.a.list }}",
		"nested": []any{map[string]any{"k": "{{ steps.a.text }}"}},
		"num":    7,
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"plain": "x", "interp": "hi alice: hello cat!", "typed": float64(2),
		"obj": []any{1, 2}, "nested": []any{map[string]any{"k": "out"}}, "num": 7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	if _, err := Render("{{ oops", env); err == nil {
		t.Error("expected error for unclosed placeholder")
	}
}
