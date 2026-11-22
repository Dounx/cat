package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dounx/cat/core/plugin"
)

func TestStateManifestRegistered(t *testing.T) {
	var found *plugin.Manifest
	for _, e := range All() {
		if e.Manifest.ID == stateID {
			m := e.Manifest
			found = &m
			break
		}
	}
	if found == nil {
		t.Fatalf("state plugin not registered in builtin.All()")
	}
	if found.Name != "状态记忆" || !found.Builtin || !found.Singleton || found.Icon != "database" {
		t.Fatalf("unexpected manifest: %+v", found)
	}
	want := []string{"get", "set", "incr", "once", "delete"}
	if len(found.Actions) != len(want) {
		t.Fatalf("actions len = %d, want %d", len(found.Actions), len(want))
	}
	for i, w := range want {
		if found.Actions[i].Name != w {
			t.Fatalf("action[%d] = %q, want %q", i, found.Actions[i].Name, w)
		}
	}
}

func TestStateGetSetDelete(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	s := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	if err := s.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	ictx := plugin.InvokeContext{RuleID: "r1", StepID: "s1"}

	// Missing key without default.
	out, err := s.Invoke(context.Background(), "get", map[string]any{"key": "k1"}, ictx)
	if err != nil || out["found"] != false || out["value"] != nil || out["key"] != "k1" {
		t.Fatalf("get missing: out=%+v err=%v", out, err)
	}

	// Missing key with default.
	out, err = s.Invoke(context.Background(), "get", map[string]any{"key": "k1", "default": "fallback"}, ictx)
	if err != nil || out["found"] != false || out["value"] != "fallback" {
		t.Fatalf("get default: out=%+v err=%v", out, err)
	}

	// Set first time.
	out, err = s.Invoke(context.Background(), "set", map[string]any{"key": "k1", "value": "v1"}, ictx)
	if err != nil || out["changed"] != true || out["previous"] != nil || out["value"] != "v1" {
		t.Fatalf("set v1: out=%+v err=%v", out, err)
	}

	// Set same value -> changed=false.
	out, err = s.Invoke(context.Background(), "set", map[string]any{"key": "k1", "value": "v1"}, ictx)
	if err != nil || out["changed"] != false || out["previous"] != "v1" {
		t.Fatalf("set v1 again: out=%+v err=%v", out, err)
	}

	// Set new value -> changed=true, previous="v1".
	out, err = s.Invoke(context.Background(), "set", map[string]any{"key": "k1", "value": "v2"}, ictx)
	if err != nil || out["changed"] != true || out["previous"] != "v1" || out["value"] != "v2" {
		t.Fatalf("set v2: out=%+v err=%v", out, err)
	}

	// Get returns v2.
	out, err = s.Invoke(context.Background(), "get", map[string]any{"key": "k1", "default": "fallback"}, ictx)
	if err != nil || out["found"] != true || out["value"] != "v2" {
		t.Fatalf("get v2: out=%+v err=%v", out, err)
	}

	// Delete returns deleted=true, second delete returns deleted=false.
	out, err = s.Invoke(context.Background(), "delete", map[string]any{"key": "k1"}, ictx)
	if err != nil || out["deleted"] != true || out["key"] != "k1" {
		t.Fatalf("delete k1: out=%+v err=%v", out, err)
	}
	out, err = s.Invoke(context.Background(), "delete", map[string]any{"key": "k1"}, ictx)
	if err != nil || out["deleted"] != false {
		t.Fatalf("delete k1 again: out=%+v err=%v", out, err)
	}
	out, err = s.Invoke(context.Background(), "get", map[string]any{"key": "k1"}, ictx)
	if err != nil || out["found"] != false {
		t.Fatalf("get after delete: out=%+v err=%v", out, err)
	}
}

