package builtin_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dounx/cat/core/api"
	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

func setupWebhookTestServer(t *testing.T) (*engine.Engine, *httptest.Server) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(st, t.TempDir(), nil)
	for _, b := range builtin.All() {
		eng.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: eng}).Handler())
	t.Cleanup(func() {
		srv.Close()
		eng.Stop()
		st.Close()
	})
	return eng, srv
}

func waitInstanceRunning(t *testing.T, eng *engine.Engine, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := eng.Instance(id); ok && v.Status == engine.StatusRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := eng.Instance(id)
	t.Fatalf("instance %s did not reach running state (status=%q err=%q)", id, v.Status, v.Error)
}

func waitRunDone(t *testing.T, eng *engine.Engine, ruleID string, wantCount int) []store.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := eng.Store.ListRuns(ruleID, "", 10)
		if len(runs) >= wantCount && runs[0].FinishedAt != nil {
			return runs
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, _ := eng.Store.ListRuns(ruleID, "", 10)
	t.Fatalf("timed out waiting for %d finished runs of rule %s (got %d)", wantCount, ruleID, len(runs))
	return nil
}

func postIngress(t *testing.T, url, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(raw)
}

func webhookFactory(t *testing.T) (plugin.Manifest, plugin.Factory) {
	t.Helper()
	for _, b := range builtin.All() {
		if b.Manifest.ID == "webhook" {
			return b.Manifest, b.Factory
		}
	}
	t.Fatal("webhook builtin not found")
	return plugin.Manifest{}, nil
}

type noopHost struct {
	onEmit func(*event.Event)
}

func (h *noopHost) Emit(ev *event.Event) (string, error) {
	if h.onEmit != nil {
		h.onEmit(ev)
	}
	return ev.ID, nil
}
func (*noopHost) Log(string, string)                                  {}
func (*noopHost) StateGet(string) (string, bool)                      { return "", false }
func (*noopHost) StateSet(string, string) error                       { return nil }
func (*noopHost) DataDir() string                                     { return "" }
func (*noopHost) InstanceID() string                                  { return "hook-unit" }
func (*noopHost) SetStatus(string, string)                            {}
func (*noopHost) TriggerErrors(map[string]string)                     {}
func (*noopHost) Resume(string, string, string, map[string]any) error { return nil }

func TestWebhookManifestCapabilitiesAndRespondAction(t *testing.T) {
	m, _ := webhookFactory(t)
	hasSource, hasAction := false, false
	for _, c := range m.Capabilities {
		if c == plugin.CapSource {
			hasSource = true
		}
		if c == plugin.CapAction {
			hasAction = true
		}
	}
	if !hasSource || !hasAction {
		t.Fatalf("expected capabilities [source, action], got %v", m.Capabilities)
	}
	if len(m.Actions) != 1 || m.Actions[0].Name != "respond" {
		t.Fatalf("expected respond action in manifest, got %+v", m.Actions)
	}
	props, _ := m.ConfigSchema["properties"].(map[string]any)
	if props["sync_response"] == nil || props["response_timeout"] == nil {
		t.Fatalf("expected sync_response and response_timeout in config_schema, got %+v", props)
	}
}

