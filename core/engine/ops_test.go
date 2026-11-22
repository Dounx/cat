package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

func TestPruneEventsCleansOrphanedRuns(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 1; i <= 10; i++ {
		evID := fmt.Sprintf("0000000000000000000000%02d", i)
		if err := st.SaveEvent(&event.Event{
			ID: evID, Source: "s1", Plugin: "webhook", Type: "t", Time: time.Now(), Payload: map[string]any{"i": i},
		}); err != nil {
			t.Fatal(err)
		}
		// Attach a run only to old events (1..5) and one new event (10)
		if i <= 5 || i == 10 {
			if err := st.SaveRun(&store.Run{
				ID: fmt.Sprintf("run-%02d", i), RuleID: "r1", RuleName: "R", EventID: evID, Status: "ok", StartedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := st.PruneEvents(3); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListEvents(store.EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("expected 3 events after prune, got %d", len(evs))
	}
	runs, err := st.ListRuns("", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "run-10" {
		t.Fatalf("expected only run-10 to survive orphan cleanup, got %+v", runs)
	}
}

func TestEngineKeepEventsRetention(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := engine.NewWithOptions(st, t.TempDir(), nil, engine.Options{KeepEvents: 6})
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()

	for i := 0; i < 12; i++ {
		if _, err := e.Emit(&event.Event{
			Source: "src", Plugin: "webhook", Type: "webhook.received", Payload: map[string]any{"n": i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := st.ListEvents(store.EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 6 {
		t.Fatalf("expected 6 retained events, got %d", len(evs))
	}
}

type slowRecorder struct {
	plugin.Base
	delay time.Duration

	mu            sync.Mutex
	active        int
	maxActive     int
	perConvActive map[string]int
	perConvMax    map[string]int
	orderByConv   map[string][]int
	completed     atomic.Int32
}

func (s *slowRecorder) Invoke(_ context.Context, _ string, _ map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	convID := ""
	if ictx.Event != nil && ictx.Event.Conversation != nil {
		convID = ictx.Event.Conversation.ID
	}
	seq := 0
	if ictx.Event != nil {
		if v, ok := ictx.Event.Payload["seq"].(int); ok {
			seq = v
		} else if f, ok := ictx.Event.Payload["seq"].(float64); ok {
			seq = int(f)
		}
	}

	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	if convID != "" {
		s.perConvActive[convID]++
		if s.perConvActive[convID] > s.perConvMax[convID] {
			s.perConvMax[convID] = s.perConvActive[convID]
		}
		s.orderByConv[convID] = append(s.orderByConv[convID], seq)
	}
	s.mu.Unlock()

	time.Sleep(s.delay)

	s.mu.Lock()
	s.active--
	if convID != "" {
		s.perConvActive[convID]--
	}
	s.mu.Unlock()
	s.completed.Add(1)
	return map[string]any{"seq": seq}, nil
}

func TestEngineMaxConcurrentCap(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := &slowRecorder{
		delay:         40 * time.Millisecond,
		perConvActive: map[string]int{},
		perConvMax:    map[string]int{},
		orderByConv:   map[string][]int{},
	}
	e := engine.NewWithOptions(st, t.TempDir(), nil, engine.Options{MaxConcurrent: 2})
	e.RegisterPlugin(plugin.Manifest{ID: "slow", Name: "Slow", Capabilities: []string{plugin.CapAction}},
		func(map[string]any) (plugin.Instance, error) { return rec, nil })
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()

	inst, err := e.SaveInstance(store.Instance{Plugin: "slow", Name: "slow", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, inst.ID)

	if _, err := e.SaveRule(rules.Rule{
		Name: "cap", Enabled: true,
		Steps: []rules.Step{{ID: "s1", Instance: inst.ID, Action: "do"}},
	}); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 6; i++ {
		if _, err := e.Emit(&event.Event{
			Source: "src", Plugin: "webhook", Type: "msg", Payload: map[string]any{"seq": i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "6 runs complete", func() bool { return rec.completed.Load() == 6 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.maxActive != 2 {
		t.Fatalf("expected maxActive == 2, got %d", rec.maxActive)
	}
}

func TestEnginePerConversationSerialization(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := &slowRecorder{
		delay:         30 * time.Millisecond,
		perConvActive: map[string]int{},
		perConvMax:    map[string]int{},
		orderByConv:   map[string][]int{},
	}
	e := engine.NewWithOptions(st, t.TempDir(), nil, engine.Options{MaxConcurrent: 8})
	e.RegisterPlugin(plugin.Manifest{ID: "slow", Name: "Slow", Capabilities: []string{plugin.CapAction}},
		func(map[string]any) (plugin.Instance, error) { return rec, nil })
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()

	inst, err := e.SaveInstance(store.Instance{Plugin: "slow", Name: "slow", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, inst.ID)

	if _, err := e.SaveRule(rules.Rule{
		Name: "chat", Enabled: true,
		Steps: []rules.Step{{ID: "s1", Instance: inst.ID, Action: "reply"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Emit 4 events to convA and 2 events to convB interleaved.
	for i := 1; i <= 4; i++ {
		if _, err := e.Emit(&event.Event{
			Source: "chat1", Plugin: "chat", Type: "message",
			Conversation: &event.Conversation{ID: "convA"},
			Payload:      map[string]any{"seq": i},
		}); err != nil {
			t.Fatal(err)
		}
		if i <= 2 {
			if _, err := e.Emit(&event.Event{
				Source: "chat1", Plugin: "chat", Type: "message",
				Conversation: &event.Conversation{ID: "convB"},
				Payload:      map[string]any{"seq": i},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitFor(t, "all 6 serialized runs complete", func() bool { return rec.completed.Load() == 6 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.perConvMax["convA"] != 1 || rec.perConvMax["convB"] != 1 {
		t.Fatalf("same conversation overlapped: %+v", rec.perConvMax)
	}
	if rec.maxActive < 2 {
		t.Fatalf("different conversations should run concurrently, maxActive=%d", rec.maxActive)
	}
	wantA := []int{1, 2, 3, 4}
	gotA := rec.orderByConv["convA"]
	if fmt.Sprint(gotA) != fmt.Sprint(wantA) {
		t.Fatalf("convA out of order: got %v, want %v", gotA, wantA)
	}
	wantB := []int{1, 2}
	gotB := rec.orderByConv["convB"]
	if fmt.Sprint(gotB) != fmt.Sprint(wantB) {
		t.Fatalf("convB out of order: got %v, want %v", gotB, wantB)
	}
}

func TestEnginePerConversationSerializationDisabled(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := &slowRecorder{
		delay:         40 * time.Millisecond,
		perConvActive: map[string]int{},
		perConvMax:    map[string]int{},
		orderByConv:   map[string][]int{},
	}
	off := false
	e := engine.NewWithOptions(st, t.TempDir(), nil, engine.Options{
		MaxConcurrent:          4,
		SerializeConversations: &off,
	})
	e.RegisterPlugin(plugin.Manifest{ID: "slow", Name: "Slow", Capabilities: []string{plugin.CapAction}},
		func(map[string]any) (plugin.Instance, error) { return rec, nil })
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()

	inst, _ := e.SaveInstance(store.Instance{Plugin: "slow", Name: "slow", Enabled: true})
	waitRunning(t, e, inst.ID)
	_, _ = e.SaveRule(rules.Rule{
		Name: "chat", Enabled: true,
		Steps: []rules.Step{{ID: "s1", Instance: inst.ID, Action: "reply"}},
	})

	for i := 1; i <= 3; i++ {
		_, _ = e.Emit(&event.Event{
			Source: "chat1", Plugin: "chat", Type: "message",
			Conversation: &event.Conversation{ID: "convA"},
			Payload:      map[string]any{"seq": i},
		})
	}
	waitFor(t, "3 concurrent runs complete", func() bool { return rec.completed.Load() == 3 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.perConvMax["convA"] < 2 {
		t.Fatalf("expected concurrent execution when serialization disabled, got max=%d", rec.perConvMax["convA"])
	}
}

func TestWorkspaceBackupExportAndImport(t *testing.T) {
	// Source workspace with secrets, settings, and rules.
	st1, _ := store.Open(":memory:")
	e1 := engine.New(st1, t.TempDir(), nil)
	e1.Secrets = memSecrets{}
	for _, b := range builtin.All() {
		e1.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := e1.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e1.Stop(); st1.Close() })

	_ = st1.KVSet(engine.SettingsScope, "public_url", "https://cat.example.com")
	_ = st1.KVSet(engine.SettingsScope, "market_url", "off")

	hook, err := e1.SaveInstance(store.Instance{
		ID:      "webhook-prod",
		Plugin:  "webhook",
		Name:    "生产 Webhook",
		Enabled: true,
		Config:  map[string]any{"secret": "top-secret-2026", "event_type": "deploy.hook"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := e1.SaveRule(rules.Rule{
		ID:      "rule-deploy",
		Name:    "自动部署通知",
		Enabled: true,
		Sources: []string{hook.ID},
		Steps: []rules.Step{{
			ID: "s1", Instance: "shell", Action: "run", Params: map[string]any{"command": "echo ok"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	backup, err := e1.ExportBackup()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, _ := json.Marshal(backup)
	if strings.Contains(string(raw), "top-secret-2026") {
		t.Fatalf("raw secret leaked in backup: %s", raw)
	}
	if backup.Format != engine.BackupFormat || backup.Version != engine.BackupVersion {
		t.Fatalf("unexpected header: %+v", backup)
	}
	if backup.Settings.PublicURL != "https://cat.example.com" || backup.Settings.MarketURL != "off" {
		t.Fatalf("unexpected settings: %+v", backup.Settings)
	}
	if len(backup.Instances) != 1 || backup.Instances[0].ID != "webhook-prod" {
		t.Fatalf("unexpected exported instances: %+v", backup.Instances)
	}
	if backup.Instances[0].Config["secret"] != engine.SecretMask {
		t.Fatalf("expected masked secret in export, got %v", backup.Instances[0].Config)
	}
	if fmt.Sprint(backup.Instances[0].MaskedSecrets) != "[secret]" {
		t.Fatalf("expected masked_secrets=[secret], got %v", backup.Instances[0].MaskedSecrets)
	}

	// Add a fake uninstalled plugin instance to verify warning handling.
	backup.Instances = append(backup.Instances, engine.BackupInstance{
		Instance: store.Instance{
			ID: "ghost-1", Plugin: "uninstalled-plugin", Name: "幽灵插件", Enabled: true, Config: map[string]any{},
		},
	})

	// Destination fresh workspace (new machine).
	st2, _ := store.Open(":memory:")
	e2 := engine.New(st2, t.TempDir(), nil)
	secrets2 := memSecrets{}
	e2.Secrets = secrets2
	for _, b := range builtin.All() {
		e2.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := e2.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Stop(); st2.Close() })

	res, err := e2.ImportBackup(backup, "merge")
	if err != nil {
		t.Fatalf("import merge: %v", err)
	}
	if res.InstancesImported != 1 || res.InstancesSkipped != 1 || res.RulesImported != 1 || res.RulesSkipped != 0 || !res.SettingsUpdated {
		t.Fatalf("unexpected import result: %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "uninstalled-plugin") {
		t.Fatalf("expected missing plugin warning, got %+v", res.Warnings)
	}
	if len(res.NeedsSecrets) != 1 || res.NeedsSecrets[0] != "生产 Webhook" {
		t.Fatalf("expected needs_secrets=[生产 Webhook], got %+v", res.NeedsSecrets)
	}
	// SecretMask placeholder must NOT be stored as an actual secret.
	if _, ok := secrets2["webhook-prod/secret"]; ok {
		t.Fatalf("mask placeholder should not be stored as a secret")
	}
	if pub, _ := st2.KVGet(engine.SettingsScope, "public_url"); pub != "https://cat.example.com" {
		t.Fatalf("public_url not restored: %q", pub)
	}
	if rules2, _ := e2.Rules(); len(rules2) != 1 || rules2[0].ID != rule.ID {
		t.Fatalf("rule ID not preserved: %+v", rules2)
	}

	// Fill secret on destination machine, then re-import in merge mode -> skipped & secret preserved!
	if _, err := e2.SaveInstance(store.Instance{
		ID: "webhook-prod", Plugin: "webhook", Name: "生产 Webhook", Enabled: true,
		Config: map[string]any{"secret": "new-machine-secret", "event_type": "deploy.hook"},
	}); err != nil {
		t.Fatal(err)
	}
	resMerge2, err := e2.ImportBackup(backup, "merge")
	if err != nil {
		t.Fatalf("second merge import: %v", err)
	}
	if resMerge2.InstancesImported != 0 || resMerge2.InstancesSkipped != 2 || resMerge2.RulesImported != 0 || resMerge2.RulesSkipped != 1 {
		t.Fatalf("unexpected second merge: %+v", resMerge2)
	}
	if secrets2["webhook-prod/secret"] != "new-machine-secret" {
		t.Fatalf("merge import wiped existing secret: %v", secrets2)
	}

	// Add an extra instance and rule, then import in replace mode.
	extraInst, _ := e2.SaveInstance(store.Instance{Plugin: "webhook", Name: "临时 Hook", Enabled: true})
	_, _ = e2.SaveRule(rules.Rule{Name: "临时规则", Enabled: true, Steps: []rules.Step{{ID: "s1", Instance: "shell", Action: "run", Params: map[string]any{"command": "echo hi"}}}})

	resReplace, err := e2.ImportBackup(backup, "replace")
	if err != nil {
		t.Fatalf("replace import: %v", err)
	}
	if resReplace.InstancesImported != 1 || resReplace.RulesImported != 1 {
		t.Fatalf("unexpected replace result: %+v", resReplace)
	}
	// Extra instance & rule removed, built-in singleton ("shell") preserved, existing secret on same ID preserved!
	if _, ok := e2.Instance(extraInst.ID); ok {
		t.Fatal("extra instance should have been deleted in replace mode")
	}
	if _, ok := e2.Instance("shell"); !ok {
		t.Fatal("built-in singleton shell should not be deleted in replace mode")
	}
	if secrets2["webhook-prod/secret"] != "new-machine-secret" {
		t.Fatalf("replace mode on same machine wiped existing secret for same ID: %v", secrets2)
	}
	if len(resReplace.NeedsSecrets) != 0 {
		t.Fatalf("same-machine replace with existing secret should not flag needs_secrets: %+v", resReplace.NeedsSecrets)
	}
}
