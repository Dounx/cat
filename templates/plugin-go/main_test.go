package main

import (
	"testing"

	"github.com/dounx/cat/sdk/go/cattest"
)

func TestGreetAndTick(t *testing.T) {
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"interval": 0.05, "greeting": "喵"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if ev := h.NextEvent(); ev.Type != "hello.tick" {
		t.Fatalf("event: %+v", ev)
	}
	out, err := h.Invoke("greet", map[string]any{"name": "世界"}, nil)
	if err != nil || out["text"] != "喵，世界！" {
		t.Fatalf("greet: %v %v", out, err)
	}
}