func TestStateScopeRuleVsGlobal(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	s := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	_ = s.Start(context.Background(), host)

	r1 := plugin.InvokeContext{RuleID: "rule-a", StepID: "s1"}
	r2 := plugin.InvokeContext{RuleID: "rule-b", StepID: "s1"}

	// Default scope ("rule") isolates keys per rule.
	if _, err := s.Invoke(context.Background(), "set", map[string]any{"key": "token", "value": "from-a"}, r1); err != nil {
		t.Fatal(err)
	}
	out, err := s.Invoke(context.Background(), "get", map[string]any{"key": "token"}, r2)
	if err != nil || out["found"] != false {
		t.Fatalf("rule-b should not see rule-a key: out=%+v err=%v", out, err)
	}

	// Global scope shares across rules and does not collide with rule scope.
	if _, err := s.Invoke(context.Background(), "set", map[string]any{"key": "token", "value": "shared", "scope": "global"}, r1); err != nil {
		t.Fatal(err)
	}
	out, err = s.Invoke(context.Background(), "get", map[string]any{"key": "token", "scope": "global"}, r2)
	if err != nil || out["found"] != true || out["value"] != "shared" {
		t.Fatalf("rule-b should see global key: out=%+v err=%v", out, err)
	}
	out, err = s.Invoke(context.Background(), "get", map[string]any{"key": "token", "scope": "rule"}, r1)
	if err != nil || out["found"] != true || out["value"] != "from-a" {
		t.Fatalf("rule-a scoped key should be intact: out=%+v err=%v", out, err)
	}

	// Invalid scope errors out.
	if _, err := s.Invoke(context.Background(), "get", map[string]any{"key": "token", "scope": "invalid"}, r1); err == nil {
		t.Fatalf("expected error for invalid scope")
	}
}

func TestStateIncr(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	s := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	_ = s.Start(context.Background(), host)
	ictx := plugin.InvokeContext{RuleID: "r1", StepID: "s1"}

	// Default delta=1 from 0.
	out, err := s.Invoke(context.Background(), "incr", map[string]any{"key": "cnt"}, ictx)
	if err != nil || out["previous"] != int64(0) || out["value"] != int64(1) {
		t.Fatalf("first incr: out=%+v err=%v", out, err)
	}
	// Custom delta +5 and TTL 60s.
	out, err = s.Invoke(context.Background(), "incr", map[string]any{"key": "cnt", "delta": float64(5), "ttl_seconds": float64(60)}, ictx)
	if err != nil || out["previous"] != int64(1) || out["value"] != int64(6) {
		t.Fatalf("second incr: out=%+v err=%v", out, err)
	}
	// Negative delta, omitting ttl_seconds preserves existing 60s TTL.
	now = now.Add(30 * time.Second)
	out, err = s.Invoke(context.Background(), "incr", map[string]any{"key": "cnt", "delta": "-2"}, ictx)
	if err != nil || out["previous"] != int64(6) || out["value"] != int64(4) {
		t.Fatalf("decrement: out=%+v err=%v", out, err)
	}
	// After another 31s (total 61s from TTL set), counter resets to 0.
	now = now.Add(31 * time.Second)
	out, err = s.Invoke(context.Background(), "incr", map[string]any{"key": "cnt"}, ictx)
	if err != nil || out["previous"] != int64(0) || out["value"] != int64(1) {
		t.Fatalf("after ttl expiry: out=%+v err=%v", out, err)
	}

	// Non-numeric existing value returns an error.
	_, _ = s.Invoke(context.Background(), "set", map[string]any{"key": "text", "value": "hello"}, ictx)
	if _, err := s.Invoke(context.Background(), "incr", map[string]any{"key": "text"}, ictx); err == nil {
		t.Fatalf("expected error incrementing non-numeric key")
	}

	// Numeric string stored via set can be incremented.
	_, _ = s.Invoke(context.Background(), "set", map[string]any{"key": "numstr", "value": "10"}, ictx)
	out, err = s.Invoke(context.Background(), "incr", map[string]any{"key": "numstr", "delta": 2.5}, ictx)
	if err != nil || out["previous"] != int64(10) || out["value"] != float64(12.5) {
		t.Fatalf("incr numeric string: out=%+v err=%v", out, err)
	}
}

