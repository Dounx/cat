package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
)

type memHost struct {
	state map[string]string
}

func (m *memHost) Emit(*event.Event) (string, error)                   { return "", nil }
func (m *memHost) Log(string, string)                                  {}
func (m *memHost) StateGet(k string) (string, bool)                    { v, ok := m.state[k]; return v, ok }
func (m *memHost) StateSet(k, v string) error                          { m.state[k] = v; return nil }
func (m *memHost) InstanceID() string                                  { return cooldownID }
func (m *memHost) DataDir() string                                     { return "" }
func (m *memHost) SetStatus(string, string)                            {}
func (m *memHost) TriggerErrors(map[string]string)                     {}
func (m *memHost) Resume(string, string, string, map[string]any) error { return nil }

func TestCooldownAllowHoldAndPersist(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	c := &cooldown{now: func() time.Time { return now }, seen: map[string]time.Time{}}
	if err := c.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	ictx := plugin.InvokeContext{RuleID: "r1", StepID: "cd1"}

	// 1. First event for key "api" passes.
	out, err := c.Invoke(context.Background(), "allow", map[string]any{"seconds": float64(60), "key": "api"}, ictx)
	if err != nil || out["passed"] != true {
		t.Fatalf("first api call: out=%+v err=%v", out, err)
	}

	// 2. Duplicate within cooldown holds with ErrHeld and readable note.
	now = now.Add(15 * time.Second)
	out, err = c.Invoke(context.Background(), "allow", map[string]any{"seconds": float64(60), "key": "api"}, ictx)
	if !errors.Is(err, plugin.ErrHeld) || out["passed"] != false || out["remaining_seconds"] != 45 || !strings.Contains(out["note"].(string), "45") {
		t.Fatalf("duplicate api call: out=%+v err=%v", out, err)
	}

	// 3. Different key "db" passes immediately.
	out, err = c.Invoke(context.Background(), "allow", map[string]any{"seconds": float64(60), "key": "db"}, ictx)
	if err != nil || out["passed"] != true {
		t.Fatalf("different key db: out=%+v err=%v", out, err)
	}

	// 4. Restarting instance reloads persisted cooldown windows.
	c2 := &cooldown{now: func() time.Time { return now }, seen: map[string]time.Time{}}
	if err := c2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Invoke(context.Background(), "allow", map[string]any{"seconds": float64(60), "key": "api"}, ictx); !errors.Is(err, plugin.ErrHeld) {
		t.Fatalf("expected persisted hold after restart, got %v", err)
	}

	// 5. After window expires, key "api" passes again.
	now = now.Add(46 * time.Second)
	out, err = c2.Invoke(context.Background(), "allow", map[string]any{"seconds": float64(60), "key": "api"}, ictx)
	if err != nil || out["passed"] != true {
		t.Fatalf("after expiry: out=%+v err=%v", out, err)
	}
}
