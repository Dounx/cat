package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

func newEngine(t *testing.T) *engine.Engine {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, t.TempDir(), nil)
	for _, b := range builtin.All() {
		e.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })
	return e
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitRunning(t *testing.T, e *engine.Engine, id string) {
	waitFor(t, "instance "+id, func() bool {
		v, ok := e.Instance(id)
		return ok && v.Status == engine.StatusRunning
	})
}

func post(t *testing.T, e *engine.Engine, instance, body string, headers map[string]string) plugin.IngressResponse {
	t.Helper()
	resp, err := e.Ingress(context.Background(), instance, plugin.IngressRequest{Method: "POST", Path: "/", Headers: headers, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebhookRuleHTTPAction(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		received = append(received, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":"pong"}`))
	}))
	defer echo.Close()

	e := newEngine(t)
	hook, err := e.SaveInstance(store.Instance{Plugin: "webhook", Name: "hook", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, "http")

	rule, err := e.SaveRule(rules.Rule{
		Name: "deploy", Enabled: true, Sources: []string{hook.ID},
		Condition: `payload.text contains "deploy"`,
		Steps: []rules.Step{
			{ID: "call", Instance: "http", Action: "request", Params: map[string]any{
				"url":  echo.URL,
				"body": map[string]any{"said": "{{ payload.text }}", "n": "{{ payload.n }}"},
			}},
			{ID: "again", Instance: "http", Action: "request", Params: map[string]any{
				"url":  echo.URL,
				"body": map[string]any{"prev": "got {{ steps.call.json.echo }}"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if r := post(t, e, hook.ID, `{"text":"please deploy now","n":3}`, nil); r.Status != 202 {
		t.Fatalf("status %d", r.Status)
	}
	post(t, e, hook.ID, `{"text":"nothing to see"}`, nil)

	waitFor(t, "run to finish", func() bool {
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		return len(runs) == 1 && runs[0].Status == "ok"
	})
	time.Sleep(50 * time.Millisecond) // the non-matching event must not start a run
	runs, _ := e.Store.ListRuns(rule.ID, "", 10)
	if len(runs) != 1 || len(runs[0].Steps) != 2 {
		t.Fatalf("want 1 run with 2 steps, got %+v", runs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("echo server got %d requests", len(received))
	}
	if received[0]["said"] != "please deploy now" || received[0]["n"] != float64(3) {
		t.Errorf("templating: %+v", received[0])
	}
	if received[1]["prev"] != "got pong" {
		t.Errorf("step output reference: %+v", received[1])
	}
	events, _ := e.Store.ListEvents(store.EventFilter{})
	if len(events) != 2 {
		t.Errorf("want 2 stored events, got %d", len(events))
	}
}

func TestWebhookSecret(t *testing.T) {
	e := newEngine(t)
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true, Config: map[string]any{"secret": "s3"}})
	waitRunning(t, e, hook.ID)
	if r := post(t, e, hook.ID, `{}`, nil); r.Status != 401 {
		t.Errorf("no secret: status %d", r.Status)
	}
	if r := post(t, e, hook.ID, `{}`, map[string]string{"X-Cat-Secret": "s3"}); r.Status != 202 {
		t.Errorf("with secret: status %d", r.Status)
	}
	events, _ := e.Store.ListEvents(store.EventFilter{})
	if len(events) != 1 {
		t.Errorf("want 1 event, got %d", len(events))
	}
}

func TestFailedStepStopsPipelineAndReplay(t *testing.T) {
	e := newEngine(t)
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true})
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, "shell")
	rule, err := e.SaveRule(rules.Rule{Name: "r", Enabled: true, Steps: []rules.Step{
		{Instance: "shell", Action: "run", Params: map[string]any{"command": "exit 3"}},
		{Instance: "shell", Action: "run", Params: map[string]any{"command": "echo never"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	post(t, e, hook.ID, `{}`, nil)
	waitFor(t, "failed run", func() bool {
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		return len(runs) == 1 && runs[0].Status == "error"
	})
	runs, _ := e.Store.ListRuns(rule.ID, "", 10)
	if len(runs[0].Steps) != 1 || runs[0].Steps[0].Error == "" {
		t.Fatalf("pipeline should stop at the failing step: %+v", runs[0].Steps)
	}
	if n, err := e.Replay(runs[0].EventID, rule.ID); err != nil || n != 1 {
		t.Fatalf("replay matched %d, err %v", n, err)
	}
	waitFor(t, "replayed run", func() bool {
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		return len(runs) == 2 && runs[0].Replay && runs[0].Status == "error"
	})
}

func TestThrottle(t *testing.T) {
	e := newEngine(t)
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true})
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, "shell")
	rule, _ := e.SaveRule(rules.Rule{Name: "r", Enabled: true, ThrottleSeconds: 60, ThrottleKey: "payload.user",
		Steps: []rules.Step{{Instance: "shell", Action: "run", Params: map[string]any{"command": "true"}}}})
	for _, body := range []string{`{"user":"a"}`, `{"user":"a"}`, `{"user":"b"}`} {
		post(t, e, hook.ID, body, nil)
	}
	waitFor(t, "runs", func() bool {
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		return len(runs) == 2
	})
	time.Sleep(50 * time.Millisecond)
	if runs, _ := e.Store.ListRuns(rule.ID, "", 10); len(runs) != 2 {
		t.Fatalf("want 2 runs (a once, b once), got %d", len(runs))
	}
}

func TestInvalidRuleRejected(t *testing.T) {
	e := newEngine(t)
	if _, err := e.SaveRule(rules.Rule{Name: "bad", Condition: "payload.text ==="}); err == nil {
		t.Fatal("expected a compile error")
	}
}

type memSecrets map[string]string

func (m memSecrets) Get(k string) (string, bool) { v, ok := m[k]; return v, ok }
func (m memSecrets) Set(k, v string) error       { m[k] = v; return nil }
func (m memSecrets) Delete(k string) error       { delete(m, k); return nil }

func TestSecretsStayOutOfDatabaseAndAPI(t *testing.T) {
	st, _ := store.Open(":memory:")
	e := engine.New(st, t.TempDir(), nil)
	secrets := memSecrets{}
	e.Secrets = secrets
	for _, b := range builtin.All() {
		e.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })

	hook, err := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true, Config: map[string]any{"secret": "s3cr3t", "event_type": "x.y"}})
	if err != nil {
		t.Fatal(err)
	}
	if hook.Config["secret"] != engine.SecretMask {
		t.Fatalf("API view should be masked: %v", hook.Config)
	}
	stored, _ := st.ListInstances()
	for _, rec := range stored {
		if b, _ := json.Marshal(rec); strings.Contains(string(b), "s3cr3t") {
			t.Fatalf("secret reached the database: %s", b)
		}
	}
	if secrets[hook.ID+"/secret"] != "s3cr3t" {
		t.Fatalf("secret store: %v", secrets)
	}
	// The running instance still received the real value.
	waitRunning(t, e, hook.ID)
	if r := post(t, e, hook.ID, `{}`, map[string]string{"X-Cat-Secret": "s3cr3t"}); r.Status != 202 {
		t.Fatalf("instance did not get its secret: %d", r.Status)
	}
	// Saving the masked view back keeps the secret; other fields update.
	view, _ := e.Instance(hook.ID)
	view.Config["event_type"] = "a.b"
	if _, err := e.SaveInstance(view.Instance); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, hook.ID)
	if r := post(t, e, hook.ID, `{}`, map[string]string{"X-Cat-Secret": "s3cr3t"}); r.Status != 202 {
		t.Fatalf("secret lost after an edit: %d", r.Status)
	}
	// Clearing the field removes it; deleting the instance removes the rest.
	view.Config["secret"] = ""
	_, _ = e.SaveInstance(view.Instance)
	if _, ok := secrets[hook.ID+"/secret"]; ok {
		t.Fatal("cleared secret still stored")
	}
}

func TestWebsocketReceiveSendReconnect(t *testing.T) {
	var mu sync.Mutex
	var got []string
	var auth string
	drop := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"text":"hello"}`))
		go func() {
			select {
			case <-drop:
				c.CloseNow()
			case <-ctx.Done():
			}
		}()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(data))
			mu.Unlock()
		}
	}))
	defer srv.Close()

	e := newEngine(t)
	ws, err := e.SaveInstance(store.Instance{Plugin: "websocket", Enabled: true, Config: map[string]any{
		"url": "ws" + strings.TrimPrefix(srv.URL, "http"), "token": "t0k",
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, ws.ID)
	countEvents := func() int {
		events, _ := e.Store.ListEvents(store.EventFilter{})
		return len(events)
	}
	waitFor(t, "first event", func() bool { return countEvents() == 1 })
	events, _ := e.Store.ListEvents(store.EventFilter{})
	if events[0].Type != "websocket.message" || events[0].Payload["text"] != "hello" {
		t.Errorf("event: %+v", events[0])
	}
	mu.Lock()
	if auth != "Bearer t0k" {
		t.Errorf("authorization header: %q", auth)
	}
	mu.Unlock()

	if _, err := e.Invoke(context.Background(), ws.ID, "send", map[string]any{"message": map[string]any{"a": 1}}, plugin.InvokeContext{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sent message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1 && got[0] == `{"a":1}`
	})

	drop <- struct{}{}
	waitFor(t, "event after reconnect", func() bool { return countEvents() == 2 })
	waitRunning(t, e, ws.ID)
}

func TestWebsocketRejectsBadURL(t *testing.T) {
	e := newEngine(t)
	ws, _ := e.SaveInstance(store.Instance{Plugin: "websocket", Enabled: true, Config: map[string]any{"url": "http://example.com"}})
	waitFor(t, "error status", func() bool {
		v, ok := e.Instance(ws.ID)
		return ok && v.Status == engine.StatusError
	})
}

func TestGraphRuleBranches(t *testing.T) {
	e := newEngine(t)
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true})
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, "shell")
	echo := func(id, text string) rules.Node {
		return rules.Node{Kind: rules.KindAction, Step: rules.Step{
			ID: id, Instance: "shell", Action: "run", Params: map[string]any{"command": "echo " + text},
		}}
	}
	// trigger → urgent? ─ yes → first → (first printed "one"?) → second
	//                    └ else → other
	rule, err := e.SaveRule(rules.Rule{Name: "g", Enabled: true, Graph: &rules.Graph{
		Nodes: []rules.Node{
			{Kind: rules.KindCondition, Step: rules.Step{ID: "urgent"}, Condition: `payload.level == "urgent"`},
			{Kind: rules.KindCondition, Step: rules.Step{ID: "said"}, Condition: `steps.first.stdout contains "one"`},
			echo("first", "one"), echo("second", "two"), echo("other", "three"),
		},
		Edges: []rules.Edge{
			{From: rules.TriggerNode, To: "urgent"},
			{From: "urgent", To: "first"},
			{From: "first", To: "said"},
			{From: "said", To: "second"},
			{From: "urgent", To: "other", Branch: rules.BranchElse},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	steps := func(n int) []string {
		t.Helper()
		waitFor(t, "runs", func() bool {
			runs, _ := e.Store.ListRuns(rule.ID, "", 10)
			return len(runs) == n && runs[0].Status == "ok"
		})
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		var ids []string
		for _, s := range runs[0].Steps {
			ids = append(ids, s.ID)
		}
		return ids
	}
	post(t, e, hook.ID, `{"level":"urgent"}`, map[string]string{"Content-Type": "application/json"})
	if got := strings.Join(steps(1), ","); got != "first,second" {
		t.Fatalf("urgent event ran %q", got)
	}
	post(t, e, hook.ID, `{"level":"low"}`, map[string]string{"Content-Type": "application/json"})
	if got := strings.Join(steps(2), ","); got != "other" {
		t.Fatalf("other event ran %q", got)
	}

	if _, err := e.SaveRule(rules.Rule{Name: "loop", Graph: &rules.Graph{
		Nodes: []rules.Node{echo("a", "x"), echo("b", "y")},
		Edges: []rules.Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}}); err == nil {
		t.Fatal("a graph with a loop should be rejected")
	}
}

func TestGraphThrottleNode(t *testing.T) {
	e := newEngine(t)
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true})
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, "shell")
	// trigger → limit (once a minute per payload.user) → run
	rule, err := e.SaveRule(rules.Rule{Name: "t", Enabled: true, Graph: &rules.Graph{
		Nodes: []rules.Node{
			{Kind: rules.KindTrigger, Step: rules.Step{ID: "in"}},
			{Kind: rules.KindThrottle, Step: rules.Step{ID: "limit"}, ThrottleSeconds: 60, ThrottleKey: "payload.user"},
			{Kind: rules.KindAction, Step: rules.Step{ID: "run", Instance: "shell", Action: "run", Params: map[string]any{"command": "echo hi"}}},
		},
		Edges: []rules.Edge{{From: "in", To: "limit"}, {From: "limit", To: "run"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	json := map[string]string{"Content-Type": "application/json"}
	post(t, e, hook.ID, `{"user":"a"}`, json)
	post(t, e, hook.ID, `{"user":"a"}`, json) // held back: same user, inside the window
	post(t, e, hook.ID, `{"user":"b"}`, json)
	waitFor(t, "two runs", func() bool {
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		return len(runs) == 2 && runs[0].Status == "ok" && runs[1].Status == "ok"
	})
	time.Sleep(200 * time.Millisecond)
	runs, _ := e.Store.ListRuns(rule.ID, "", 10)
	if len(runs) != 2 {
		t.Fatalf("the repeated event should have been held back, got %d runs", len(runs))
	}
	// a replay is let through whatever the throttle says
	if n, err := e.Replay(runs[0].EventID, rule.ID); err != nil || n != 1 {
		t.Fatalf("replay matched %d, err %v", n, err)
	}
}

func TestTryRuleRunsUnsavedRule(t *testing.T) {
	e := newEngine(t)
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true})
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, "shell")
	post(t, e, hook.ID, `{"text":"hi"}`, nil)
	var eventID string
	waitFor(t, "event", func() bool {
		list, _ := e.Store.ListEvents(store.EventFilter{Limit: 1})
		if len(list) == 1 {
			eventID = list[0].ID
		}
		return eventID != ""
	})
	rule := rules.Rule{Graph: &rules.Graph{
		Nodes: []rules.Node{
			{Step: rules.Step{ID: "t1"}, Kind: "trigger", Sources: []string{hook.ID}},
			{Step: rules.Step{ID: "s1", Instance: "shell", Action: "run", Params: map[string]any{"command": "echo {{ payload.text }}"}}, Kind: "action"},
		},
		Edges: []rules.Edge{{From: "t1", To: "s1"}},
	}}
	run, err := e.TryRule(rule, eventID)
	if err != nil || run == nil {
		t.Fatalf("run %v, err %v", run, err)
	}
	if run.Status != "ok" || !run.Replay || len(run.Steps) != 1 || run.Steps[0].ID != "s1" {
		t.Fatalf("unexpected run: %+v", run)
	}
	if saved, _ := e.Rules(); len(saved) != 0 {
		t.Fatalf("trying a rule must not save it, got %d", len(saved))
	}
	// an event no trigger takes starts nothing
	rule.Graph.Nodes[0].Sources = []string{"someone-else"}
	if run, err := e.TryRule(rule, eventID); err != nil || run != nil {
		t.Fatalf("want no run, got %v, err %v", run, err)
	}
}

// oracle is a plugin whose "judge" action picks the choice the event's
// payload names under "say", and counts how often it is asked.
type oracle struct {
	plugin.Base
	asked *int
	mu    *sync.Mutex
}

func (o oracle) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != rules.JudgeAction {
		return nil, plugin.ErrUnknownAction(action)
	}
	o.mu.Lock()
	*o.asked++
	o.mu.Unlock()
	say, _ := ictx.Event.Payload["say"].(string)
	if say == "" {
		return map[string]any{"text": "no idea"}, nil
	}
	return map[string]any{"choice": say, "prompt": p["prompt"], "choices": p["choices"]}, nil
}

func TestGraphJudgedCondition(t *testing.T) {
	e := newEngine(t)
	var mu sync.Mutex
	asked := 0
	e.RegisterPlugin(plugin.Manifest{ID: "oracle", Name: "oracle", Category: "processor"},
		func(map[string]any) (plugin.Instance, error) { return oracle{asked: &asked, mu: &mu}, nil })
	hook, _ := e.SaveInstance(store.Instance{Plugin: "webhook", Enabled: true})
	judge, err := e.SaveInstance(store.Instance{Plugin: "oracle", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, hook.ID)
	waitRunning(t, e, judge.ID)
	waitRunning(t, e, "shell")
	echo := func(id, text string) rules.Node {
		return rules.Node{Kind: rules.KindAction, Step: rules.Step{
			ID: id, Instance: "shell", Action: "run", Params: map[string]any{"command": "echo " + text},
		}}
	}
	// trigger → kind (bug / question) ─ bug → spam? ─ yes → drop
	//                                  │             └ else → fix (told what kind said)
	//                                  └ else → other
	rule, err := e.SaveRule(rules.Rule{Name: "judged", Enabled: true, Graph: &rules.Graph{
		Nodes: []rules.Node{
			{Kind: rules.KindCondition, Choices: []string{"bug", "question"}, Step: rules.Step{
				ID: "kind", Instance: judge.ID, Params: map[string]any{"prompt": "what is {{ payload.text }}"},
			}},
			{Kind: rules.KindCondition, Step: rules.Step{ID: "spam", Instance: judge.ID, Action: rules.JudgeAction}},
			echo("drop", "dropped"), echo("fix", "{{ steps.kind.choice }}"), echo("other", "other"),
		},
		Edges: []rules.Edge{
			{From: rules.TriggerNode, To: "kind"},
			{From: "kind", To: "spam", Branch: "bug"},
			{From: "spam", To: "drop"},
			{From: "spam", To: "fix", Branch: rules.BranchElse},
			{From: "kind", To: "other", Branch: rules.BranchElse},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	last := func(n int) store.Run {
		t.Helper()
		waitFor(t, "runs", func() bool {
			runs, _ := e.Store.ListRuns(rule.ID, "", 10)
			return len(runs) == n && runs[0].Status != "running"
		})
		runs, _ := e.Store.ListRuns(rule.ID, "", 10)
		return runs[0]
	}
	ids := func(r store.Run) string {
		var list []string
		for _, s := range r.Steps {
			list = append(list, s.ID)
		}
		return strings.Join(list, ",")
	}
	json := map[string]string{"Content-Type": "application/json"}

	// The oracle says "bug" to both: that is one of kind's choices, but not
	// a yes for spam, so the walk goes kind → spam → fix.
	post(t, e, hook.ID, `{"say":"bug","text":"it broke"}`, json)
	run := last(1)
	if got := ids(run); got != "kind,spam,fix" || run.Status != "ok" {
		t.Fatalf("bug ran %q (%s)", got, run.Status)
	}
	if p := run.Steps[0].Params; p["prompt"] != "what is it broke" || len(p["choices"].([]any)) != 2 {
		t.Fatalf("kind was asked %v", p)
	}
	if p := run.Steps[1].Params; len(p["choices"].([]any)) != 2 || p["choices"].([]any)[0] != rules.JudgeYes {
		t.Fatalf("spam was asked %v", p)
	}
	if out, _ := run.Steps[2].Output["stdout"].(string); !strings.Contains(out, "bug") {
		t.Fatalf("fix was given %v", run.Steps[2].Output)
	}
	// a choice that is not listed takes the else way out
	post(t, e, hook.ID, `{"say":"praise"}`, json)
	if got := ids(last(2)); got != "kind,other" {
		t.Fatalf("praise ran %q", got)
	}
	// no verdict at all is an error, and that branch ends there
	post(t, e, hook.ID, `{}`, json)
	run = last(3)
	if got := ids(run); got != "kind" || run.Status != "error" || !strings.Contains(run.Steps[0].Error, "choice") {
		t.Fatalf("no verdict: %q %s %+v", got, run.Status, run.Steps)
	}
	// each condition was asked once per event that reached it, and never
	// while only matching
	mu.Lock()
	defer mu.Unlock()
	if asked != 4 {
		t.Fatalf("asked %d times, want 4", asked)
	}
}

// watcher is a source that watches what the rules ask it to: it remembers
// the triggers it is given, finds fault with those that ask for "bad", and
// emits an event for a trigger on request.
type watcher struct {
	plugin.Base
	mu   sync.Mutex
	host plugin.Host
	sets [][]plugin.Trigger
}

func (w *watcher) Start(_ context.Context, h plugin.Host) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.host = h
	return nil
}

func (w *watcher) SetTriggers(_ context.Context, list []plugin.Trigger) error {
	w.mu.Lock()
	w.sets = append(w.sets, list)
	h := w.host
	w.mu.Unlock()
	errs := map[string]string{}
	for _, t := range list {
		if t.Params["feed"] == "bad" {
			errs[t.ID] = "看不懂的订阅源「bad」"
		}
	}
	if h != nil {
		h.TriggerErrors(errs)
	}
	return nil
}

// last gives the feeds asked for at the latest hand-over, and how many
// hand-overs there were.
func (w *watcher) last() (feeds string, n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.sets) == 0 {
		return "", 0
	}
	var list []string
	for _, t := range w.sets[len(w.sets)-1] {
		list = append(list, t.Params["feed"].(string))
	}
	sort.Strings(list)
	return strings.Join(list, ","), len(w.sets)
}

func (w *watcher) emit(t *testing.T, triggers ...string) {
	t.Helper()
	if _, err := w.host.Emit(&event.Event{Type: "feed.item", Triggers: triggers}); err != nil {
		t.Fatal(err)
	}
}

// What a rule's trigger node asks of its source reaches the source before it
// starts and whenever the rules change, and the node takes only the events
// emitted for it.
func TestTriggerParams(t *testing.T) {
	e := newEngine(t)
	w := &watcher{}
	e.RegisterPlugin(plugin.Manifest{ID: "feeds", Name: "feeds", Category: "monitor", TriggerParams: map[string]any{"type": "object"}},
		func(map[string]any) (plugin.Instance, error) { return w, nil })
	src, err := e.SaveInstance(store.Instance{Plugin: "feeds", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, src.ID)
	waitRunning(t, e, "shell")
	if feeds, n := w.last(); feeds != "" || n != 1 {
		t.Fatalf("a new instance is told that nothing is asked of it: %q after %d", feeds, n)
	}

	watch := func(name, feed string) rules.Rule {
		t.Helper()
		r, err := e.SaveRule(rules.Rule{Name: name, Enabled: true, Graph: &rules.Graph{
			Nodes: []rules.Node{
				{Kind: rules.KindTrigger, Step: rules.Step{ID: "t1", Params: map[string]any{"feed": feed, "note": "  "}}, Sources: []string{src.ID}},
				{Kind: rules.KindAction, Step: rules.Step{ID: "s1", Instance: "shell", Action: "run", Params: map[string]any{"command": "echo " + name}}},
			},
			Edges: []rules.Edge{{From: "t1", To: "s1"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := watch("a", "one")
	b := watch("b", " one ") // the same thing, asked once
	c := watch("c", "two")
	if feeds, _ := w.last(); feeds != "one,two" {
		t.Fatalf("asked for %q", feeds)
	}
	// a rule that takes whatever the source emits
	all, err := e.SaveRule(rules.Rule{Name: "all", Enabled: true, Sources: []string{src.ID}, Steps: []rules.Step{
		{Instance: "shell", Action: "run", Params: map[string]any{"command": "echo all"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, n := w.last(); n != 3 { // at the start, then for "one" and for "two"
		t.Fatalf("a rule that asks nothing should change nothing: %d hand-overs", n)
	}

	runs := func(r rules.Rule) int {
		list, _ := e.Store.ListRuns(r.ID, "", 10)
		return len(list)
	}
	one := rules.TriggerID(map[string]any{"feed": "one"})
	w.emit(t, one)
	waitFor(t, "the rules that asked", func() bool { return runs(a) == 1 && runs(b) == 1 && runs(all) == 1 })
	w.emit(t) // an event nobody asked for in particular
	waitFor(t, "the rule that takes everything", func() bool { return runs(all) == 2 })
	if runs(a) != 1 || runs(b) != 1 || runs(c) != 0 {
		t.Fatalf("runs: a=%d b=%d c=%d", runs(a), runs(b), runs(c))
	}

	// Switched off or deleted, a rule no longer asks.
	c.Enabled = false
	if _, err := e.SaveRule(c); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteRule(a.ID); err != nil {
		t.Fatal(err)
	}
	if feeds, _ := w.last(); feeds != "one" { // b still asks
		t.Fatalf("asked for %q", feeds)
	}

	// What the source cannot make sense of shows on the rule.
	bad := watch("bad", "bad")
	list, _ := e.Rules()
	for _, r := range list {
		if want := r.ID == bad.ID; (r.Error != "") != want || (want && !strings.Contains(r.Error, "看不懂")) {
			t.Fatalf("rule %s: error %q", r.Name, r.Error)
		}
	}

	// A restarted instance is told again.
	if _, err := e.SaveInstance(src); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, e, src.ID)
	if feeds, _ := w.last(); feeds != "bad,one" {
		t.Fatalf("after a restart: %q", feeds)
	}

	// A trigger that asks for something has to say of whom.
	_, err = e.SaveRule(rules.Rule{Name: "nobody", Enabled: true, Graph: &rules.Graph{Nodes: []rules.Node{
		{Kind: rules.KindTrigger, Step: rules.Step{ID: "t1", Params: map[string]any{"feed": "x"}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "选定来源") {
		t.Fatalf("err = %v", err)
	}
}

// A plugin that works without being set up comes with an instance, once.
func TestDefaultInstance(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	clock := plugin.Manifest{ID: "clock", Name: "定时", Category: "monitor", DefaultInstance: true}
	boot := func(seen ...plugin.Manifest) *engine.Engine {
		e := engine.New(st, t.TempDir(), nil)
		for _, m := range seen {
			e.RegisterPlugin(m, func(map[string]any) (plugin.Instance, error) { return &watcher{}, nil })
		}
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		return e
	}
	count := func(e *engine.Engine, pluginID string) (n int) {
		for _, i := range e.Instances() {
			if i.Plugin == pluginID {
				n++
			}
		}
		return n
	}

	e := boot(clock)
	waitRunning(t, e, "clock")
	if v, _ := e.Instance("clock"); v.Name != "定时" || !v.Enabled {
		t.Fatalf("instance: %+v", v)
	}
	// One installed while Cat runs gets its instance then.
	pages := plugin.Manifest{ID: "pages", Name: "网页", Category: "monitor", DefaultInstance: true}
	e.RegisterPlugin(pages, func(map[string]any) (plugin.Instance, error) { return &watcher{}, nil })
	waitRunning(t, e, "pages")
	// More can be added beside it, and it can be deleted for good.
	if _, err := e.SaveInstance(store.Instance{Plugin: "clock", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteInstance("pages"); err != nil {
		t.Fatal(err)
	}
	e.Stop()

	e = boot(clock, pages)
	defer e.Stop()
	if count(e, "clock") != 2 || count(e, "pages") != 0 {
		t.Fatalf("after a restart: clock=%d pages=%d", count(e, "clock"), count(e, "pages"))
	}
	// A plugin that had instances before it came with one keeps just those.
	feeds := plugin.Manifest{ID: "feeds", Name: "feeds", Category: "monitor"}
	e.RegisterPlugin(feeds, func(map[string]any) (plugin.Instance, error) { return &watcher{}, nil })
	if _, err := e.SaveInstance(store.Instance{Plugin: "feeds", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	feeds.DefaultInstance = true
	e.RegisterPlugin(feeds, func(map[string]any) (plugin.Instance, error) { return &watcher{}, nil })
	if count(e, "feeds") != 1 {
		t.Fatalf("feeds=%d", count(e, "feeds"))
	}
}
