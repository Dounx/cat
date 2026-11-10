package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dounx/cat/core/event"
)

func action(id string) Node {
	return Node{Step: Step{ID: id, Instance: "i", Action: "a"}, Kind: KindAction}
}

func condition(id, src string) Node {
	return Node{Step: Step{ID: id}, Kind: KindCondition, Condition: src}
}

// ran walks the rule for an event and lists the actions run, in order. Each
// action's output is {"ok": true} unless it is named in failing, which also
// ends its branch.
func ran(t *testing.T, r Rule, ev *event.Event, failing ...string) []string {
	t.Helper()
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	outputs := map[string]any{}
	c.Walk(ev, outputs, func(s Step) bool {
		got = append(got, s.ID)
		for _, f := range failing {
			if f == s.ID {
				outputs[s.ID] = map[string]any{}
				return false
			}
		}
		outputs[s.ID] = map[string]any{"ok": true}
		return true
	}, func(id string, err error) { t.Fatalf("condition %s: %v", id, err) }, nil)
	return got
}

func TestGraphBranches(t *testing.T) {
	// trigger → isDeploy ─┬─ yes → notify → log
	//                     │        └─ isProd (after isDeploy) → page
	//                     └─ else → ignore
	// trigger → always (an action straight off the trigger)
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			condition("isDeploy", `payload.text contains "deploy"`),
			condition("isProd", `payload.env == "prod" and steps.notify.ok`),
			action("notify"), action("log"), action("page"), action("ignore"), action("always"),
		},
		Edges: []Edge{
			{From: TriggerNode, To: "isDeploy"},
			{From: "isDeploy", To: "notify"},
			{From: "notify", To: "log"},
			{From: "notify", To: "isProd"},
			{From: "isProd", To: "page"},
			{From: "isDeploy", To: "ignore", Branch: BranchElse},
			{From: TriggerNode, To: "always"},
		},
	}}
	cases := []struct {
		payload map[string]any
		failing []string
		want    []string
	}{
		{map[string]any{"text": "deploy ok", "env": "prod"}, nil, []string{"notify", "log", "page", "always"}},
		{map[string]any{"text": "deploy ok", "env": "dev"}, nil, []string{"notify", "log", "always"}},
		{map[string]any{"text": "hello"}, nil, []string{"ignore", "always"}},
		// a failed action ends its own branch only
		{map[string]any{"text": "deploy ok", "env": "prod"}, []string{"notify"}, []string{"notify", "always"}},
	}
	for _, tc := range cases {
		got := ran(t, r, &event.Event{Payload: tc.payload}, tc.failing...)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("payload %v: ran %v, want %v", tc.payload, got, tc.want)
		}
	}
}

func TestGraphMatch(t *testing.T) {
	// conditions chained before the only action: both must hold to match
	r := Rule{Types: []string{"message.*"}, Graph: &Graph{
		Nodes: []Node{condition("a", `payload.n > 1`), condition("b", `payload.n < 5`), action("do")},
		Edges: []Edge{{From: TriggerNode, To: "a"}, {From: "a", To: "b"}, {From: "b", To: "do"}},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]bool{0: false, 3: true, 9: false} {
		got, err := c.Match(&event.Event{Type: "message.created", Payload: map[string]any{"n": n}})
		if err != nil || got != want {
			t.Errorf("n=%d: match %v (err %v), want %v", n, got, err, want)
		}
	}
	if ok, _ := c.Match(&event.Event{Type: "notice", Payload: map[string]any{"n": 3}}); ok {
		t.Error("an event outside the rule's types must not match")
	}
}

func TestGraphNodeRunsOnce(t *testing.T) {
	// two paths meet at "end": it runs once
	r := Rule{Graph: &Graph{
		Nodes: []Node{action("a"), action("b"), action("end")},
		Edges: []Edge{
			{From: TriggerNode, To: "a"}, {From: TriggerNode, To: "b"},
			{From: "a", To: "end"}, {From: "b", To: "end"},
		},
	}}
	if got, want := ran(t, r, &event.Event{}), []string{"a", "end", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestGraphRejected(t *testing.T) {
	cases := map[string]Graph{
		"循环": {Nodes: []Node{action("a"), action("b")}, Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}}},
		"重复": {Nodes: []Node{action("a"), action("a")}},
		"终点": {Nodes: []Node{action("a")}, Edges: []Edge{{From: TriggerNode, To: "gone"}}},
		"分支": {Nodes: []Node{action("a"), action("b")}, Edges: []Edge{{From: "a", To: "b", Branch: BranchElse}}},
		"类型": {Nodes: []Node{{Step: Step{ID: "x"}, Kind: "loop"}}},
		"条件": {Nodes: []Node{condition("c", `payload.text +`)}},
	}
	for want, g := range cases {
		_, err := Compile(Rule{Graph: &g})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error mentioning %q, got %v", want, err)
		}
	}
}

