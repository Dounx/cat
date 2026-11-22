package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
)

type resumeCall struct {
	RuleID  string
	StepID  string
	EventID string
	Output  map[string]any
}

type recordHost struct {
	mu        sync.Mutex
	state     map[string]string
	resumes   []resumeCall
	resumeErr error
	instID    string
}

func newRecordHost(instID string) *recordHost {
	return &recordHost{state: map[string]string{}, instID: instID}
}

func (h *recordHost) Emit(*event.Event) (string, error) { return "", nil }
func (h *recordHost) Log(string, string)                {}
func (h *recordHost) StateGet(k string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.state[k]
	return v, ok
}
func (h *recordHost) StateSet(k, v string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state[k] = v
	return nil
}
func (h *recordHost) InstanceID() string              { return h.instID }
func (h *recordHost) DataDir() string                 { return "" }
func (h *recordHost) SetStatus(string, string)        {}
func (h *recordHost) TriggerErrors(map[string]string) {}
func (h *recordHost) Resume(ruleID, stepID, eventID string, output map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resumeErr != nil {
		return h.resumeErr
	}
	h.resumes = append(h.resumes, resumeCall{RuleID: ruleID, StepID: stepID, EventID: eventID, Output: output})
	return nil
}

func (h *recordHost) resumeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.resumes)
}

func (h *recordHost) setResumeErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resumeErr = err
}

func TestDelayHoldAndResume(t *testing.T) {
	origTick := DelayTick
	DelayTick = 20 * time.Millisecond
	defer func() { DelayTick = origTick }()

	var mu sync.Mutex
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	host := newRecordHost(DelayID)
	d := &delay{now: getNow, timers: map[string]*delayTimer{}, wake: make(chan struct{}, 1)}
	if err := d.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer d.Stop()

	ictx := plugin.InvokeContext{
		RuleID: "rule-1",
		StepID: "wait-1",
		RunID:  "run-1",
		Event:  &event.Event{ID: "ev-1"},
	}
	out, err := d.Invoke(context.Background(), "wait", map[string]any{"seconds": 5, "note": "等待服务预热"}, ictx)
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatalf("expected ErrHeld, got err=%v out=%+v", err, out)
	}
	if out["waited_seconds"] != 5.0 || out["resume_at"] == "" {
		t.Fatalf("unexpected held output: %+v", out)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "等待服务预热") || !strings.Contains(note, "5 秒") {
		t.Fatalf("unexpected note: %q", note)
	}
	if host.resumeCount() != 0 {
		t.Fatalf("resumed too early")
	}

	advance(6 * time.Second)
	d.signalWake()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if host.resumeCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if host.resumeCount() != 1 {
		t.Fatalf("expected 1 resume call, got %d", host.resumeCount())
	}
	call := host.resumes[0]
	if call.RuleID != "rule-1" || call.StepID != "wait-1" || call.EventID != "ev-1" {
		t.Fatalf("unexpected resume target: %+v", call)
	}
	if call.Output["waited_seconds"] != 5.0 || call.Output["resumed_at"] == "" || call.Output["note"] != "等待服务预热" {
		t.Fatalf("unexpected resume output: %+v", call.Output)
	}

	// Timer must be removed from persisted state after resuming.
	raw, _ := host.StateGet(delayState)
	var saved map[string]*delayTimer
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("expected empty persisted timers after resume, got %+v", saved)
	}
}

func TestDelayPersistAndRestartRecovery(t *testing.T) {
	origTick := DelayTick
	DelayTick = 20 * time.Millisecond
	defer func() { DelayTick = origTick }()

	var mu sync.Mutex
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	host := newRecordHost(DelayID)
	d1 := &delay{now: getNow, timers: map[string]*delayTimer{}, wake: make(chan struct{}, 1)}
	if err := d1.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	ictx := plugin.InvokeContext{RuleID: "r-restart", StepID: "s-wait", Event: &event.Event{ID: "ev-restart"}}
	if _, err := d1.Invoke(context.Background(), "wait", map[string]any{"seconds": 10}, ictx); !errors.Is(err, plugin.ErrHeld) {
		t.Fatalf("expected ErrHeld, got %v", err)
	}
	if err := d1.Stop(); err != nil {
		t.Fatal(err)
	}

	// Advance clock past due while Cat is down, then start a new instance.
	mu.Lock()
	now = now.Add(15 * time.Second)
	mu.Unlock()

	d2 := &delay{now: getNow, timers: map[string]*delayTimer{}, wake: make(chan struct{}, 1)}
	if err := d2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer d2.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if host.resumeCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if host.resumeCount() != 1 {
		t.Fatalf("expected resumed timer after restart, got %d", host.resumeCount())
	}
}

func TestDelayDisabledRuleKeepsPendingAndGoneDrops(t *testing.T) {
	now := time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)
	host := newRecordHost(DelayID)
	d := &delay{now: func() time.Time { return now }, timers: map[string]*delayTimer{}, wake: make(chan struct{}, 1)}
	d.host = host

	// Seed two due timers directly via Invoke.
	_, err := d.Invoke(context.Background(), "wait", map[string]any{"seconds": 1}, plugin.InvokeContext{
		RuleID: "r-disabled", StepID: "s1", Event: &event.Event{ID: "e1"},
	})
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)

	// Disabled rule error keeps the timer pending.
	host.setResumeErr(errors.New("规则已停用"))
	d.release()
	if len(d.timers) != 1 {
		t.Fatalf("expected timer to stay pending when rule disabled, got %d", len(d.timers))
	}

	// Re-enabling lets it resume and clear.
	host.setResumeErr(nil)
	d.release()
	if len(d.timers) != 0 || host.resumeCount() != 1 {
		t.Fatalf("expected timer to resume once re-enabled: timers=%d resumes=%d", len(d.timers), host.resumeCount())
	}

	// ErrGone drops the timer cleanly.
	_, _ = d.Invoke(context.Background(), "wait", map[string]any{"seconds": 1}, plugin.InvokeContext{
		RuleID: "r-deleted", StepID: "s2", Event: &event.Event{ID: "e2"},
	})
	now = now.Add(2 * time.Second)
	host.setResumeErr(plugin.ErrGone)
	d.release()
	if len(d.timers) != 0 {
		t.Fatalf("expected ErrGone timer to be dropped, got %d", len(d.timers))
	}
}

func TestDelayValidation(t *testing.T) {
	host := newRecordHost(DelayID)
	d := &delay{now: time.Now, timers: map[string]*delayTimer{}, wake: make(chan struct{}, 1)}
	d.host = host

	if _, err := d.Invoke(context.Background(), "other", map[string]any{}, plugin.InvokeContext{}); err == nil {
		t.Fatal("expected error for unknown action")
	}
	if _, err := d.Invoke(context.Background(), "wait", map[string]any{"seconds": 0}, plugin.InvokeContext{}); err == nil {
		t.Fatal("expected error for seconds=0")
	}
	if _, err := d.Invoke(context.Background(), "wait", map[string]any{"seconds": -5}, plugin.InvokeContext{}); err == nil {
		t.Fatal("expected error for negative seconds")
	}
	// Omitted seconds defaults to 60.
	out, err := d.Invoke(context.Background(), "wait", map[string]any{}, plugin.InvokeContext{RuleID: "r", StepID: "s"})
	if !errors.Is(err, plugin.ErrHeld) || out["waited_seconds"] != 60.0 {
		t.Fatalf("default seconds: out=%+v err=%v", out, err)
	}
}
