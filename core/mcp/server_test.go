package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/mcp"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

type testMCPHarness struct {
	st  *store.Store
	e   *engine.Engine
	srv *httptest.Server
}

func newTestServer(t *testing.T, extra ...func(*engine.Engine)) *testMCPHarness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, t.TempDir(), nil)
	for _, b := range builtin.All() {
		e.RegisterPlugin(b.Manifest, b.Factory)
	}
	for _, fn := range extra {
		fn(e)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&mcp.Server{Engine: e, Version: "test"})
	t.Cleanup(func() {
		srv.Close()
		e.Stop()
		st.Close()
	})
	return &testMCPHarness{st: st, e: e, srv: srv}
}

func (h *testMCPHarness) rpc(t *testing.T, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(h.srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode != http.StatusAccepted {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
	}
	return out
}

func (h *testMCPHarness) call(t *testing.T, name, args string) (any, bool) {
	t.Helper()
	out := h.rpc(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil {
		t.Fatalf("%s: %v", name, out)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	failed, _ := res["isError"].(bool)
	var v any
	if json.Unmarshal([]byte(text), &v) != nil {
		v = text
	}
	return v, failed
}

// An MCP client introduces itself, lists the tools and uses them: it makes a
// rule, sees it listed, changes it without losing where its nodes stand, and
// deletes it.
func TestServer(t *testing.T) {
	h := newTestServer(t)

	init := h.rpc(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	initRes := init["result"].(map[string]any)
	if v := initRes["protocolVersion"]; v != "2024-11-05" {
		t.Errorf("protocol version %v", v)
	}
	if v := initRes["serverInfo"].(map[string]any)["version"]; v != "test" {
		t.Errorf("serverInfo.version = %v, want %q", v, "test")
	}
	if out := h.rpc(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); out != nil {
		t.Errorf("a notification was answered: %v", out)
	}
	list := h.rpc(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)["result"].(map[string]any)["tools"].([]any)
	if len(list) < 13 {
		t.Errorf("expected at least 13 tools, got %d", len(list))
	}
	if out := h.rpc(t, `{"jsonrpc":"2.0","id":3,"method":"nope"}`); out["error"] == nil {
		t.Errorf("unknown method: %v", out)
	}

	conns, _ := h.call(t, "list_connections", `{}`)
	if s, _ := json.Marshal(conns); !strings.Contains(string(s), `"id":"http"`) {
		t.Errorf("connections: %s", s)
	}
	if _, failed := h.call(t, "invoke_action", `{"instance":"nope","action":"x"}`); !failed {
		t.Error("an action on nothing did not fail")
	}
	if _, failed := h.call(t, "save_rule", `{"rule":{"id":"nope","name":"x","graph":{"nodes":[],"edges":[]}}}`); !failed {
		t.Error("a rule that is not there was changed")
	}
	const graph = `"graph":{"nodes":[{"id":"t1","kind":"trigger"},{"id":"s1","kind":"action","instance":"http","action":"request","params":{"url":"http://127.0.0.1:1/"}}],"edges":[{"from":"t1","to":"s1"}]}`
	made, failed := h.call(t, "save_rule", `{"rule":{"name":"from an AI","enabled":false,`+graph+`}}`)
	if failed {
		t.Fatalf("save_rule: %v", made)
	}
	id := made.(map[string]any)["id"].(string)
	// the editor places the nodes; an AI that changes the rule leaves them be
	stored, _ := h.st.ListRules()
	stored[0].Graph.Nodes[1].Meta = map[string]any{"x": 300.0, "y": 40.0}
	h.st.SaveRule(&stored[0])
	if _, failed := h.call(t, "save_rule", `{"rule":{"id":"`+id+`","name":"renamed","enabled":false,`+graph+`}}`); failed {
		t.Fatal("changing the rule failed")
	}
	stored, _ = h.st.ListRules()
	if len(stored) != 1 || stored[0].Name != "renamed" || stored[0].Graph.Nodes[1].Meta["x"] != 300.0 {
		t.Errorf("after the change: %+v", stored)
	}
	if out, failed := h.call(t, "set_rule_enabled", `{"id":"`+id+`","enabled":true}`); failed || out.(map[string]any)["enabled"] != true {
		t.Errorf("enable: %v", out)
	}
	if rulesList, _ := h.call(t, "list_rules", `{}`); len(rulesList.([]any)) != 1 || rulesList.([]any)[0].(map[string]any)["meta"] != nil {
		t.Errorf("list_rules: %v", rulesList)
	}
	if _, failed := h.call(t, "delete_rule", `{"id":"`+id+`"}`); failed {
		t.Error("delete failed")
	}
	if stored, _ = h.st.ListRules(); len(stored) != 0 {
		t.Errorf("still there: %+v", stored)
	}

	evID, err := h.e.Emit(&event.Event{Source: "webhook", Plugin: "webhook", Type: "webhook.received", Payload: map[string]any{"text": "hello mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	const echoGraph = `"graph":{"nodes":[{"id":"t1","kind":"trigger"},{"id":"s1","kind":"action","instance":"shell","action":"run","params":{"command":"echo mcp-trial-ok"}}],"edges":[{"from":"t1","to":"s1"}]}`
	tried, failed := h.call(t, "try_rule", `{"event_id":"`+evID+`","rule":{"name":"draft trial",`+echoGraph+`}}`)
	if failed {
		t.Fatalf("try_rule failed: %v", tried)
	}
	tm := tried.(map[string]any)
	if tm["matched"] != true {
		t.Fatalf("try_rule not matched: %+v", tm)
	}
	runObj := tm["run"].(map[string]any)
	steps := runObj["steps"].([]any)
	stdout := steps[0].(map[string]any)["output"].(map[string]any)["stdout"].(string)
	if runObj["status"] != "ok" || !strings.Contains(stdout, "mcp-trial-ok") {
		t.Fatalf("try_rule run = %+v", runObj)
	}
	// Non-matching draft rule returns matched: false
	unmatched, failed := h.call(t, "try_rule", `{"event_id":"`+evID+`","rule":{"name":"nope","graph":{"nodes":[{"id":"t1","kind":"trigger","sources":["other"]},{"id":"s1","kind":"action","instance":"shell","action":"run","params":{"command":"echo no"}}],"edges":[{"from":"t1","to":"s1"}]}}}`)
	if failed || unmatched.(map[string]any)["matched"] != false {
		t.Fatalf("expected unmatched try_rule: %v %v", unmatched, failed)
	}

	// Save rule disabled and replay_event explicitly targeting rule_id
	saved, failed := h.call(t, "save_rule", `{"rule":{"name":"replay target","enabled":false,`+echoGraph+`}}`)
	if failed {
		t.Fatalf("save_rule: %v", saved)
	}
	rid := saved.(map[string]any)["id"].(string)
	replayed, failed := h.call(t, "replay_event", `{"event_id":"`+evID+`","rule_id":"`+rid+`"}`)
	if failed || replayed.(map[string]any)["matched"].(float64) != 1 {
		t.Fatalf("replay_event: %v %v", replayed, failed)
	}

	// try_rule with synthetic mock event + dry_run + mock_outputs (no side effects, no run persisted)
	const dryGraph = `"graph":{"nodes":[{"id":"t1","kind":"trigger","sources":["mock-hook"]},{"id":"c1","kind":"condition","condition":"payload.level == \"high\""},{"id":"s1","kind":"action","instance":"http","action":"request","params":{"url":"https://example.invalid/alert","body":"告警: {{ payload.text }}"}},{"id":"s2","kind":"action","instance":"http","action":"request","params":{"url":"https://example.invalid/next","body":"结果: {{ steps.s1.text }}"}}],"edges":[{"from":"t1","to":"c1"},{"from":"c1","to":"s1"},{"from":"s1","to":"s2"}]}`
	dryOut, failed := h.call(t, "try_rule", `{"dry_run":true,"event":{"source":"mock-hook","type":"webhook.received","payload":{"level":"high","text":"disk 99%"}},"mock_outputs":{"s1":{"text":"ok-mock"}},"rule":{"name":"dry draft",`+dryGraph+`}}`)
	if failed {
		t.Fatalf("try_rule dry_run failed: %v", dryOut)
	}
	dm := dryOut.(map[string]any)
	if dm["matched"] != true {
		t.Fatalf("try_rule dry_run not matched: %+v", dm)
	}
	dryRunObj := dm["run"].(map[string]any)
	if dryRunObj["dry_run"] != true || dryRunObj["status"] != "ok" {
		t.Fatalf("dry_run run = %+v", dryRunObj)
	}
	drySteps := dryRunObj["steps"].([]any)
	if len(drySteps) != 2 || drySteps[0].(map[string]any)["dry_run"] != true {
		t.Fatalf("dry_run steps = %+v", drySteps)
	}
	if drySteps[0].(map[string]any)["params"].(map[string]any)["body"] != "告警: disk 99%" {
		t.Fatalf("step 0 rendered params = %+v", drySteps[0])
	}
	if drySteps[1].(map[string]any)["params"].(map[string]any)["body"] != "结果: ok-mock" {
		t.Fatalf("step 1 rendered params = %+v", drySteps[1])
	}
	afterRuns, _ := h.st.ListRuns("", "", 50)
	for _, r := range afterRuns {
		if r.DryRun || r.RuleName == "dry draft" {
			t.Fatalf("dry_run persisted a run: %+v", r)
		}
	}
	afterEvents, _ := h.st.ListEvents(store.EventFilter{Source: "mock-hook"})
	if len(afterEvents) != 0 {
		t.Fatalf("dry_run persisted mock event: %+v", afterEvents)
	}
}

type diagPlugin struct {
	plugin.Base
	started chan struct{}
	blocked chan struct{}
	host    plugin.Host
}

func (d *diagPlugin) Start(_ context.Context, host plugin.Host) error {
	d.host = host
	host.Log("info", "diag line 1")
	host.Log("warn", "diag line 2")
	host.Log("error", "diag line 3")
	close(d.started)
	return nil
}

func (d *diagPlugin) Invoke(ctx context.Context, action string, _ map[string]any, _ plugin.InvokeContext) (map[string]any, error) {
	if action == "wait" {
		select {
		case d.blocked <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]any{"ok": true}, nil
}

func TestDiagnosticTools(t *testing.T) {
	dp := &diagPlugin{started: make(chan struct{}), blocked: make(chan struct{}, 1)}
	h := newTestServer(t, func(e *engine.Engine) {
		e.RegisterPlugin(plugin.Manifest{
			ID:        "diag",
			Name:      "Diag",
			Category:  plugin.CategoryProcessor,
			Singleton: true,
			Actions:   []plugin.ActionSpec{{Name: "wait", Title: "Wait"}},
		}, func(map[string]any) (plugin.Instance, error) { return dp, nil })
	})
	<-dp.started

	// Emit two events and attach runs for filter testing.
	ev1, err := h.e.Emit(&event.Event{Source: "webhook", Plugin: "webhook", Type: "order.created", Payload: map[string]any{"text": "alpha needle"}})
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := h.e.Emit(&event.Event{Source: "webhook", Plugin: "webhook", Type: "order.failed", Payload: map[string]any{"text": "beta failure"}})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-2 * time.Second)
	t1 := t0.Add(350 * time.Millisecond)
	okRun := &store.Run{
		ID: "run-ok-1", RuleID: "rule-a", RuleName: "Rule A", EventID: ev1, Status: "ok",
		StartedAt: t0, FinishedAt: &t1,
		Steps: []store.StepResult{{ID: "s1", Instance: "http", Action: "request", DurationMS: 350}},
		Trace: []rules.Visit{{Node: "t1"}, {Node: "s1"}},
		Rule:  &rules.Rule{ID: "rule-a", Name: "Rule A"},
	}
	badRun := &store.Run{
		ID: "run-bad-1", RuleID: "rule-b", RuleName: "Rule B", EventID: ev2, Status: "error",
		StartedAt: t0, FinishedAt: &t1,
		Steps: []store.StepResult{{ID: "s1", Instance: "http", Action: "request", Error: "boom", DurationMS: 120}},
		Trace: []rules.Visit{{Node: "t1"}, {Node: "s1", Error: "boom"}},
	}
	if err := h.st.SaveRun(okRun); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SaveRun(badRun); err != nil {
		t.Fatal(err)
	}

	// 1. list_events with q, rule_id, failed
	byQ, failed := h.call(t, "list_events", `{"q":"needle"}`)
	if failed || len(byQ.([]any)) != 1 || byQ.([]any)[0].(map[string]any)["id"] != ev1 {
		t.Fatalf("list_events q: %v failed=%v", byQ, failed)
	}
	byRule, failed := h.call(t, "list_events", `{"rule_id":"rule-b"}`)
	if failed || len(byRule.([]any)) != 1 || byRule.([]any)[0].(map[string]any)["id"] != ev2 {
		t.Fatalf("list_events rule_id: %v failed=%v", byRule, failed)
	}
	byFailed, failed := h.call(t, "list_events", `{"failed":true}`)
	if failed || len(byFailed.([]any)) != 1 || byFailed.([]any)[0].(map[string]any)["id"] != ev2 {
		t.Fatalf("list_events failed: %v failed=%v", byFailed, failed)
	}

	// 2. get_run returns trace, steps, duration_ms and omits Rule snapshot
	gotRun, failed := h.call(t, "get_run", `{"id":"run-ok-1"}`)
	if failed {
		t.Fatalf("get_run failed: %v", gotRun)
	}
	rm := gotRun.(map[string]any)
	if rm["status"] != "ok" || rm["rule"] != nil || rm["duration_ms"].(float64) != 350 {
		t.Fatalf("get_run unexpected: %+v", rm)
	}
	if trace, _ := rm["trace"].([]any); len(trace) != 2 {
		t.Fatalf("get_run trace missing: %+v", rm)
	}
	if _, failed := h.call(t, "get_run", `{"id":"no-such-run"}`); !failed {
		t.Error("get_run on missing id should fail")
	}

	// 3. get_instance_logs validates instance and respects limit (returns newest N)
	logs, failed := h.call(t, "get_instance_logs", `{"instance":"diag","limit":2}`)
	if failed {
		t.Fatalf("get_instance_logs failed: %v", logs)
	}
	ll := logs.([]any)
	if len(ll) != 2 || ll[0].(map[string]any)["message"] != "diag line 2" || ll[1].(map[string]any)["message"] != "diag line 3" {
		t.Fatalf("get_instance_logs unexpected: %+v", ll)
	}
	if _, failed := h.call(t, "get_instance_logs", `{"instance":"missing-inst"}`); !failed {
		t.Error("get_instance_logs on missing instance should fail")
	}

	// 4. cancel_run cancels an in-flight run
	_, err = h.e.SaveRule(rules.Rule{
		Name:    "slow wait",
		Enabled: true,
		Graph: &rules.Graph{
			Nodes: []rules.Node{
				{Step: rules.Step{ID: "t1"}, Kind: rules.KindTrigger, Types: []string{"slow.test"}},
				{Step: rules.Step{ID: "s1", Instance: "diag", Action: "wait"}, Kind: rules.KindAction},
			},
			Edges: []rules.Edge{{From: "t1", To: "s1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	slowEv, err := h.e.Emit(&event.Event{Source: "webhook", Plugin: "webhook", Type: "slow.test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-dp.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("slow run never entered action")
	}
	var runningID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := h.st.ListRuns("", slowEv, 5)
		if len(runs) > 0 && runs[0].Status == "running" {
			runningID = runs[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runningID == "" {
		t.Fatal("could not find running run id")
	}
	cancelled, failed := h.call(t, "cancel_run", `{"id":"`+runningID+`"}`)
	if failed || cancelled.(map[string]any)["cancelled"] != true {
		t.Fatalf("cancel_run failed: %v %v", cancelled, failed)
	}
	// Cancelling a non-existent/already-finished run returns error
	if _, failed := h.call(t, "cancel_run", `{"id":"run-ok-1"}`); !failed {
		t.Error("cancel_run on finished run should fail")
	}
}