func TestStateOnceDedupAndTTL(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	s := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	_ = s.Start(context.Background(), host)
	ictx := plugin.InvokeContext{RuleID: "r1", StepID: "dedup"}

	// 1. First time passes.
	out, err := s.Invoke(context.Background(), "once", map[string]any{"key": "ORD-1001"}, ictx)
	if err != nil || out["seen"] != false || out["first"] != true || out["key"] != "ORD-1001" {
		t.Fatalf("first once: out=%+v err=%v", out, err)
	}

	// 2. Duplicate within default 86400s TTL holds with ErrHeld.
	now = now.Add(time.Hour)
	out, err = s.Invoke(context.Background(), "once", map[string]any{"key": "ORD-1001"}, ictx)
	if !errors.Is(err, plugin.ErrHeld) || out["seen"] != true || out["first"] != false {
		t.Fatalf("duplicate once: out=%+v err=%v", out, err)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "ORD-1001") {
		t.Fatalf("expected note with key, got %q", note)
	}

	// 3. After 24h TTL expires, same key passes again.
	now = now.Add(23 * time.Hour)
	out, err = s.Invoke(context.Background(), "once", map[string]any{"key": "ORD-1001"}, ictx)
	if err != nil || out["seen"] != false || out["first"] != true {
		t.Fatalf("after default ttl expiry: out=%+v err=%v", out, err)
	}

	// 4. Permanent dedup with ttl_seconds: 0 never expires.
	out, err = s.Invoke(context.Background(), "once", map[string]any{"key": "PERM-1", "ttl_seconds": float64(0)}, ictx)
	if err != nil || out["first"] != true {
		t.Fatalf("first permanent once: out=%+v err=%v", out, err)
	}
	now = now.Add(3650 * 24 * time.Hour)
	out, err = s.Invoke(context.Background(), "once", map[string]any{"key": "PERM-1", "ttl_seconds": float64(0)}, ictx)
	if !errors.Is(err, plugin.ErrHeld) || out["seen"] != true {
		t.Fatalf("permanent once should still hold after 10 years: out=%+v err=%v", out, err)
	}

	// 5. Deleting a once key allows it to pass again.
	out, err = s.Invoke(context.Background(), "delete", map[string]any{"key": "PERM-1"}, ictx)
	if err != nil || out["deleted"] != true {
		t.Fatalf("delete perm key: out=%+v err=%v", out, err)
	}
	out, err = s.Invoke(context.Background(), "once", map[string]any{"key": "PERM-1", "ttl_seconds": float64(0)}, ictx)
	if err != nil || out["first"] != true {
		t.Fatalf("once after delete: out=%+v err=%v", out, err)
	}
}

func TestStateRestartPersistenceAndTTL(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	s := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	if err := s.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	ictx := plugin.InvokeContext{RuleID: "r1", StepID: "s1"}

	_, _ = s.Invoke(context.Background(), "set", map[string]any{"key": "perm", "value": "forever"}, ictx)
	_, _ = s.Invoke(context.Background(), "set", map[string]any{"key": "temp", "value": "short", "ttl_seconds": float64(30)}, ictx)
	_, _ = s.Invoke(context.Background(), "incr", map[string]any{"key": "counter", "delta": float64(7)}, ictx)
	_, _ = s.Invoke(context.Background(), "once", map[string]any{"key": "evt-1", "ttl_seconds": float64(120)}, ictx)

	// Restart 10s later: everything is reloaded intact.
	now = now.Add(10 * time.Second)
	s2 := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	if err := s2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if out, err := s2.Invoke(context.Background(), "get", map[string]any{"key": "perm"}, ictx); err != nil || out["value"] != "forever" {
		t.Fatalf("perm after restart: out=%+v err=%v", out, err)
	}
	if out, err := s2.Invoke(context.Background(), "get", map[string]any{"key": "temp"}, ictx); err != nil || out["value"] != "short" {
		t.Fatalf("temp after restart: out=%+v err=%v", out, err)
	}
	if out, err := s2.Invoke(context.Background(), "incr", map[string]any{"key": "counter"}, ictx); err != nil || out["value"] != int64(8) {
		t.Fatalf("counter after restart: out=%+v err=%v", out, err)
	}
	if _, err := s2.Invoke(context.Background(), "once", map[string]any{"key": "evt-1"}, ictx); !errors.Is(err, plugin.ErrHeld) {
		t.Fatalf("once should remain held after restart: err=%v", err)
	}

	// Restart 25s later (total 35s): "temp" has expired, "perm" and "evt-1" remain.
	now = now.Add(25 * time.Second)
	s3 := &stateStore{now: func() time.Time { return now }, entries: map[string]stateItem{}}
	if err := s3.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if out, err := s3.Invoke(context.Background(), "get", map[string]any{"key": "temp", "default": "gone"}, ictx); err != nil || out["found"] != false || out["value"] != "gone" {
		t.Fatalf("expired temp after restart: out=%+v err=%v", out, err)
	}
	if out, err := s3.Invoke(context.Background(), "get", map[string]any{"key": "perm"}, ictx); err != nil || out["found"] != true {
		t.Fatalf("perm should survive: out=%+v err=%v", out, err)
	}
}

