package mcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/mcp"
	"github.com/dounx/cat/core/store"
)

// An MCP client introduces itself, lists the tools and uses them: it makes a
// rule, sees it listed, changes it without losing where its nodes stand, and
// deletes it.
func TestServer(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := engine.New(st, t.TempDir(), nil)
	for _, b := range builtin.All() {
		e.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	srv := httptest.NewServer(&mcp.Server{Engine: e, Version: "test"})
	defer srv.Close()

	rpc := func(body string) map[string]any {
		t.Helper()
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
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
	// call uses a tool and gives what it said, parsed, and whether it failed
	call := func(name, args string) (any, bool) {
		t.Helper()
		out := rpc(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
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

	init := rpc(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if v := init["result"].(map[string]any)["protocolVersion"]; v != "2024-11-05" {
		t.Errorf("protocol version %v", v)
	}
	if out := rpc(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); out != nil {
		t.Errorf("a notification was answered: %v", out)
	}
	if list := rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)["result"].(map[string]any)["tools"].([]any); len(list) < 8 {
		t.Errorf("tools: %v", list)
	}
	if out := rpc(`{"jsonrpc":"2.0","id":3,"method":"nope"}`); out["error"] == nil {
		t.Errorf("unknown method: %v", out)
	}

	conns, _ := call("list_connections", `{}`)
	if s, _ := json.Marshal(conns); !strings.Contains(string(s), `"id":"http"`) {
		t.Errorf("connections: %s", s)
	}
	if _, failed := call("invoke_action", `{"instance":"nope","action":"x"}`); !failed {
		t.Error("an action on nothing did not fail")
	}
	if _, failed := call("save_rule", `{"rule":{"id":"nope","name":"x","graph":{"nodes":[],"edges":[]}}}`); !failed {
		t.Error("a rule that is not there was changed")
	}
	const graph = `"graph":{"nodes":[{"id":"t1","kind":"trigger"},{"id":"s1","kind":"action","instance":"http","action":"request","params":{"url":"http://127.0.0.1:1/"}}],"edges":[{"from":"t1","to":"s1"}]}`
	made, failed := call("save_rule", `{"rule":{"name":"from an AI","enabled":false,`+graph+`}}`)
	if failed {
		t.Fatalf("save_rule: %v", made)
	}
	id := made.(map[string]any)["id"].(string)
	// the editor places the nodes; an AI that changes the rule leaves them be
	stored, _ := st.ListRules()
	stored[0].Graph.Nodes[1].Meta = map[string]any{"x": 300.0, "y": 40.0}
	st.SaveRule(&stored[0])
	if _, failed := call("save_rule", `{"rule":{"id":"`+id+`","name":"renamed","enabled":false,`+graph+`}}`); failed {
		t.Fatal("changing the rule failed")
	}
	stored, _ = st.ListRules()
	if len(stored) != 1 || stored[0].Name != "renamed" || stored[0].Graph.Nodes[1].Meta["x"] != 300.0 {
		t.Errorf("after the change: %+v", stored)
	}
	if out, failed := call("set_rule_enabled", `{"id":"`+id+`","enabled":true}`); failed || out.(map[string]any)["enabled"] != true {
		t.Errorf("enable: %v", out)
	}
	if list, _ := call("list_rules", `{}`); len(list.([]any)) != 1 || list.([]any)[0].(map[string]any)["meta"] != nil {
		t.Errorf("list_rules: %v", list)
	}
	if _, failed := call("delete_rule", `{"id":"`+id+`"}`); failed {
		t.Error("delete failed")
	}
	if stored, _ = st.ListRules(); len(stored) != 0 {
		t.Errorf("still there: %+v", stored)
	}

	// try_rule & replay_event against a stored event
	evID, err := e.Emit(&event.Event{Source: "webhook", Plugin: "webhook", Type: "webhook.received", Payload: map[string]any{"text": "hello mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	const echoGraph = `"graph":{"nodes":[{"id":"t1","kind":"trigger"},{"id":"s1","kind":"action","instance":"shell","action":"run","params":{"command":"echo mcp-trial-ok"}}],"edges":[{"from":"t1","to":"s1"}]}`
	tried, failed := call("try_rule", `{"event_id":"`+evID+`","rule":{"name":"draft trial",`+echoGraph+`}}`)
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
	unmatched, failed := call("try_rule", `{"event_id":"`+evID+`","rule":{"name":"nope","graph":{"nodes":[{"id":"t1","kind":"trigger","sources":["other"]},{"id":"s1","kind":"action","instance":"shell","action":"run","params":{"command":"echo no"}}],"edges":[{"from":"t1","to":"s1"}]}}}`)
	if failed || unmatched.(map[string]any)["matched"] != false {
		t.Fatalf("expected unmatched try_rule: %v %v", unmatched, failed)
	}

	// Save rule disabled and replay_event explicitly targeting rule_id
	saved, failed := call("save_rule", `{"rule":{"name":"replay target","enabled":false,`+echoGraph+`}}`)
	if failed {
		t.Fatalf("save_rule: %v", saved)
	}
	rid := saved.(map[string]any)["id"].(string)
	replayed, failed := call("replay_event", `{"event_id":"`+evID+`","rule_id":"`+rid+`"}`)
	if failed || replayed.(map[string]any)["matched"].(float64) != 1 {
		t.Fatalf("replay_event: %v %v", replayed, failed)
	}
}