func TestGraphTriggers(t *testing.T) {
	// Two triggers with their own events and their own way on; "both" is
	// wired from each and runs once however the event came in.
	trigger := func(id, source, typ string) Node {
		return Node{Step: Step{ID: id}, Kind: KindTrigger, Sources: []string{source}, Types: []string{typ}}
	}
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			trigger("chat", "qq", "message.*"), trigger("feed", "rss", "feed.item"),
			action("reply"), action("post"), action("both"),
		},
		Edges: []Edge{
			{From: "chat", To: "reply"}, {From: "feed", To: "post"},
			{From: "chat", To: "both"}, {From: "feed", To: "both"},
		},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source, typ string
		want        []string
	}{
		{"qq", "message.created", []string{"reply", "both"}},
		{"rss", "feed.item", []string{"post", "both"}},
		{"rss", "message.created", nil}, // right type, wrong source for it
		{"tg", "message.created", nil},
	}
	for _, tc := range cases {
		ev := &event.Event{Source: tc.source, Type: tc.typ}
		if got := ran(t, r, ev); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %s: ran %v, want %v", tc.source, tc.typ, got, tc.want)
		}
		if ok, _ := c.Match(ev); ok != (tc.want != nil) {
			t.Errorf("%s %s: match %v", tc.source, tc.typ, ok)
		}
	}
	if _, err := Compile(Rule{Graph: &Graph{
		Nodes: []Node{trigger("a", "x", "y"), action("b")},
		Edges: []Edge{{From: "b", To: "a"}},
	}}); err == nil || !strings.Contains(err.Error(), "触发") {
		t.Errorf("a wire into a trigger should be rejected, got %v", err)
	}
}

func TestGraphThrottle(t *testing.T) {
	// trigger → always
	//         → limit → limited      (the gate decides)
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			{Step: Step{ID: "t"}, Kind: KindTrigger},
			{Step: Step{ID: "limit"}, Kind: KindThrottle, ThrottleSeconds: 60, ThrottleKey: "actor.id"},
			action("limited"), action("always"),
		},
		Edges: []Edge{{From: "t", To: "limit"}, {From: "limit", To: "limited"}, {From: "t", To: "always"}},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	ev := &event.Event{Actor: &event.Actor{ID: "u1"}}
	for _, open := range []bool{true, false} {
		var got, asked []string
		c.Walk(ev, map[string]any{}, func(s Step) bool {
			got = append(got, s.ID)
			return true
		}, nil, func(n *Node, env map[string]any) bool {
			key, _ := Eval(n.ThrottleKey, env)
			asked = append(asked, n.ID+":"+key.(string))
			return open
		})
		want := []string{"always"}
		if open {
			want = []string{"limited", "always"}
		}
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(asked, []string{"limit:u1"}) {
			t.Errorf("gate open=%v: ran %v (want %v), asked %v", open, got, want, asked)
		}
	}
	// held back on its only way to an action, the event does not match at all
	only := Rule{Graph: &Graph{
		Nodes: []Node{{Step: Step{ID: "t"}, Kind: KindTrigger}, {Step: Step{ID: "limit"}, Kind: KindThrottle}, action("a")},
		Edges: []Edge{{From: "t", To: "limit"}, {From: "limit", To: "a"}},
	}}
	oc, _ := Compile(only)
	if ok, _ := oc.MatchPast(ev, func(*Node, map[string]any) bool { return false }); ok {
		t.Error("an event every throttle holds back should not match")
	}
	if ok, _ := oc.Match(ev); !ok {
		t.Error("without a gate a throttle holds nothing back")
	}
	if _, err := Compile(Rule{Graph: &Graph{Nodes: []Node{{Step: Step{ID: "x"}, Kind: KindThrottle, ThrottleKey: "a +"}}}}); err == nil {
		t.Error("a throttle key that does not compile should be rejected")
	}
}