func TestStateEvictionAndConcurrency(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := &memHost{state: map[string]string{}}
	s := &stateStore{now: func() time.Time { return now }, max: 3, entries: map[string]stateItem{}}
	_ = s.Start(context.Background(), host)
	ictx := plugin.InvokeContext{RuleID: "r1", StepID: "s1"}

	// Add 3 permanent entries with increasing timestamps, then a 4th -> oldest ("k1") evicted.
	for i := 1; i <= 4; i++ {
		now = now.Add(time.Second)
		if _, err := s.Invoke(context.Background(), "set", map[string]any{"key": fmt.Sprintf("k%d", i), "value": i}, ictx); err != nil {
			t.Fatal(err)
		}
	}
	if out, _ := s.Invoke(context.Background(), "get", map[string]any{"key": "k1"}, ictx); out["found"] != false {
		t.Fatalf("expected oldest key k1 to be evicted, got %+v", out)
	}
	if out, _ := s.Invoke(context.Background(), "get", map[string]any{"key": "k4"}, ictx); out["found"] != true {
		t.Fatalf("expected newest key k4 to be kept, got %+v", out)
	}

	// Concurrent increments are race-free and exact.
	cs := &stateStore{now: time.Now, entries: map[string]stateItem{}}
	_ = cs.Start(context.Background(), nil)
	var wg sync.WaitGroup
	const workers = 50
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cs.Invoke(context.Background(), "incr", map[string]any{"key": "hits"}, ictx); err != nil {
				t.Errorf("concurrent incr: %v", err)
			}
		}()
	}
	wg.Wait()
	out, err := cs.Invoke(context.Background(), "get", map[string]any{"key": "hits"}, ictx)
	if err != nil || out["value"] != int64(workers) {
		t.Fatalf("concurrent hits = %+v (err=%v), want %d", out, err, workers)
	}
}

func TestStateValidationErrors(t *testing.T) {
	s := &stateStore{now: time.Now, entries: map[string]stateItem{}}
	ictx := plugin.InvokeContext{RuleID: "r1"}

	cases := []struct {
		name   string
		action string
		params map[string]any
	}{
		{"unknown action", "flush", map[string]any{"key": "a"}},
		{"empty key", "get", map[string]any{"key": "   "}},
		{"missing value", "set", map[string]any{"key": "a"}},
		{"negative ttl on set", "set", map[string]any{"key": "a", "value": "1", "ttl_seconds": -5}},
		{"negative ttl on once", "once", map[string]any{"key": "a", "ttl_seconds": -1}},
		{"bad delta on incr", "incr", map[string]any{"key": "a", "delta": "abc"}},
	}
	for _, tc := range cases {
		if _, err := s.Invoke(context.Background(), tc.action, tc.params, ictx); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}