func TestWebhookAsyncDefaultReturns202(t *testing.T) {
	eng, srv := setupWebhookTestServer(t)
	hook, err := eng.SaveInstance(store.Instance{Plugin: "webhook", Name: "async-hook", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitInstanceRunning(t, eng, hook.ID)

	status, hdr, body := postIngress(t, srv.URL+"/ingress/"+hook.ID, `{"msg":"hi"}`, nil)
	if status != 202 {
		t.Fatalf("status = %d, want 202 (body=%s)", status, body)
	}
	if got := hdr.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed["id"] == "" {
		t.Fatalf("expected JSON {id}, got %q (err=%v)", body, err)
	}
}

func TestWebhookSyncCustomStatusHeadersAndBody(t *testing.T) {
	eng, srv := setupWebhookTestServer(t)
	hook, err := eng.SaveInstance(store.Instance{
		Plugin:  "webhook",
		Name:    "sync-hook",
		Enabled: true,
		Config:  map[string]any{"sync_response": true, "response_timeout": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitInstanceRunning(t, eng, hook.ID)

	// 1. JSON body + custom status + custom headers (targeting explicit webhook instance)
	rule, err := eng.SaveRule(rules.Rule{
		Name:    "sync-json",
		Enabled: true,
		Sources: []string{hook.ID},
		Steps: []rules.Step{{
			ID:       "r1",
			Instance: hook.ID,
			Action:   "respond",
			Params: map[string]any{
				"status":  201,
				"headers": map[string]any{"X-Trace": "cat-123", "X-From": "{{ payload.who }}"},
				"body":    `{"ok":true,"echo":"{{ payload.msg }}"}`,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, hdr, body := postIngress(t, srv.URL+"/ingress/"+hook.ID, `{"msg":"hello","who":"shortcut"}`, nil)
	if status != 201 {
		t.Fatalf("status = %d, want 201 (body=%s)", status, body)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if hdr.Get("X-Trace") != "cat-123" || hdr.Get("X-From") != "shortcut" {
		t.Fatalf("custom headers missing: %+v", hdr)
	}
	if body != `{"ok":true,"echo":"hello"}` {
		t.Fatalf("body = %q", body)
	}
	runs := waitRunDone(t, eng, rule.ID, 1)
	if runs[0].Status != "ok" || runs[0].Steps[0].Output["responded"] != true || runs[0].Steps[0].Output["status"] != float64(201) {
		t.Fatalf("unexpected run step output: %+v", runs[0])
	}

	// 2. Plain text body + explicit content_type override
	if err := eng.DeleteRule(rule.ID); err != nil {
		t.Fatal(err)
	}
	_, err = eng.SaveRule(rules.Rule{
		Name:    "sync-text",
		Enabled: true,
		Sources: []string{hook.ID},
		Steps: []rules.Step{{
			ID:       "r1",
			Instance: hook.ID,
			Action:   "respond",
			Params: map[string]any{
				"status":       200,
				"content_type": "text/markdown; charset=utf-8",
				"body":         "# Hi {{ payload.name }}",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, hdr, body = postIngress(t, srv.URL+"/ingress/"+hook.ID, `{"name":"Cat"}`, nil)
	if status != 200 || hdr.Get("Content-Type") != "text/markdown; charset=utf-8" || body != "# Hi Cat" {
		t.Fatalf("text response mismatch: status=%d ct=%q body=%q", status, hdr.Get("Content-Type"), body)
	}

	// 3. Auto text/plain when body is plain text and content_type is omitted
	list, _ := eng.Rules()
	for _, r := range list {
		_ = eng.DeleteRule(r.ID)
	}
	_, err = eng.SaveRule(rules.Rule{
		Name:    "sync-auto-text",
		Enabled: true,
		Sources: []string{hook.ID},
		Steps: []rules.Step{{
			ID:       "r1",
			Instance: rules.SourceInstance,
			Action:   "respond",
			Params: map[string]any{
				"body": "pong: {{ payload.msg }}",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, hdr, body = postIngress(t, srv.URL+"/ingress/"+hook.ID, `{"msg":"abc"}`, nil)
	if status != 200 || hdr.Get("Content-Type") != "text/plain; charset=utf-8" || body != "pong: abc" {
		t.Fatalf("auto text mismatch: status=%d ct=%q body=%q", status, hdr.Get("Content-Type"), body)
	}
}

func TestWebhookSyncSourceAndMultiStepGraphPipeline(t *testing.T) {
	eng, srv := setupWebhookTestServer(t)

	// Downstream HTTP API called mid-pipeline
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upper": strings.ToUpper(in["msg"].(string)),
		})
	}))
	defer downstream.Close()

	hook, err := eng.SaveInstance(store.Instance{
		Plugin:  "webhook",
		Name:    "api-hook",
		Enabled: true,
		Config:  map[string]any{"sync_response": true, "response_timeout": 15},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitInstanceRunning(t, eng, hook.ID)
	waitInstanceRunning(t, eng, "http")

	// Graph: trigger -> condition (payload.action == "ping")
	//   then -> http.request -> $source.respond (200 JSON)
	//   else -> $source.respond (400 JSON object)
	_, err = eng.SaveRule(rules.Rule{
		Name:    "webhook-api",
		Enabled: true,
		Graph: &rules.Graph{
			Nodes: []rules.Node{
				{Step: rules.Step{ID: "t1"}, Kind: rules.KindTrigger, Sources: []string{hook.ID}},
				{Step: rules.Step{ID: "c1"}, Kind: rules.KindCondition, Condition: `payload?.action == "ping"`},
				{
					Kind: rules.KindAction,
					Step: rules.Step{
						ID:       "h1",
						Instance: "http",
						Action:   "request",
						Params: map[string]any{
							"method": "POST",
							"url":    downstream.URL,
							"body":   map[string]any{"msg": "{{ payload.msg }}"},
						},
					},
				},
				{
					Kind: rules.KindAction,
					Step: rules.Step{
						ID:       "ok_resp",
						Instance: rules.SourceInstance,
						Action:   "respond",
						Params: map[string]any{
							"status": 200,
							"body":   `{"ok":true,"echo":"{{ payload.msg }}","upper":"{{ steps.h1.json.upper }}"}`,
						},
					},
				},
				{
					Kind: rules.KindAction,
					Step: rules.Step{
						ID:       "bad_resp",
						Instance: rules.SourceInstance,
						Action:   "respond",
						Params: map[string]any{
							"status": 400,
							"body": map[string]any{
								"ok":    false,
								"error": "unsupported action",
							},
						},
					},
				},
			},
			Edges: []rules.Edge{
				{From: "t1", To: "c1"},
				{From: "c1", To: "h1"},
				{From: "h1", To: "ok_resp"},
				{From: "c1", To: "bad_resp", Branch: rules.BranchElse},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Branch 1: action == "ping"
	status, hdr, body := postIngress(t, srv.URL+"/ingress/"+hook.ID, `{"action":"ping","msg":"hello world"}`, nil)
	if status != 200 || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("ping branch: status=%d ct=%q body=%q", status, hdr.Get("Content-Type"), body)
	}
	var res1 map[string]any
	if err := json.Unmarshal([]byte(body), &res1); err != nil {
		t.Fatalf("invalid json %q: %v", body, err)
	}
	if res1["ok"] != true || res1["echo"] != "hello world" || res1["upper"] != "HELLO WORLD" {
		t.Fatalf("unexpected ping response: %+v", res1)
	}

	// Branch 2: action == "unknown" -> 400 with map body marshaled to JSON
	status, hdr, body = postIngress(t, srv.URL+"/ingress/"+hook.ID, `{"action":"unknown"}`, nil)
	if status != 400 || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("else branch: status=%d ct=%q body=%q", status, hdr.Get("Content-Type"), body)
	}
	var res2 map[string]any
	if err := json.Unmarshal([]byte(body), &res2); err != nil {
		t.Fatalf("invalid json %q: %v", body, err)
	}
	if res2["ok"] != false || res2["error"] != "unsupported action" {
		t.Fatalf("unexpected else response: %+v", res2)
	}
}

func TestWebhookSyncTimeoutAndCleanup(t *testing.T) {
	_, factory := webhookFactory(t)
	inst, err := factory(map[string]any{"sync_response": true, "response_timeout": 1})
	if err != nil {
		t.Fatal(err)
	}
	var emittedID string
	if err := inst.Start(context.Background(), &noopHost{
		onEmit: func(ev *event.Event) { emittedID = ev.ID },
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := inst.HandleIngress(context.Background(), plugin.IngressRequest{
		Method: "POST",
		Body:   []byte(`{"x":1}`),
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("HandleIngress err = %v", err)
	}
	if resp.Status != 504 {
		t.Fatalf("timeout status = %d, want 504 (body=%s)", resp.Status, string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "timeout") {
		t.Fatalf("expected timeout error body, got %s", string(resp.Body))
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("timeout returned too early: %v", elapsed)
	}
	if emittedID == "" {
		t.Fatal("expected pre-generated event ID before Emit")
	}

	// After timeout, pending entry must be cleaned up: late respond returns error cleanly.
	_, err = inst.Invoke(context.Background(), "respond", map[string]any{"body": "too late"}, plugin.InvokeContext{
		Event: &event.Event{ID: emittedID},
	})
	if err == nil {
		t.Fatal("expected error when responding after timeout")
	}
}

func TestWebhookRespondEdgeCasesAndStopUnblocks(t *testing.T) {
	_, factory := webhookFactory(t)

	// 1. Calling respond twice or without pending request returns clean error without panicking.
	inst, err := factory(map[string]any{"sync_response": true, "response_timeout": 5})
	if err != nil {
		t.Fatal(err)
	}
	eventCh := make(chan *event.Event, 1)
	if err := inst.Start(context.Background(), &noopHost{
		onEmit: func(ev *event.Event) { eventCh <- ev },
	}); err != nil {
		t.Fatal(err)
	}

	// No event in InvokeContext
	if _, err := inst.Invoke(context.Background(), "respond", map[string]any{"body": "x"}, plugin.InvokeContext{}); err == nil {
		t.Fatal("expected error when InvokeContext.Event is nil")
	}
	// Unknown event ID
	if _, err := inst.Invoke(context.Background(), "respond", map[string]any{"body": "x"}, plugin.InvokeContext{
		Event: &event.Event{ID: "non-existent"},
	}); err == nil {
		t.Fatal("expected error for unknown event ID")
	}

	// Start a real in-flight sync request
	doneCh := make(chan plugin.IngressResponse, 1)
	go func() {
		r, _ := inst.HandleIngress(context.Background(), plugin.IngressRequest{Method: "POST", Body: []byte(`{"k":"v"}`)})
		doneCh <- r
	}()
	ev := <-eventCh

	// Invalid status should fail WITHOUT consuming pending channel
	if _, err := inst.Invoke(context.Background(), "respond", map[string]any{"status": 999, "body": "bad"}, plugin.InvokeContext{Event: ev}); err == nil {
		t.Fatal("expected error for invalid status 999")
	}

	// First valid respond succeeds
	out, err := inst.Invoke(context.Background(), "respond", map[string]any{"status": "202", "body": "accepted"}, plugin.InvokeContext{Event: ev})
	if err != nil || out["responded"] != true || out["status"] != 202 || out["event_id"] != ev.ID {
		t.Fatalf("first respond failed: out=%+v err=%v", out, err)
	}
	resp := <-doneCh
	if resp.Status != 202 || string(resp.Body) != "accepted" {
		t.Fatalf("unexpected ingress response: %+v", resp)
	}

	// Second respond for the same event returns clean error
	if _, err := inst.Invoke(context.Background(), "respond", map[string]any{"body": "second"}, plugin.InvokeContext{Event: ev}); err == nil {
		t.Fatal("expected error on second respond for the same event")
	}

	// 2. Context cancellation returns 499 and cleans up pending entry.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		r, _ := inst.HandleIngress(ctx, plugin.IngressRequest{Method: "POST", Body: []byte(`{}`)})
		doneCh <- r
	}()
	evCancelled := <-eventCh
	cancel()
	resp = <-doneCh
	if resp.Status != 499 {
		t.Fatalf("cancelled status = %d, want 499", resp.Status)
	}
	// Give defer cleanup a moment if needed, then verify respond fails
	time.Sleep(10 * time.Millisecond)
	if _, err := inst.Invoke(context.Background(), "respond", map[string]any{"body": "after cancel"}, plugin.InvokeContext{Event: evCancelled}); err == nil {
		t.Fatal("expected error when responding after context cancellation")
	}

	// 3. Stop() unblocks pending sync requests with 503 immediately.
	go func() {
		r, _ := inst.HandleIngress(context.Background(), plugin.IngressRequest{Method: "POST", Body: []byte(`{}`)})
		doneCh <- r
	}()
	<-eventCh
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case resp = <-doneCh:
		if resp.Status != 503 {
			t.Fatalf("stopped status = %d, want 503", resp.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop() did not unblock pending HandleIngress")
	}
}
